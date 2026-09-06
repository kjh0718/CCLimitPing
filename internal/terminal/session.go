// Package terminal abstracts launching a child process attached to a
// pseudo-terminal, so provider triggers can drive an interactive CLI (Claude
// Code, Codex) without depending on a specific PTY backend. Unix/macOS uses a
// real PTY (github.com/creack/pty); Windows is not supported yet (a ConPTY
// backend is tracked separately) and returns an error from Start.
package terminal

import (
	"context"
	"io"
)

// Session is a child process running under a pseudo-terminal. Read and Write
// are the PTY master side — Read drains the child's output, Write feeds its
// input. Close tears the PTY down, Wait blocks until the child exits, and Kill
// force-terminates it. The abstraction owns the whole process lifecycle (not
// just the I/O stream) because a PTY backend may spawn the child itself rather
// than through os/exec, so Wait and Kill cannot be assumed to come from an
// externally held *exec.Cmd.
type Session interface {
	io.Reader
	io.Writer
	io.Closer
	// Wait blocks until the child exits, returning its exit error (nil on a
	// clean exit), matching (*exec.Cmd).Wait semantics.
	Wait() error
	// Kill force-terminates the child. It is safe to call when the process is
	// already gone.
	Kill() error
}

// Start launches name (with args) attached to a new pseudo-terminal and returns
// a Session that owns the child's lifecycle. Cancelling ctx terminates the
// child. On platforms without PTY support it returns an error and a nil Session.
func Start(ctx context.Context, name string, args []string) (Session, error) {
	return start(ctx, name, args)
}
