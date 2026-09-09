//go:build windows

package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conPTYSize is the size every pseudoconsole is created with. The interactive
// CLIs only need a plausible terminal to lay their TUI out in, and nothing here
// resizes, so this is fixed rather than tracked against a real console: the ping
// paths run detached, where there is no console to track.
var conPTYSize = windows.Coord{X: 120, Y: 30}

const (
	// conhostExitTimeout bounds how long teardown waits for the console host to
	// run itself down after its clients are gone, before reaping it.
	conhostExitTimeout = time.Second
	// hpconCloseTimeout bounds how long teardown waits for the background
	// ClosePseudoConsole to return before leaving it to finish on its own.
	hpconCloseTimeout = 2 * time.Second
)

// pipeHandle owns one end of an anonymous pipe and serializes closing the
// handle against the I/O in flight on it.
//
// A blocked ReadFile is unblocked by the pipe breaking — the ConPTY side goes
// away once the pseudoconsole is closed and the console host exits — and not by
// closing the handle underneath the reader: Win32 does not define closing a
// handle that a synchronous ReadFile is parked on, and the freed handle value
// can be reused by the next CreatePipe in this process, which would leave a
// still-live read pointed at an unrelated pipe. So Close only marks the end
// closed and leaves the actual CloseHandle to whichever side finishes last.
type pipeHandle struct {
	mu     sync.Mutex
	h      windows.Handle
	busy   int
	closed bool
	freed  bool
}

func newPipeHandle(h windows.Handle) *pipeHandle { return &pipeHandle{h: h} }

// acquire pins the handle for one syscall, or reports that the end is closed.
func (p *pipeHandle) acquire() (windows.Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, os.ErrClosed
	}
	p.busy++
	return p.h, nil
}

func (p *pipeHandle) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy--
	p.freeLocked()
}

// freeLocked closes the handle once the end is closed and no syscall is holding
// it. Callers hold p.mu.
func (p *pipeHandle) freeLocked() {
	if p.freed || !p.closed || p.busy > 0 {
		return
	}
	p.freed = true
	_ = windows.CloseHandle(p.h)
}

// Close marks the end closed. It is idempotent and never blocks on in-flight
// I/O; see the type comment for who runs the CloseHandle.
func (p *pipeHandle) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.freeLocked()
	return nil
}

// Read reads from the pipe with a raw ReadFile rather than wrapping the handle
// in an *os.File: os.NewFile hands the handle to the runtime poller, which does
// not fit a synchronous anonymous pipe, and reads past the pseudoconsole's
// opening handshake then stall indefinitely even while the child keeps writing.
// Owning the syscall also lets the pipe's end states map onto io.EOF exactly.
func (p *pipeHandle) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	h, err := p.acquire()
	if err != nil {
		// Reading a torn-down session is end of input, not a failure: the
		// providers drain the session with io.Copy while closing it elsewhere.
		return 0, io.EOF
	}
	defer p.release()

	var n uint32
	if err := windows.ReadFile(h, b, &n, nil); err != nil {
		if isPipeEOF(err) {
			return int(n), io.EOF
		}
		return int(n), err
	}
	if n == 0 {
		// A zero-length read on a byte pipe only happens at end of input.
		return 0, io.EOF
	}
	return int(n), nil
}

// Write feeds the child's stdin, looping over short writes as io.Writer wants.
func (p *pipeHandle) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	h, err := p.acquire()
	if err != nil {
		return 0, err
	}
	defer p.release()

	total := 0
	for total < len(b) {
		var n uint32
		if err := windows.WriteFile(h, b[total:], &n, nil); err != nil {
			if isPipeEOF(err) {
				return total + int(n), io.ErrClosedPipe
			}
			return total + int(n), err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
		total += int(n)
	}
	return total, nil
}

// isPipeEOF reports whether err means the far end of the pipe is gone, which is
// the normal way a ConPTY session ends: the console host closes its side when
// the pseudoconsole is closed after the child exits.
//
// The comparisons go through errors.Is rather than testing err directly. The
// callers hand over what ReadFile and WriteFile returned, unwrapped, so today
// the two are equivalent — but a syscall error that picks up any context on its
// way here would otherwise stop being recognised, and silently turn the end of a
// session into a read failure.
func isPipeEOF(err error) bool {
	switch {
	case errors.Is(err, windows.ERROR_BROKEN_PIPE),
		errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED),
		errors.Is(err, windows.ERROR_HANDLE_EOF):
		return true
	case errors.Is(err, windows.ERROR_OPERATION_ABORTED),
		errors.Is(err, windows.ERROR_INVALID_HANDLE):
		// The session was torn down underneath the call.
		return true
	}
	return false
}

// conPTY is a pseudoconsole plus the parent-side ends of the pipes wired to it:
// in feeds the child's stdin, out drains everything the console renders.
type conPTY struct {
	hpc windows.Handle
	// conhost is a handle to the console host serving this pseudoconsole, or 0
	// when it could not be identified; see openNewConhostChild.
	conhost windows.Handle

	in  *pipeHandle
	out *pipeHandle

	hpcOnce sync.Once
	hpcDone chan struct{}
}

