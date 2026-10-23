//go:build !darwin && !(linux && (amd64 || arm64)) && !(windows && (amd64 || arm64))

package screencapture

import (
	"context"
	"errors"
	"image"
)

func windows(context.Context) ([]Window, error) { return nil, errors.ErrUnsupported }

func access() bool        { return false }
func requestAccess() bool { return false }

func screenshot(context.Context, Source, Options) (*image.RGBA, bool, error) {
	return nil, true, errors.ErrUnsupported
}

func start(context.Context, Source, Options, *Recording) (func(), error) {
	return nil, errors.ErrUnsupported
}
