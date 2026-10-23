//go:build linux && (amd64 || arm64)

package screencapture

import "testing"

func TestParseFormat(t *testing.T) {
	// A format as a compositor settles it, its size a choice of one.
	pod := podObject(spaTypeObjectFormat, spaParamFormat,
		uint32(spaFormatMediaType), podID(spaMediaTypeVideo),
		uint32(spaFormatMediaSubtype), podID(spaMediaSubtypeRaw),
		uint32(spaFormatVideoFormat), podID(spaVideoFormatRGBx),
		uint32(spaFormatVideoSize), podChoice(spaChoiceNone, spaTypeRectangle, 8, u32s(2560, 1440)),
		uint32(spaFormatVideoFramerate), pod(spaTypeFraction, u32s(0, 1)),
	)
	f, err := parseFormat(pod)
	if err != nil {
		t.Fatal(err)
	}
	if f != (videoFormat{format: spaVideoFormatRGBx, width: 2560, height: 1440}) {
		t.Errorf("parsed %+v", f)
	}
	if _, err := parseFormat(pod[:20]); err == nil {
		t.Error("parsed a format cut short")
	}
}

func TestEnumFormatPod(t *testing.T) {
	// The default of each choice is what parseFormat reads.
	f, err := parseFormat(enumFormatPod())
	if err != nil {
		t.Fatal(err)
	}
	if f != (videoFormat{format: spaVideoFormatBGRx, width: 1920, height: 1080}) {
		t.Errorf("parsed %+v", f)
	}
	if len(enumFormatPod())%8 != 0 || len(buffersPod())%8 != 0 {
		t.Error("pods are not padded")
	}
}
