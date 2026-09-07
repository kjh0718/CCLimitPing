//go:build windows

package terminal

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Every test here drives a real child process, so every wait is bounded: a
// regression in the teardown ordering shows up as a deadlock, and a deadlocked
// test must fail rather than hang until the package timeout.
const (
	waitTimeout   = 15 * time.Second
	outputTimeout = 15 * time.Second
)

// cmdExe is the child used throughout. It is a console application that reads
// its input from the console (which is what makes the stdin round trip a real
// test of the pseudoconsole rather than of a pipe), and it is present on every
// Windows install. The real CLIs are deliberately never run from tests: they
// would spend the account's quota.
const cmdExe = "cmd.exe"

// interactiveArgs keeps a cmd.exe session alive reading from the console. /Q
// turns off command echo so the output is mostly what commands actually print,
// and /D skips any AutoRun command the machine may have configured.
var interactiveArgs = []string{"/Q", "/D", "/K"}

func startSession(t *testing.T, ctx context.Context, name string, args ...string) Session {
	t.Helper()
	sess, err := Start(ctx, name, args)
	if err != nil {
		t.Fatalf("Start(%s %v) = %v", name, args, err)
	}
	return sess
}

// output collects everything a session renders, on its own goroutine, the way
// the providers drain a session with io.Copy.
type output struct {
	mu   sync.Mutex
	buf  []byte
	err  error
	done chan struct{}
}

func drain(sess Session) *output {
	o := &output{done: make(chan struct{})}
	go func() {
		defer close(o.done)
		b := make([]byte, 4096)
		for {
			n, err := sess.Read(b)
			o.mu.Lock()
			o.buf = append(o.buf, b[:n]...)
			if err != nil {
				o.err = err
				o.mu.Unlock()
				return
			}
			o.mu.Unlock()
		}
	}()
	return o
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.buf)
}

// readErr is valid once the drain goroutine has finished.
func (o *output) readErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

