//go:build linux && (amd64 || arm64)

package screencapture

import (
	"encoding/binary"
	"errors"
)

// PipeWire negotiates formats with SPA pods: values with a header of their
// size and type, in native byte order (little endian on amd64 and arm64),
// padded to 8 bytes. spa/pod/pod.h and spa/param/video/raw.h define them.

const (
	spaTypeID        = 3
	spaTypeInt       = 4
	spaTypeRectangle = 10
	spaTypeFraction  = 11
	spaTypeObject    = 15
	spaTypeChoice    = 19

	spaChoiceNone  = 0
	spaChoiceRange = 1
	spaChoiceEnum  = 3
	spaChoiceFlags = 4

	spaTypeObjectFormat       = 0x40003
	spaTypeObjectParamBuffers = 0x40004

	spaParamEnumFormat = 3
	spaParamFormat     = 4
	spaParamBuffers    = 5

	spaFormatMediaType      = 1
	spaFormatMediaSubtype   = 2
	spaFormatVideoFormat    = 0x20001
	spaFormatVideoSize      = 0x20003
	spaFormatVideoFramerate = 0x20004

	spaMediaTypeVideo  = 2
	spaMediaSubtypeRaw = 1

	spaVideoFormatRGBx = 7
	spaVideoFormatBGRx = 8
	spaVideoFormatRGBA = 11
	spaVideoFormatBGRA = 12

	spaParamBuffersDataType = 6
	spaDataMemPtr           = 1
	spaDataMemFd            = 2
)

var le = binary.LittleEndian

// pod returns a pod of typ with body, padded.
func pod(typ uint32, body []byte) []byte {
	b := le.AppendUint32(nil, uint32(len(body)))
	b = le.AppendUint32(b, typ)
	b = append(b, body...)
	for len(b)%8 != 0 {
		b = append(b, 0)
	}
	return b
}

func u32s(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = le.AppendUint32(b, v)
	}
	return b
}

func podID(v uint32) []byte { return pod(spaTypeID, u32s(v)) }

// podChoice returns a choice of kind among values of childType, each of
// childSize bytes, the first of which is the default.
func podChoice(kind, childType, childSize uint32, values ...[]byte) []byte {
	body := u32s(kind, 0, childSize, childType)
	for _, v := range values {
		body = append(body, v...)
	}
	return pod(spaTypeChoice, body)
}

// podObject returns an object of typ and id with properties, pairs of a
// key and a pod.
func podObject(typ, id uint32, props ...any) []byte {
	body := u32s(typ, id)
	for i := 0; i+1 < len(props); i += 2 {
		body = append(body, u32s(props[i].(uint32), 0)...)
		body = append(body, props[i+1].([]byte)...)
	}
	return pod(spaTypeObject, body)
}

// videoFormats are the formats a capture takes, the cheapest to convert
// first.
var videoFormats = []uint32{spaVideoFormatBGRx, spaVideoFormatBGRA, spaVideoFormatRGBx, spaVideoFormatRGBA}

// enumFormatPod is the formats the stream takes: raw video of 4 bytes a
// pixel, of any size and rate. It asks for no modifier, so that the
// compositor shares memory rather than DMA-BUFs.
func enumFormatPod() []byte {
	formats := [][]byte{u32s(videoFormats[0])}
	for _, f := range videoFormats {
		formats = append(formats, u32s(f))
	}
	return podObject(spaTypeObjectFormat, spaParamEnumFormat,
		uint32(spaFormatMediaType), podID(spaMediaTypeVideo),
		uint32(spaFormatMediaSubtype), podID(spaMediaSubtypeRaw),
		uint32(spaFormatVideoFormat), podChoice(spaChoiceEnum, spaTypeID, 4, formats...),
		uint32(spaFormatVideoSize), podChoice(spaChoiceRange, spaTypeRectangle, 8, u32s(1920, 1080), u32s(1, 1), u32s(16384, 16384)),
		uint32(spaFormatVideoFramerate), podChoice(spaChoiceRange, spaTypeFraction, 8, u32s(0, 1), u32s(0, 1), u32s(1000, 1)),
	)
}

// buffersPod asks for buffers in memory the stream can map.
func buffersPod() []byte {
	types := u32s(1<<spaDataMemPtr | 1<<spaDataMemFd)
	return podObject(spaTypeObjectParamBuffers, spaParamBuffers,
		uint32(spaParamBuffersDataType), podChoice(spaChoiceFlags, spaTypeInt, 4, types, types),
	)
}

// videoFormat is what a format pod settled on.
type videoFormat struct {
	format        uint32
	width, height uint32
}

// parseFormat reads the format and the size of a format pod.
func parseFormat(b []byte) (videoFormat, error) {
	var f videoFormat
	if len(b) < 16 || le.Uint32(b[4:]) != spaTypeObject {
		return f, errors.New("screencapture: not a format")
	}
	size := int(le.Uint32(b))
	if size+8 > len(b) {
		return f, errors.New("screencapture: format cut short")
	}
	body := b[8 : 8+size]
	if le.Uint32(body) != spaTypeObjectFormat {
		return f, errors.New("screencapture: not a format")
	}
	for p := body[8:]; len(p) >= 16; {
		key := le.Uint32(p)
		vsize := int(le.Uint32(p[8:]))
		vtype := le.Uint32(p[12:])
		if 16+vsize > len(p) {
			break
		}
		value := p[16 : 16+vsize]
		if vtype == spaTypeChoice && len(value) >= 16 {
			// The first value is the one chosen.
			n := int(le.Uint32(value[8:]))
			vtype = le.Uint32(value[12:])
			value = value[16:]
			if n <= len(value) {
				value = value[:n]
			}
		}
		switch {
		case key == spaFormatVideoFormat && vtype == spaTypeID && len(value) >= 4:
			f.format = le.Uint32(value)
		case key == spaFormatVideoSize && vtype == spaTypeRectangle && len(value) >= 8:
			f.width, f.height = le.Uint32(value), le.Uint32(value[4:])
		}
		next := 16 + (vsize+7)&^7
		if next > len(p) {
			break
		}
		p = p[next:]
	}
	if f.width == 0 || f.height == 0 {
		return f, errors.New("screencapture: the format has no size")
	}
	return f, nil
}
