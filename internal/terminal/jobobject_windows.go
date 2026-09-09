//go:build windows

package terminal

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobObject holds the ConPTY child and every process it goes on to spawn, so
// the session can terminate the whole tree at once. Closing the pseudoconsole
// only delivers CTRL_CLOSE_EVENT to the clients attached at that moment, which
// misses a child still starting up and any grandchild spawned while the console
// is going down, and clients are free to ignore it — the job kill is what
// actually reaps all of those.
//
// The handle is guarded because Kill (from the caller, or from the context
// watcher) and Close race for it: terminating through a handle that Close has
// already freed would either fail or, once the handle value is reused, act on an
// unrelated object.
type jobObject struct {
	mu     sync.Mutex
	h      windows.Handle
	closed bool
}

// newJobObject creates a job that kills its members when the last handle to it
// goes away. That doubles as a safety net: if the process exits without running
// a teardown, the handle is closed for us and the tree is still reaped.
func newJobObject() (*jobObject, error) {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}
	return &jobObject{h: h}, nil
}

// assign puts a process into the job. The child is created suspended so that
// this lands before it runs its first instruction, which is what guarantees
// every descendant it ever spawns is in the job from the start.
func (j *jobObject) assign(process windows.Handle) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return fmt.Errorf("AssignProcessToJobObject: job already closed")
	}
	if err := windows.AssignProcessToJobObject(j.h, process); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// terminate kills every process still in the job. It is safe to call when the
// tree is already gone (an empty job terminates successfully) and after close,
// which reports nothing left to do rather than touching a freed handle.
func (j *jobObject) terminate() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	if err := windows.TerminateJobObject(j.h, 1); err != nil {
		return fmt.Errorf("TerminateJobObject: %w", err)
	}
	return nil
}

// close releases the job handle, which also kills anything still in it.
func (j *jobObject) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return windows.CloseHandle(j.h)
}