// newConPTY creates a pseudoconsole of the given size and returns the parent
// side of it.
func newConPTY(size windows.Coord) (*conPTY, error) {
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("CreatePipe (stdin): %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, fmt.Errorf("CreatePipe (stdout): %w", err)
	}

	// CreatePseudoConsole also spawns the conhost.exe serving the console
	// session, as a direct child of this process. Teardown wants a handle to it
	// (see reapConhost) but Windows offers no way to derive one from the HPCON,
	// so identify it by diffing our conhost children across the call, under a
	// lock so that two sessions starting at once cannot make each other's diff
	// ambiguous. The handle is opened right away, before the pid can be reused.
	var hpc, conhost windows.Handle
	conhostScanMu.Lock()
	before := conhostChildren()
	err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc)
	if err == nil {
		conhost = openNewConhostChild(before)
	}
	conhostScanMu.Unlock()

	// The pseudoconsole duplicates the child-side ends it needs, so drop ours
	// either way: holding the output pipe's write end open here would keep the
	// reader from ever seeing EOF.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, fmt.Errorf("CreatePseudoConsole: %w", err)
	}

	return &conPTY{
		hpc:     hpc,
		conhost: conhost,
		in:      newPipeHandle(inWrite),
		out:     newPipeHandle(outRead),
		hpcDone: make(chan struct{}),
	}, nil
}

// closeHPCON starts closing the pseudoconsole, exactly once, on its own
// goroutine, and returns a channel that is closed when that call returns.
//
// It must run off the caller's thread because ClosePseudoConsole can block for
// a long time: before Windows 11 24H2 it waits for the console host to exit,
// and closing only delivers CTRL_CLOSE_EVENT to the attached client rather than
// terminating it, so a client that keeps running keeps the host — and with it
// ClosePseudoConsole — alive arbitrarily long. Sequencing the job kill after a
// blocking close would deadlock in exactly the case the kill exists for, so
// teardown starts this and kills in parallel.
//
// Closing is also what ends the output stream: unlike a Unix master fd, which
// EOFs when the slave side closes on child exit, the ConPTY output pipe stays
// alive until the pseudoconsole is closed. The waiter therefore calls this as
// soon as the child exits, so a blocked Read sees EOF once the buffered output
// has drained.
func (c *conPTY) closeHPCON() <-chan struct{} {
	c.hpcOnce.Do(func() {
		go func() {
			defer close(c.hpcDone)
			windows.ClosePseudoConsole(c.hpc)
		}()
	})
	return c.hpcDone
}

// reapConhost waits briefly for the console host to run itself down once its
// clients are gone, and terminates it if it does not. A healthy conhost exits by
// itself after the pseudoconsole is closed and its clients are gone, leaving
// this as just the handle close; conhost builds before Windows 11 24H2 can fail
// to complete that rundown when a client was killed after the close event was
// delivered — the fate of every client the job kill is for — and such a host
// then sits around forever serving nothing.
func (c *conPTY) reapConhost() {
	if c.conhost == 0 {
		return
	}
	event, err := windows.WaitForSingleObject(c.conhost, uint32(conhostExitTimeout/time.Millisecond))
	if err != nil || event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(c.conhost, 1)
	}
	_ = windows.CloseHandle(c.conhost)
	c.conhost = 0
}

// shutdown releases everything the pseudoconsole owns, running kill (when the
// caller has a child to reap) after the pseudoconsole close has been started but
// before anything waits on it: on builds where ClosePseudoConsole blocks until
// the console host exits, the host stays alive as long as a surviving client
// does, and that client only goes away through the kill — so ordering the kill
// after the close would deadlock in exactly the case the kill exists for.
//
// The pipe ends go first so that the pseudoconsole close, which flushes the
// client's pending output into the output pipe, cannot wedge on a pipe nobody is
// draining. A Read blocked at this moment is still draining and keeps its handle
// alive until it returns, which it does as soon as the far end goes away.
func (c *conPTY) shutdown(kill func()) {
	_ = c.in.Close()
	_ = c.out.Close()
	done := c.closeHPCON()

	if kill != nil {
		kill()
	}
	c.reapConhost()

	select {
	case <-done:
	case <-time.After(hpconCloseTimeout):
		// Left to finish on its own. It only touches the pseudoconsole's own
		// handles from here; both pipe ends are already released above.
	}
}

// teardown releases the pseudoconsole on the start error paths, where no child
// exists yet and so there is nothing to kill.
func (c *conPTY) teardown() { c.shutdown(nil) }

// conhostScanMu serializes CreatePseudoConsole and the process scans around it,
// so concurrent starts cannot confuse each other's "which conhost is new" diff.
var conhostScanMu sync.Mutex

// conhostChildren returns the pids of the conhost.exe processes that are direct
// children of this process. Errors just yield a smaller set: identifying the
// console host is best effort and never fails a start.
func conhostChildren() map[uint32]bool {
	pids := map[uint32]bool{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return pids
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	me := uint32(os.Getpid())
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ParentProcessID == me && strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), "conhost.exe") {
			pids[pe.ProcessID] = true
		}
	}
	return pids
}

// openNewConhostChild returns a handle to the single conhost child that appeared
// since the before scan, or 0 if there is not exactly one candidate or it cannot
// be opened.
func openNewConhostChild(before map[uint32]bool) windows.Handle {
	var found []uint32
	for pid := range conhostChildren() {
		if !before[pid] {
			found = append(found, pid)
		}
	}
	if len(found) != 1 {
		return 0
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE, false, found[0])
	if err != nil {
		return 0
	}
	return h
}
