//go:build windows

package terminal

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by Start on Windows until the ConPTY backend lands.
// The interactive provider triggers require a real pseudo-terminal to anchor the
// subscription window, which github.com/creack/pty does not provide on Windows.
var ErrUnsupported = errors.New("interactive PTY sessions are not supported on Windows yet")

func start(_ context.Context, _ string, _ []string) (Session, error) {
	return nil, ErrUnsupported
}
