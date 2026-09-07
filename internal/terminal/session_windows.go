//go:build windows

package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	app, line, err := prepareWindowsCommand(cmd)
	if err != nil {
		return 0, err
	}
	appName, err := windows.UTF16PtrFromString(app)
	if err != nil {
		return 0, err
	}
	cmdLine, err := windows.UTF16PtrFromString(line)
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

// prepareWindowsCommand decides how CreateProcess should launch cmd: what to
// pass as lpApplicationName, and the command line to go with it.
//
// A native image is launched directly, with the arguments quoted the way the
// Windows CRT parses them back. A batch file is not an image, and CreateProcess
// documents the only supported way to run one: set lpApplicationName to the
// command interpreter and pass it /c plus the batch file. Some Windows builds do
// run a batch file handed straight to CreateProcess, which is why this went
// unnoticed, but nothing promises that and it is the wrong shape regardless —
// cmd.exe re-parses the command line under rules the CRT quoting does not
// account for, so an argument containing cmd metacharacters is not carried
// safely. Going through the interpreter explicitly makes the launch documented
// and lets the arguments be quoted for the parse that actually happens.
//
// This matters here because the npm-installed CLIs this package drives are .cmd
// shims on Windows, so the batch path is the common one, not the exotic one.
func prepareWindowsCommand(cmd *exec.Cmd) (appName, cmdLine string, err error) {
	switch strings.ToLower(filepath.Ext(cmd.Path)) {
	case ".cmd", ".bat":
		return batchCommand(cmd.Path, cmd.Args[1:])
	default:
		return cmd.Path, windows.ComposeCommandLine(cmd.Args), nil
	}
}

// batchCommand builds the interpreter invocation that runs a .cmd/.bat script.
//
// The command line is `<comspec> /d /v:off /s /c "<script> <args...>"`:
//   - /d skips any AutoRun command the machine has configured, so the session
//     starts from a predictable shell,
//   - /v:off turns off delayed expansion, so !NAME! stays literal no matter what
//     the registry's DelayedExpansion default or an inherited setting says,
//   - /c runs the command and exits,
//   - /s settles how the quotes are read: with /s, cmd strips the first and last
//     quote of everything after /c and takes the rest as-is. That is what the one
//     extra wrapping pair is for — it is consumed by that rule, leaving the
//     script and each argument still carrying their own quotes.
//
// Composing the whole line with ComposeCommandLine instead would break both
// halves of that: it escapes an embedded quote as \" — a backslash cmd does not
// honour, so the quote toggles and the rest of the argument becomes live command
// text — and it leaves no wrapping pair, so the /s rule eats the quotes around a
// script path that has a space in it and cmd tries to run the first word alone.
func batchCommand(script string, args []string) (appName, cmdLine string, err error) {
	inner, err := quoteBatchArg(script)
	if err != nil {
		return "", "", fmt.Errorf("batch script path %q: %w", script, err)
	}
	for i, a := range args {
		q, err := quoteBatchArg(a)
		if err != nil {
			// The argument's value is deliberately left out of the message: what
			// arrives here is the configured prompt and extra args, which are the
			// user's own text. The position is enough to find it in the config.
			return "", "", fmt.Errorf("argument %d of %d passed to batch file %q: %w", i+1, len(args), script, err)
		}
		inner += " " + q
	}

	comspec := commandInterpreter()
	prefix := windows.ComposeCommandLine([]string{comspec, "/d", "/v:off", "/s", "/c"})
	return comspec, prefix + ` "` + inner + `"`, nil
}

// commandInterpreter returns the command processor to run batch files with. It
// is COMSPEC, as documented, falling back to the system cmd.exe by absolute path
// so that an empty or missing COMSPEC cannot turn into a bare-name lookup that
// resolves against the current directory or PATH.
func commandInterpreter() string {
	if comspec := os.Getenv("COMSPEC"); comspec != "" {
		return comspec
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "cmd.exe")
}

// quoteBatchArg quotes one token of the command cmd.exe parses once /s has taken
// the wrapping quotes off. The token is wrapped in quotes and any quote inside it
// is doubled, which is the escape cmd understands (and, for the npm shims that
// forward %* to a CRT program, the escape that program parses back too).
//
// The wrapping is what contains cmd's own metacharacters: inside quotes & | < >
// ( ) and ^ are ordinary text, so an argument cannot end the command it belongs
// to and start another one.
//
// Two things cannot be carried across at all, and are refused rather than
// delivered as something other than what was asked for:
//
//   - A NUL or a newline, which a command line has no way to represent; passing
//     one on would silently truncate the argument at that byte.
//
//   - A percent sign. cmd expands %NAME% while it parses, before the script ever
//     runs, and a bare % has no escape inside a quoted command-line token: ^%
//     only works outside quotes and %% only inside a script file. /v:off does not
//     help — that governs !NAME! and nothing else. So a literal % cannot reach
//     the script, and the alternative to refusing is handing the script a value
//     silently replaced by an environment variable's contents, which is both a
//     lost argument and, depending on what that variable holds, more shell syntax
//     arriving where an argument was meant.
//
// Neither limit applies to a native executable: its arguments never pass through
// cmd, so they go through ComposeCommandLine untouched.
func quoteBatchArg(arg string) (string, error) {
	if strings.ContainsAny(arg, "\x00\r\n") {
		return "", errors.New("cannot be passed to a batch file: it contains a NUL or a newline, which a command line cannot represent")
	}
	if strings.Contains(arg, "%") {
		return "", errors.New("cannot be passed to a batch file: it contains %, which cmd.exe expands as an environment variable reference before the script runs")
	}
	var b strings.Builder
	b.Grow(len(arg) + 2)
	b.WriteByte('"')
	for i := 0; i < len(arg); i++ {
		if arg[i] == '"' {
			b.WriteString(`""`)
			continue
		}
		b.WriteByte(arg[i])
	}
	b.WriteByte('"')
	return b.String(), nil
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
