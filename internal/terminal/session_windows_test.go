//go:build windows

package terminal

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestStartUnsupportedOnWindows verifies the current Windows behavior: Start
// reports ErrUnsupported, returns a nil Session, and does so promptly (no hang).
// The ConPTY backend will replace this stub; until then this pins the contract.
func TestStartUnsupportedOnWindows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type result struct {
		sess Session
		err  error
	}
	done := make(chan result, 1)
	go func() {
		// A harmless command line; it must never actually run on the stub path.
		sess, err := Start(ctx, "cmd", []string{"/c", "echo hi"})
		done <- result{sess: sess, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			if r.sess != nil {
				_ = r.sess.Close()
			}
			t.Fatal("Start returned a nil error on Windows; expected unsupported")
		}
		if !errors.Is(r.err, ErrUnsupported) {
			t.Fatalf("Start error = %v, want ErrUnsupported", r.err)
		}
		if r.sess != nil {
			t.Fatalf("Start returned a non-nil Session on Windows: %v", r.sess)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return promptly on Windows (possible hang)")
	}
}
