//go:build !windows

package terminal

import (
	"context"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// unixSession wraps a creack/pty master file and the exec.Cmd running under it.
// This preserves the exact behavior the provider triggers relied on before the
// terminal seam was introduced: exec.CommandContext binds ctx cancellation to
// the child, pty.Start attaches the PTY, and the master file is the child's
// stdin/stdout.
type unixSession struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func start(ctx context.Context, name string, args []string) (Session, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, err
	}
	return &unixSession{cmd: cmd, ptmx: ptmx}, nil
}

func (s *unixSession) Read(p []byte) (int, error)  { return s.ptmx.Read(p) }
func (s *unixSession) Write(p []byte) (int, error) { return s.ptmx.Write(p) }
func (s *unixSession) Close() error                { return s.ptmx.Close() }
func (s *unixSession) Wait() error                 { return s.cmd.Wait() }

func (s *unixSession) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Kill()
}
