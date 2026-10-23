//go:build !darwin && !(windows && (amd64 || arm64))

package speech

import "context"

// Linux has no speech recognizer of the system; in browsers, pages have
// the Web Speech API of their own.

func platformSupported() bool { return false }

func platformAuthorize(context.Context) error { return ErrUnsupported }

func platformStart(context.Context, Options, func(string, bool), func(error)) (session, error) {
	return nil, ErrUnsupported
}
