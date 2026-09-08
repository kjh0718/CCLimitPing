//go:build !windows

package terminal

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStartRunsCommandAndReadsOutput starts a trivial shell command under a real
// PTY, reads its output through the Session, and confirms Wait returns cleanly.
// It uses `sh -c echo` (always present on Unix), never a real CLI or credentials.
func TestStartRunsCommandAndReadsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const marker = "pty-test-output"
	sess, err := Start(ctx, "sh", []string{"-c", "echo " + marker})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Close the PTY session exactly once. The guard keeps the in-body close
	// (which unblocks the reader) and the cleanup safety net mutually
	// exclusive, so the session is never closed twice; the close error is
	// reported at the in-body call rather than dropped.
	closed := false
	closeSession := func() error {
		if closed {
			return nil
		}
		closed = true
		return sess.Close()
	}
	t.Cleanup(func() {
		if err := closeSession(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Drain the PTY master concurrently. Reading a master after the child exits
	// can return EIO on Linux (vs a clean EOF on macOS), so the read error is
	// intentionally ignored — the assertion is on the captured bytes.
	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		b := make([]byte, 1024)
		for {
			n, rerr := sess.Read(b)
			if n > 0 {
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("Wait returned an error for a clean exit: %v", err)
		}
	case <-ctx.Done():
		_ = sess.Kill()
		t.Fatalf("timed out waiting for the command: %v", ctx.Err())
	}

	// Closing the master unblocks the reader if the child's exit didn't.
	if err := closeSession(); err != nil {
		t.Errorf("Close: %v", err)
	}
	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not finish after Close (possible hang)")
	}

	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if !strings.Contains(got, marker) {
		t.Fatalf("PTY output %q does not contain %q", got, marker)
	}
}

// TestKillTerminatesCommand confirms Kill stops a long-running child and that
// Wait then returns, all within the context deadline (no hang, no leaked child).
func TestKillTerminatesCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := Start(ctx, "sh", []string{"-c", "sleep 60"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Single, error-checked close (no second Close call anywhere in this test).
	defer func() {
		if err := sess.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// Keep the master drained so the child never blocks on a full PTY buffer.
	go func() {
		b := make([]byte, 512)
		for {
			if _, rerr := sess.Read(b); rerr != nil {
				return
			}
		}
	}()

	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	select {
	case <-waitErr:
		// A killed process makes Wait return a non-nil error; either way it
		// must return, which is what this test asserts.
	case <-ctx.Done():
		t.Fatalf("Wait did not return after Kill (possible hang): %v", ctx.Err())
	}
}

// TestKillAfterWaitSucceeds pins the Session contract's promise that Kill is
// safe once the child is already gone. After Wait reaps it, the underlying
// Process.Kill reports os.ErrProcessDone, which must not surface as a failure —
// the Windows backend returns nil in the same situation (TestSessionKill), and
// the two backends have to stay interchangeable for the provider triggers.
func TestKillAfterWaitSucceeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := Start(ctx, "sh", []string{"-c", "exit 0"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Single, error-checked close (no second Close call anywhere in this test).
	defer func() {
		if err := sess.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// Keep the master drained so the child never blocks on a full PTY buffer.
	go func() {
		b := make([]byte, 512)
		for {
			if _, rerr := sess.Read(b); rerr != nil {
				return
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("Wait returned an error for a clean exit: %v", err)
		}
	case <-ctx.Done():
		_ = sess.Kill()
		t.Fatalf("Wait did not return for a command that exits immediately: %v", ctx.Err())
	}

	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill after Wait = %v, want nil for an already-exited child", err)
	}
}