// waitFor blocks until the session has rendered marker, or the timeout expires.
func (o *output) waitFor(marker string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(o.String(), marker) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-o.done:
			// The stream ended; one last look at what it carried.
			return strings.Contains(o.String(), marker)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// waitFinished waits for the drain goroutine to stop reading.
func (o *output) waitFinished(timeout time.Duration) bool {
	select {
	case <-o.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// waitResult runs Wait on its own goroutine so a wedged teardown fails the test
// instead of hanging it.
func waitResult(sess Session, timeout time.Duration) (error, bool) {
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

func TestSessionCleanExit(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, "/c", "exit 0")
	defer sess.Close()

	err, ok := waitResult(sess, waitTimeout)
	if !ok {
		t.Fatal("Wait did not return for a child that exits immediately")
	}
	if err != nil {
		t.Fatalf("Wait = %v, want nil for a clean exit", err)
	}
}

func TestSessionNonZeroExit(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, "/c", "exit 3")
	defer sess.Close()

	err, ok := waitResult(sess, waitTimeout)
	if !ok {
		t.Fatal("Wait did not return for a child that exits immediately")
	}
	if err == nil {
		t.Fatal("Wait = nil, want an error for exit status 3")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("Wait = %v, want the exit status in the message", err)
	}
}

func TestSessionRendersChildOutput(t *testing.T) {
	const marker = "__LIMITPING_STDOUT_OK__"
	sess := startSession(t, context.Background(), cmdExe, "/c", "echo "+marker)
	defer sess.Close()

	out := drain(sess)
	if !out.waitFor(marker, outputTimeout) {
		t.Fatalf("child output never reached the pseudoconsole; got %q", out.String())
	}
}

// TestSessionStdinRoundTrip is the test the whole backend exists for: the
// providers steer the interactive CLIs by writing keystrokes, so a write has to
// reach the child as console input and be acted on.
//
// The marker is assembled from an environment variable the child sets itself,
// because the console echoes typed input straight back to the output pipe:
// searching the output for a marker that was also typed would pass on the echo
// alone. `__LIMITPING_STDIN_%LP%__` is what gets echoed, and only running the
// command can turn it into `__LIMITPING_STDIN_OK__`.
func TestSessionStdinRoundTrip(t *testing.T) {
	const marker = "__LIMITPING_STDIN_OK__"
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()

	out := drain(sess)
	if _, err := sess.Write([]byte("set LP=OK\r")); err != nil {
		t.Fatalf("Write(set) = %v", err)
	}
	if _, err := sess.Write([]byte("echo __LIMITPING_STDIN_%LP%__\r")); err != nil {
		t.Fatalf("Write(echo) = %v", err)
	}
	if !out.waitFor(marker, outputTimeout) {
		t.Fatalf("the child never ran the command written to its stdin; got %q", out.String())
	}

	// An explicit status keeps the exit unambiguous: bare `exit` carries
	// whatever ERRORLEVEL the session happens to be holding.
	if _, err := sess.Write([]byte("exit 0\r")); err != nil {
		t.Fatalf("Write(exit) = %v", err)
	}
	err, ok := waitResult(sess, waitTimeout)
	if !ok {
		t.Fatal("Wait did not return after the child was told to exit")
	}
	if err != nil {
		t.Fatalf("Wait = %v, want nil after `exit 0`", err)
	}
}

// TestSessionLaunchesCmdShim covers the shape an npm-installed CLI takes on
// Windows: a .cmd shim rather than a native executable. CreateProcess runs those
// through cmd.exe itself, and the arguments have to survive the round trip
// through the composed command line, quoting and all.
func TestSessionLaunchesCmdShim(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "limitping-probe.cmd")
	script := "@echo off\r\necho __LIMITPING_SHIM__ [%~1] [%~2]\r\n"
	if err := os.WriteFile(shim, []byte(script), 0o600); err != nil {
		t.Fatalf("writing the shim: %v", err)
	}

	sess := startSession(t, context.Background(), shim, "hello world", "plain")
	defer sess.Close()

	out := drain(sess)
	if !out.waitFor("__LIMITPING_SHIM__ [hello world] [plain]", outputTimeout) {
		t.Fatalf("the shim did not run with its arguments intact; got %q", out.String())
	}
	err, ok := waitResult(sess, waitTimeout)
	if !ok {
		t.Fatal("Wait did not return after the shim finished")
	}
	if err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
}

func TestSessionKill(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()

	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	if _, ok := waitResult(sess, waitTimeout); !ok {
		t.Fatal("Wait did not return after Kill")
	}
	// Killing a child that is already gone must stay safe.
	if err := sess.Kill(); err != nil {
		t.Fatalf("second Kill = %v", err)
	}
}

func TestSessionContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := startSession(t, ctx, cmdExe, interactiveArgs...)
	defer sess.Close()

	cancel()
	if _, ok := waitResult(sess, waitTimeout); !ok {
		t.Fatal("Wait did not return after the context was cancelled")
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)

	if err := sess.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if _, ok := waitResult(sess, waitTimeout); !ok {
		t.Fatal("Wait did not return after Close")
	}
}

// TestSessionReadEOFsOnChildExit pins the behavior the Unix master fd gives for
// free: once the child is gone the reader must reach end of input. ConPTY keeps
// the output pipe alive until the pseudoconsole is closed, so this only holds
// because the waiter closes it on child exit.
func TestSessionReadEOFsOnChildExit(t *testing.T) {
	const marker = "__LIMITPING_EOF_OK__"
	sess := startSession(t, context.Background(), cmdExe, "/c", "echo "+marker)
	defer sess.Close()

	out := drain(sess)
	if !out.waitFinished(outputTimeout) {
		t.Fatalf("Read never returned after the child exited; got %q", out.String())
	}
	if err := out.readErr(); !errors.Is(err, io.EOF) {
		t.Fatalf("Read error = %v, want io.EOF", err)
	}
	if !strings.Contains(out.String(), marker) {
		t.Fatalf("output ended without the child's output; got %q", out.String())
	}
}

