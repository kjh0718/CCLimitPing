//go:build windows

package terminal

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsSession runs the child under a ConPTY pseudoconsole. Read and Write
// are the parent ends of the pipes wired to that console, and the child, plus
// anything it spawns, lives in a job object so the whole tree can be reaped.
type windowsSession struct {
	pty *conPTY
	job *jobObject

	// processDone is closed once the child has exited and waitErr holds its
	// result. The real wait runs exactly once, in a background waiter started by
	// start, so the process handle is released even if nobody ever calls Wait.
	processDone chan struct{}
	waitErr     error

	closeOnce sync.Once
}

func start(ctx context.Context, name string, args []string) (Session, error) {
	// exec.Command is used only to resolve the executable and assemble argv the
	// way os/exec would; the process itself has to be created by hand, because
	// attaching a pseudoconsole needs an extended startup info block that
	// (*exec.Cmd).Start does not expose.
	cmd := exec.Command(name, args...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}

	pty, err := newConPTY(conPTYSize)
	if err != nil {
		return nil, err
	}
	job, err := newJobObject()
	if err != nil {
		pty.teardown()
		return nil, err
	}
	proc, err := startAttached(cmd, pty.hpc, job)
	if err != nil {
		// Closing the job kills anything that did get assigned to it.
		_ = job.close()
		pty.teardown()
		return nil, err
	}

	s := &windowsSession{pty: pty, job: job, processDone: make(chan struct{})}
	go s.wait(proc)
	go s.watch(ctx)
	return s, nil
}

// startAttached creates the child attached to the pseudoconsole and returns its
// process handle, which the caller's waiter owns from then on.
//
// The startup info is an extended block whose Cb covers the whole STARTUPINFOEX
// (not just the STARTUPINFO inside it), carrying the pseudoconsole attribute,
// with EXTENDED_STARTUPINFO_PRESENT set so the attribute list is read at all.
//
// It also declares STARTF_USESTDHANDLES with all three handles left NULL, which
// is what actually attaches the child's stdio to the pseudoconsole rather than
// to ours. Without the flag, CreateProcess duplicates this process's standard
// handles into the child (it only skips that for CREATE_NEW_CONSOLE,
// CREATE_NO_WINDOW and DETACHED_PROCESS), and the console attach path replaces
// a standard handle "if the existing value is NULL, or ... looks like a console
// pseudohandle" — so inherited *console* handles get re-pointed at the
// pseudoconsole, while inherited pipes and files survive and the child renders
// into them instead. Every way this package is actually used has non-console
// standard handles: `bg`/`watch` run detached with their output on a log file,
// and the tests run under `go test`, which collects output through a pipe. The
// symptom is stark — the pseudoconsole emits its opening handshake and nothing
// more, while the child's output turns up on the parent's stdout.
//
// NULL handles rather than the pipe ends are what the console documentation
// asks for here (a console process started with no standard handles has them
// "filled automatically with appropriate handles to a new console", which for
// this child is the pseudoconsole), and it is what the Windows console
// maintainers recommend for a ConPTY parent whose own output is redirected. It
// is also why handle inheritance stays off: nothing is being handed down, and
// the pseudoconsole duplicates the pipe ends it needs by itself.
func startAttached(cmd *exec.Cmd, hpc windows.Handle, job *jobObject) (windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, fmt.Errorf("NewProcThreadAttributeList: %w", err)
	}
	defer attrs.Delete()

	// The attribute value is the HPCON itself, not a pointer to it — an HPCON is
	// already a pointer-sized handle. Spelling that as unsafe.Pointer(hpc) would
	// trip go vet's unsafeptr check (a uintptr-based type converted straight to
	// unsafe.Pointer), so reinterpret the handle's bits through its address:
	// &hpc is a real pointer, so none of these conversions is the flagged cast,
	// and the resulting value is identical.
	if err := attrs.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)),
		unsafe.Sizeof(hpc),
	); err != nil {
		return 0, fmt.Errorf("UpdateProcThreadAttribute: %w", err)
	}

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.ProcThreadAttributeList = attrs.List()
	si.Flags |= windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = 0, 0, 0

	appName, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return 0, err
	}
	// ComposeCommandLine applies the quoting rules the Windows CRT parses back,
	// so an argument containing spaces or quotes survives the round trip.
	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		return 0, err
	}

	// A nil environment block inherits this process's environment, which is what
	// the providers need (PATH, the CLI's own credentials in the user profile).
	// CREATE_UNICODE_ENVIRONMENT documents that inherited block as Unicode.
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		appName,
		cmdLine,
		nil,   // process security attributes
		nil,   // thread security attributes
		false, // no handle inheritance; the pseudoconsole dups what it needs
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_SUSPENDED,
		nil, // environment: inherited
		nil, // working directory: inherited
		&si.StartupInfo,
		&pi,
	); err != nil {
		return 0, fmt.Errorf("CreateProcess: %w", err)
	}

	// Created suspended so the job assignment lands before the child runs its
	// first instruction; only then is every descendant it spawns in the job too.
	if err := job.assign(pi.Process); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		return 0, err
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		_ = windows.TerminateProcess(pi.Process, 1)
		_ = windows.CloseHandle(pi.Thread)
		_ = windows.CloseHandle(pi.Process)
		return 0, fmt.Errorf("ResumeThread: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	return pi.Process, nil
}

// wait is the single waiter for the child. It records the exit status, starts
// the pseudoconsole close so a blocked Read ends up at EOF, and only then
// publishes the result — the close is started rather than awaited because
// ClosePseudoConsole may block long past the child's exit (see closeHPCON).
func (s *windowsSession) wait(proc windows.Handle) {
	defer close(s.processDone)
	s.waitErr = waitProcess(proc)
	s.pty.closeHPCON()
}

// waitProcess blocks until the process exits and maps its status onto
// (*exec.Cmd).Wait semantics: nil for a clean exit, an error otherwise. It owns
// the handle and releases it before returning.
func waitProcess(proc windows.Handle) error {
	defer func() { _ = windows.CloseHandle(proc) }()

	event, err := windows.WaitForSingleObject(proc, windows.INFINITE)
	if err != nil {
		return fmt.Errorf("WaitForSingleObject: %w", err)
	}
	if event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("waiting for child returned unexpected state %#x", event)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(proc, &code); err != nil {
		return fmt.Errorf("GetExitCodeProcess: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("exit status %d", code)
	}
	return nil
}

// watch binds ctx cancellation to the child, mirroring what exec.CommandContext
// does on Unix. It ends with the child, so it cannot outlive the session.
func (s *windowsSession) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = s.Kill()
	case <-s.processDone:
	}
}

func (s *windowsSession) Read(p []byte) (int, error)  { return s.pty.out.Read(p) }
func (s *windowsSession) Write(p []byte) (int, error) { return s.pty.in.Write(p) }

func (s *windowsSession) Wait() error {
	<-s.processDone
	return s.waitErr
}

func (s *windowsSession) Kill() error { return s.job.terminate() }

// Close tears the session down, once, in the order conPTY.shutdown documents:
// the pipe ends go first, the pseudoconsole close runs in the background, and
// the process tree is killed without waiting for it. Killing the tree is what
// unblocks a Read parked on the output pipe, so Close is safe to call while the
// providers are still draining the session.
func (s *windowsSession) Close() error {
	s.closeOnce.Do(func() {
		s.pty.shutdown(func() {
			_ = s.job.terminate()
			_ = s.job.close()
		})
	})
	return nil
}