// TestSessionCloseUnblocksRead covers the shape the providers use: a goroutine
// parked in Read while the session is closed from elsewhere. Both it and Wait
// have to come back, which is what the teardown ordering in conPTY.shutdown is
// for — a Close that waited on ClosePseudoConsole before killing the tree would
// hang here.
func TestSessionCloseUnblocksRead(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	out := drain(sess)

	// Let the shell get far enough to be sitting on the console read, so the
	// close lands on a genuinely blocked Read rather than on the startup burst.
	if !out.waitFor(">", outputTimeout) {
		t.Logf("no prompt seen before Close; got %q", out.String())
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !out.waitFinished(waitTimeout) {
		t.Fatal("Read stayed blocked after Close")
	}
	if _, ok := waitResult(sess, waitTimeout); !ok {
		t.Fatal("Wait did not return after Close")
	}
}

// TestSessionRapidStartStop shakes out handle and conhost leaks, and the races
// between a start and the teardown of the session before it.
func TestSessionRapidStartStop(t *testing.T) {
	for i := 0; i < 20; i++ {
		sess := startSession(t, context.Background(), cmdExe, "/c", "exit 0")
		err, ok := waitResult(sess, waitTimeout)
		if !ok {
			t.Fatalf("iteration %d: Wait did not return", i)
		}
		if err != nil {
			t.Fatalf("iteration %d: Wait = %v", i, err)
		}
		if err := sess.Close(); err != nil {
			t.Fatalf("iteration %d: Close = %v", i, err)
		}
	}
}

// TestSessionConcurrent runs several sessions at once: the conhost identification
// diffs this process's children around CreatePseudoConsole, so overlapping starts
// must not confuse each other.
func TestSessionConcurrent(t *testing.T) {
	const sessions = 6
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := "__LIMITPING_CONCURRENT_" + string(rune('A'+i)) + "__"
			sess, err := Start(context.Background(), cmdExe, []string{"/c", "echo " + marker})
			if err != nil {
				t.Errorf("session %d: Start = %v", i, err)
				return
			}
			defer sess.Close()

			out := drain(sess)
			if !out.waitFor(marker, outputTimeout) {
				t.Errorf("session %d: never rendered its marker; got %q", i, out.String())
			}
			if err, ok := waitResult(sess, waitTimeout); !ok {
				t.Errorf("session %d: Wait did not return", i)
			} else if err != nil {
				t.Errorf("session %d: Wait = %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}

// jobBasicAccounting mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, which
// x/sys/windows does not declare. Only ActiveProcesses is read here, to observe
// the whole process tree rather than just the child this package holds a handle
// to.
type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func activeProcesses(t *testing.T, job *jobObject) uint32 {
	t.Helper()
	var info jobBasicAccounting
	if err := windows.QueryInformationJobObject(
		job.h, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil,
	); err != nil {
		t.Fatalf("QueryInformationJobObject = %v", err)
	}
	return info.ActiveProcesses
}

// TestSessionKillsProcessTree checks that a grandchild spawned by the shell dies
// with the session. That is the point of creating the child suspended and
// assigning it to the job before it runs: closing the pseudoconsole only
// notifies the clients attached at that moment, so nothing but the job kill
// reaps a process that was started later.
func TestSessionKillsProcessTree(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()

	ws := sess.(*windowsSession)
	out := drain(sess)

	// `start /b` runs the grandchild in the same console, where `pause` blocks
	// on the console read and keeps it alive.
	if _, err := sess.Write([]byte("start \"\" /b cmd.exe /Q /D /c pause\r")); err != nil {
		t.Fatalf("Write = %v", err)
	}

	deadline := time.Now().Add(outputTimeout)
	for activeProcesses(t, ws.job) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the grandchild never joined the job; output %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill = %v", err)
	}
	deadline = time.Now().Add(waitTimeout)
	for {
		if n := activeProcesses(t, ws.job); n == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("%d processes still alive in the job after Kill", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestConhostReturnsToBaseline checks the console hosts are reaped rather than
// accumulating: each pseudoconsole spawns one as a direct child of this process,
// and pre-24H2 builds can leave it running after its clients are killed.
func TestConhostReturnsToBaseline(t *testing.T) {
	baseline := len(conhostChildren())

	for i := 0; i < 4; i++ {
		sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
		out := drain(sess)
		out.waitFor(">", outputTimeout)
		if err := sess.Close(); err != nil {
			t.Fatalf("iteration %d: Close = %v", i, err)
		}
		if _, ok := waitResult(sess, waitTimeout); !ok {
			t.Fatalf("iteration %d: Wait did not return", i)
		}
	}

	// The reap in Close is bounded but asynchronous on the OS side, so give the
	// hosts a moment to disappear before concluding they leaked.
	deadline := time.Now().Add(waitTimeout)
	for {
		n := len(conhostChildren())
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("conhost children = %d, baseline %d: console hosts leaked", n, baseline)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSessionCtrlC records what a raw ETX byte does to a console child, because
// the Codex trigger stops its TUI by writing one. The pseudoconsole turns the
// byte into a real console interrupt: the shell abandons the line it was
// editing, so the command typed before it never runs.
//
// The interrupt has to arrive as its own write. Sent in the same burst as the
// line it should cancel, it is consumed as part of that line and the command
// runs anyway — which is how the providers use it (a lone write, repeated on a
// ticker), so this test writes it the same way.
//
// Two limits of the mechanism, both found here and neither pinned as a test:
// this conhost cancels the line without echoing the usual "^C", and a command
// already running (rather than reading the console) keeps running — the ETX is
// only turned into an interrupt when a client reads it, so ConPTY input alone
// does not stand in for GenerateConsoleCtrlEvent. Killing the job is what the
// providers fall back to, and that is covered by TestSessionKill.
func TestSessionCtrlC(t *testing.T) {
	sess := startSession(t, context.Background(), cmdExe, interactiveArgs...)
	defer sess.Close()

	out := drain(sess)
	if _, err := sess.Write([]byte("set LP=RAN\r")); err != nil {
		t.Fatalf("Write(set) = %v", err)
	}
	if !out.waitFor(">", outputTimeout) {
		t.Fatalf("no prompt to type at; got %q", out.String())
	}

	// A line left unsubmitted. Waiting for the echo confirms the line editor has
	// taken it, so the interrupt that follows cannot be swallowed as part of it.
	if _, err := sess.Write([]byte("echo __LIMITPING_CTRLC_%LP%__")); err != nil {
		t.Fatalf("Write(line) = %v", err)
	}
	if !out.waitFor("__LIMITPING_CTRLC_%LP%__", outputTimeout) {
		t.Fatalf("the line was never echoed back; got %q", out.String())
	}

	if _, err := sess.Write([]byte{0x03}); err != nil {
		t.Fatalf("Write(ETX) = %v", err)
	}
	// The Enter that would have run the line, had the interrupt not thrown it
	// away, followed by a command that must run — it both proves the session
	// survived the interrupt and orders the check below: once its marker is on
	// screen, the interrupted line has had its chance.
	if _, err := sess.Write([]byte("\r")); err != nil {
		t.Fatalf("Write(CR) = %v", err)
	}
	if _, err := sess.Write([]byte("echo __LIMITPING_AFTER_%LP%__\r")); err != nil {
		t.Fatalf("Write(after) = %v", err)
	}
	if !out.waitFor("__LIMITPING_AFTER_RAN__", outputTimeout) {
		t.Fatalf("the session did not survive the interrupt; got %q", out.String())
	}
	if strings.Contains(out.String(), "__LIMITPING_CTRLC_RAN__") {
		t.Fatalf("the interrupted line ran anyway; got %q", out.String())
	}
}
