package daemon

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const daemonTestTimeout = 5 * time.Second

func TestDaemonStartServesStatusAndSecuresRuntimeFiles(t *testing.T) {
	requireSupportedUnix(t)

	dataDir := shortDataDir(t)
	daemon, err := New(Options{
		DataDir:         dataDir,
		ShutdownTimeout: daemonTestTimeout,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got, want := daemon.SocketPath(), filepath.Join(dataDir, "cronmate.sock"); got != want {
		t.Fatalf("SocketPath() = %q, want %q", got, want)
	}
	if got, want := daemon.PIDPath(), filepath.Join(dataDir, "cronmate.pid"); got != want {
		t.Fatalf("PIDPath() = %q, want %q", got, want)
	}

	if err := daemon.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	assertPathMode(t, dataDir, os.ModeDir, 0o700)
	assertPathMode(t, daemon.PIDPath(), 0, 0o600)
	assertPathMode(t, daemon.SocketPath(), os.ModeSocket, 0o600)

	pidContents, err := os.ReadFile(daemon.PIDPath())
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidContents)))
	if err != nil {
		t.Fatalf("pid file contents = %q, want a process ID: %v", pidContents, err)
	}
	if pid != os.Getpid() {
		t.Fatalf("pid file = %d, want %d", pid, os.Getpid())
	}

	status := requestStatus(t, daemon.SocketPath())
	if status.State != "running" {
		t.Errorf("status state = %q, want running", status.State)
	}
	if status.PID != os.Getpid() {
		t.Errorf("status pid = %d, want %d", status.PID, os.Getpid())
	}
	if status.InstanceID == "" {
		t.Error("status instance_id is empty")
	}
	if status.StartedAt.IsZero() {
		t.Error("status started_at is zero")
	}
}

func TestClientShutdownRequiresCurrentInstance(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	if err := daemon.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	client, err := NewClient(daemon.SocketPath())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancel()

	firstStatus, err := client.Status(ctx)
	if err != nil {
		t.Fatalf("first Status() error = %v", err)
	}
	if err := client.Shutdown(ctx, "not-the-current-instance"); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("Shutdown() with wrong instance error = %v, want HTTP 409", err)
	}
	if status := requestStatus(t, daemon.SocketPath()); status.InstanceID != firstStatus.InstanceID {
		t.Fatalf("wrong-instance shutdown changed daemon status: got %#v, want instance %q", status, firstStatus.InstanceID)
	}

	if err := daemon.Shutdown(ctx); err != nil {
		t.Fatalf("first direct Shutdown() error = %v", err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatalf("restart Start() error = %v", err)
	}
	secondStatus := requestStatus(t, daemon.SocketPath())
	if secondStatus.InstanceID == "" || secondStatus.InstanceID == firstStatus.InstanceID {
		t.Fatalf("restart instance id = %q, want a new non-empty id", secondStatus.InstanceID)
	}

	if err := client.Shutdown(ctx, firstStatus.InstanceID); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("Shutdown() with stale instance error = %v, want HTTP 409", err)
	}
	if status := requestStatus(t, daemon.SocketPath()); status.InstanceID != secondStatus.InstanceID {
		t.Fatalf("stale-instance shutdown changed daemon status: got %#v, want instance %q", status, secondStatus.InstanceID)
	}

	if err := client.Shutdown(ctx, secondStatus.InstanceID); err != nil {
		t.Fatalf("Shutdown() with current instance error = %v", err)
	}
	if err := daemon.Shutdown(ctx); err != nil {
		t.Fatalf("wait for requested Shutdown() error = %v", err)
	}
	assertPathDoesNotExist(t, daemon.SocketPath())
	assertPathDoesNotExist(t, daemon.PIDPath())
}

func TestRunReturnsAfterClientShutdown(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	runDone := make(chan error, 1)
	go func() {
		runDone <- daemon.Run(context.Background())
	}()
	t.Cleanup(func() {
		shutdownDaemon(t, daemon)
	})

	status := waitForRunningDaemon(t, daemon.SocketPath(), runDone)
	client, err := NewClient(daemon.SocketPath())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancel()
	if err := client.Shutdown(ctx, status.InstanceID); err != nil {
		t.Fatalf("Client.Shutdown() error = %v", err)
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() after client shutdown error = %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("Run() did not return after client shutdown: %v", ctx.Err())
	}
	assertPathDoesNotExist(t, daemon.SocketPath())
	assertPathDoesNotExist(t, daemon.PIDPath())
}

func TestDaemonRejectsSecondInstanceWithoutDisturbingFirst(t *testing.T) {
	requireSupportedUnix(t)

	dataDir := shortDataDir(t)
	first := newTestDaemon(t, dataDir)
	if err := first.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	registerDaemonCleanup(t, first)

	socketBefore, err := os.Lstat(first.SocketPath())
	if err != nil {
		t.Fatalf("lstat first socket: %v", err)
	}
	pidBefore, err := os.Lstat(first.PIDPath())
	if err != nil {
		t.Fatalf("lstat first pid file: %v", err)
	}

	second := newTestDaemon(t, dataDir)
	if err := second.Start(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyRunning", err)
	}

	socketAfter, err := os.Lstat(first.SocketPath())
	if err != nil {
		t.Fatalf("lstat socket after rejected start: %v", err)
	}
	if !os.SameFile(socketBefore, socketAfter) {
		t.Fatal("rejected second start replaced the running daemon's socket")
	}
	pidAfter, err := os.Lstat(first.PIDPath())
	if err != nil {
		t.Fatalf("lstat pid file after rejected start: %v", err)
	}
	if !os.SameFile(pidBefore, pidAfter) {
		t.Fatal("rejected second start replaced the running daemon's pid file")
	}

	status := requestStatus(t, first.SocketPath())
	if status.State != "running" || status.PID != os.Getpid() {
		t.Fatalf("first daemon status after rejected start = %#v", status)
	}
}

func TestDaemonRecoversStaleSocketAndPIDFile(t *testing.T) {
	requireSupportedUnix(t)

	dataDir := shortDataDir(t)
	daemon := newTestDaemon(t, dataDir)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create data directory: %v", err)
	}

	listener, err := net.Listen("unix", daemon.SocketPath())
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		listener.Close()
		t.Fatalf("unix listener has type %T", listener)
	}
	unixListener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale socket listener: %v", err)
	}
	assertPathType(t, daemon.SocketPath(), os.ModeSocket)

	if err := os.WriteFile(daemon.PIDPath(), []byte("not-a-live-pid\n"), 0o600); err != nil {
		t.Fatalf("create stale pid file: %v", err)
	}

	if err := daemon.Start(); err != nil {
		t.Fatalf("Start() with stale artifacts error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	status := requestStatus(t, daemon.SocketPath())
	if status.State != "running" || status.PID != os.Getpid() {
		t.Fatalf("status after stale socket recovery = %#v", status)
	}
	pidContents, err := os.ReadFile(daemon.PIDPath())
	if err != nil {
		t.Fatalf("read replaced pid file: %v", err)
	}
	if got, want := strings.TrimSpace(string(pidContents)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("pid file after recovery = %q, want %q", got, want)
	}
}

func TestDaemonPreservesUnsafeSocketPathCollisions(t *testing.T) {
	requireSupportedUnix(t)

	t.Run("regular file", func(t *testing.T) {
		dataDir := shortDataDir(t)
		daemon := newTestDaemon(t, dataDir)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatalf("create data directory: %v", err)
		}

		original := []byte("must not be removed")
		if err := os.WriteFile(daemon.SocketPath(), original, 0o640); err != nil {
			t.Fatalf("create socket-path file: %v", err)
		}
		before, err := os.Lstat(daemon.SocketPath())
		if err != nil {
			t.Fatalf("lstat socket-path file: %v", err)
		}

		if err := daemon.Start(); err == nil {
			registerDaemonCleanup(t, daemon)
			t.Fatal("Start() error = nil with a regular file at the socket path")
		}

		after, err := os.Lstat(daemon.SocketPath())
		if err != nil {
			t.Fatalf("lstat socket-path file after Start: %v", err)
		}
		if !os.SameFile(before, after) || after.Mode() != before.Mode() {
			t.Fatalf("socket-path file changed: before=%v after=%v", before.Mode(), after.Mode())
		}
		contents, err := os.ReadFile(daemon.SocketPath())
		if err != nil {
			t.Fatalf("read socket-path file after Start: %v", err)
		}
		if string(contents) != string(original) {
			t.Fatalf("socket-path file = %q, want %q", contents, original)
		}
		assertPathDoesNotExist(t, daemon.PIDPath())
	})

	t.Run("symbolic link", func(t *testing.T) {
		dataDir := shortDataDir(t)
		daemon := newTestDaemon(t, dataDir)
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatalf("create data directory: %v", err)
		}

		targetPath := filepath.Join(filepath.Dir(dataDir), "socket-target")
		original := []byte("symlink target must not be changed")
		if err := os.WriteFile(targetPath, original, 0o600); err != nil {
			t.Fatalf("create symlink target: %v", err)
		}
		if err := os.Symlink(targetPath, daemon.SocketPath()); err != nil {
			t.Fatalf("create socket-path symlink: %v", err)
		}

		if err := daemon.Start(); err == nil {
			registerDaemonCleanup(t, daemon)
			t.Fatal("Start() error = nil with a symlink at the socket path")
		}

		info, err := os.Lstat(daemon.SocketPath())
		if err != nil {
			t.Fatalf("lstat socket-path symlink after Start: %v", err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("socket path mode after Start = %v, want symlink", info.Mode())
		}
		gotTarget, err := os.Readlink(daemon.SocketPath())
		if err != nil {
			t.Fatalf("read socket-path symlink after Start: %v", err)
		}
		if gotTarget != targetPath {
			t.Fatalf("socket-path symlink target = %q, want %q", gotTarget, targetPath)
		}
		contents, err := os.ReadFile(targetPath)
		if err != nil {
			t.Fatalf("read symlink target after Start: %v", err)
		}
		if string(contents) != string(original) {
			t.Fatalf("symlink target = %q, want %q", contents, original)
		}
		assertPathDoesNotExist(t, daemon.PIDPath())
	})
}

func TestDaemonRunCancellationCleansUpAndAllowsRestart(t *testing.T) {
	requireSupportedUnix(t)

	dataDir := shortDataDir(t)
	daemon := newTestDaemon(t, dataDir)
	runContext, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- daemon.Run(runContext)
	}()
	t.Cleanup(func() {
		cancelRun()
		shutdownDaemon(t, daemon)
	})

	waitForRunningDaemon(t, daemon.SocketPath(), runDone)
	cancelRun()

	waitContext, cancelWait := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelWait()
	select {
	case err := <-runDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() after cancellation error = %v", err)
		}
	case <-waitContext.Done():
		t.Fatalf("Run() did not return after cancellation: %v", waitContext.Err())
	}
	assertPathDoesNotExist(t, daemon.SocketPath())
	assertPathDoesNotExist(t, daemon.PIDPath())

	restarted := newTestDaemon(t, dataDir)
	if err := restarted.Start(); err != nil {
		t.Fatalf("restarted Start() error = %v", err)
	}
	registerDaemonCleanup(t, restarted)
	status := requestStatus(t, restarted.SocketPath())
	if status.State != "running" || status.PID != os.Getpid() {
		t.Fatalf("restarted daemon status = %#v", status)
	}
}

func TestDaemonConcurrentShutdownIsIdempotent(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	if err := daemon.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	const callers = 16
	start := make(chan struct{})
	errorsByCaller := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelShutdown()
	for range callers {
		go func() {
			ready.Done()
			<-start
			errorsByCaller <- daemon.Shutdown(shutdownContext)
		}()
	}
	ready.Wait()
	close(start)

	for range callers {
		select {
		case err := <-errorsByCaller:
			if err != nil {
				t.Errorf("concurrent Shutdown() error = %v", err)
			}
		case <-shutdownContext.Done():
			t.Fatalf("concurrent Shutdown() calls did not finish: %v", shutdownContext.Err())
		}
	}
	if err := daemon.Shutdown(shutdownContext); err != nil {
		t.Errorf("repeated Shutdown() error = %v", err)
	}
	if err := daemon.Wait(); err != nil {
		t.Errorf("Wait() after Shutdown() error = %v", err)
	}
	assertPathDoesNotExist(t, daemon.SocketPath())
	assertPathDoesNotExist(t, daemon.PIDPath())
}

func TestDaemonCanRestartSameInstance(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	if err := daemon.Start(); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	daemon.mu.RLock()
	firstRun := daemon.current
	daemon.mu.RUnlock()
	if firstRun == nil {
		t.Fatal("first Start() did not install a current run")
	}
	requestStatus(t, daemon.SocketPath())

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), daemonTestTimeout)
	if err := daemon.Shutdown(shutdownContext); err != nil {
		cancelShutdown()
		t.Fatalf("first Shutdown() error = %v", err)
	}
	cancelShutdown()
	if err := daemon.Wait(); err != nil {
		t.Fatalf("Wait() for first run error = %v", err)
	}

	if err := daemon.Start(); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	daemon.mu.RLock()
	secondRun := daemon.current
	daemon.mu.RUnlock()
	if secondRun == nil || secondRun == firstRun {
		t.Fatalf("second Start() run = %p, want a new generation distinct from %p", secondRun, firstRun)
	}

	status := requestStatus(t, daemon.SocketPath())
	if status.State != StateRunning || status.PID != os.Getpid() || status.StartedAt.IsZero() {
		t.Fatalf("status after same-instance restart = %#v", status)
	}
}

func TestShutdownRunIgnoresOldGenerationAfterRestart(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	firstRun, err := daemon.start()
	if err != nil {
		t.Fatalf("first start() error = %v", err)
	}
	registerDaemonCleanup(t, daemon)

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), daemonTestTimeout)
	if err := daemon.shutdownRun(shutdownContext, firstRun); err != nil {
		cancelShutdown()
		t.Fatalf("shutdownRun(first) error = %v", err)
	}
	cancelShutdown()

	secondRun, err := daemon.start()
	if err != nil {
		t.Fatalf("second start() error = %v", err)
	}
	socketBefore, err := os.Lstat(daemon.SocketPath())
	if err != nil {
		t.Fatalf("lstat second run socket: %v", err)
	}
	pidBefore, err := os.Lstat(daemon.PIDPath())
	if err != nil {
		t.Fatalf("lstat second run pid file: %v", err)
	}

	staleContext, cancelStale := context.WithTimeout(context.Background(), daemonTestTimeout)
	if err := daemon.shutdownRun(staleContext, firstRun); err != nil {
		cancelStale()
		t.Fatalf("shutdownRun(first) after restart error = %v", err)
	}
	cancelStale()

	daemon.mu.RLock()
	current := daemon.current
	state := daemon.state
	daemon.mu.RUnlock()
	if current != secondRun || state != StateRunning {
		t.Fatalf("daemon after stale shutdown = (run %p, state %q), want (run %p, state %q)", current, state, secondRun, StateRunning)
	}
	select {
	case <-secondRun.done:
		t.Fatal("stale shutdown stopped the replacement run")
	default:
	}
	assertSamePath(t, daemon.SocketPath(), socketBefore)
	assertSamePath(t, daemon.PIDPath(), pidBefore)

	status := requestStatus(t, daemon.SocketPath())
	if status.State != StateRunning || status.PID != os.Getpid() {
		t.Fatalf("replacement status after stale shutdown = %#v", status)
	}
}

func TestRunCancellationFromOldGenerationCannotStopReplacement(t *testing.T) {
	requireSupportedUnix(t)

	daemon := newTestDaemon(t, shortDataDir(t))
	runContext, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	runDone := make(chan error, 1)
	go func() {
		runDone <- daemon.Run(runContext)
	}()
	registerDaemonCleanup(t, daemon)

	waitForRunningDaemon(t, daemon.SocketPath(), runDone)
	daemon.mu.RLock()
	firstRun := daemon.current
	daemon.mu.RUnlock()
	if firstRun == nil {
		t.Fatal("Run() did not install its generation")
	}

	// Hold shutdown serialization so the cancelled Run cannot act on its
	// captured generation until a replacement has been installed.
	daemon.shutdownMu.Lock()
	shutdownLocked := true
	defer func() {
		if shutdownLocked {
			daemon.shutdownMu.Unlock()
		}
	}()
	cancelRun()

	stopContext, cancelStop := context.WithTimeout(context.Background(), daemonTestTimeout)
	if err := firstRun.server.Shutdown(stopContext); err != nil {
		cancelStop()
		t.Fatalf("stop first generation server: %v", err)
	}
	cancelStop()
	if err := firstRun.wait(); err != nil {
		t.Fatalf("wait for first generation server: %v", err)
	}
	if err := cleanupResources(
		daemon.socketPath,
		firstRun.listener,
		firstRun.socketInfo,
		daemon.pidPath,
		firstRun.pidFile,
		firstRun.pidInfo,
	); err != nil {
		t.Fatalf("clean up first generation: %v", err)
	}

	// Complete the same ownership transition shutdownRun performs. Run is
	// still queued on shutdownMu with firstRun captured.
	daemon.mu.Lock()
	if daemon.current != firstRun {
		current := daemon.current
		daemon.mu.Unlock()
		t.Fatalf("current run changed before replacement: got %p, want %p", current, firstRun)
	}
	daemon.current = nil
	daemon.last = firstRun
	daemon.state = StateStopped
	daemon.mu.Unlock()

	secondRun, err := daemon.start()
	if err != nil {
		t.Fatalf("start replacement generation: %v", err)
	}
	daemon.shutdownMu.Unlock()
	shutdownLocked = false

	waitContext, cancelWait := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelWait()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() for first generation error = %v", err)
		}
	case <-waitContext.Done():
		t.Fatalf("Run() for first generation did not finish: %v", waitContext.Err())
	}

	daemon.mu.RLock()
	current := daemon.current
	state := daemon.state
	daemon.mu.RUnlock()
	if current != secondRun || state != StateRunning {
		t.Fatalf("daemon after old Run returned = (run %p, state %q), want (run %p, state %q)", current, state, secondRun, StateRunning)
	}
	select {
	case <-secondRun.done:
		t.Fatal("Run from the first generation stopped the replacement")
	default:
	}
	status := requestStatus(t, daemon.SocketPath())
	if status.State != StateRunning || status.PID != os.Getpid() {
		t.Fatalf("replacement status after old Run returned = %#v", status)
	}
}

func newTestDaemon(t *testing.T, dataDir string) *Daemon {
	t.Helper()

	daemon, err := New(Options{
		DataDir:         dataDir,
		ShutdownTimeout: daemonTestTimeout,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return daemon
}

func requestStatus(t *testing.T, socketPath string) Status {
	t.Helper()

	client, err := NewClient(socketPath)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)

	requestContext, cancelRequest := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelRequest()
	status, err := client.Status(requestContext)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	return status
}

func waitForRunningDaemon(t *testing.T, socketPath string, runDone <-chan error) Status {
	t.Helper()

	waitContext, cancelWait := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelWait()
	for {
		select {
		case err := <-runDone:
			t.Fatalf("Run() returned before becoming ready: %v", err)
		case <-waitContext.Done():
			t.Fatalf("daemon did not create its socket: %v", waitContext.Err())
		default:
		}

		_, err := os.Lstat(socketPath)
		switch {
		case err == nil:
			client, err := NewClient(socketPath)
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.CloseIdleConnections()
			status, err := client.Status(waitContext)
			if err != nil {
				t.Fatalf("Status() while waiting for readiness error = %v", err)
			}
			return status
		case errors.Is(err, os.ErrNotExist):
			runtime.Gosched()
		default:
			t.Fatalf("lstat daemon socket while waiting for readiness: %v", err)
		}
	}
}

func registerDaemonCleanup(t *testing.T, daemon *Daemon) {
	t.Helper()
	t.Cleanup(func() {
		shutdownDaemon(t, daemon)
	})
}

func shutdownDaemon(t *testing.T, daemon *Daemon) {
	t.Helper()

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), daemonTestTimeout)
	defer cancelShutdown()
	if err := daemon.Shutdown(shutdownContext); err != nil {
		t.Errorf("cleanup Shutdown() error = %v", err)
	}
}

func shortDataDir(t *testing.T) string {
	t.Helper()

	root, err := os.MkdirTemp("", "cm-")
	if err != nil {
		t.Fatalf("create short temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove temporary directory %q: %v", root, err)
		}
	})
	return filepath.Join(root, "data")
}

func assertPathMode(t *testing.T, path string, kind os.FileMode, permissions os.FileMode) {
	t.Helper()

	info := assertPathType(t, path, kind)
	if info.Mode().Perm() != permissions {
		t.Errorf("%q permissions = %04o, want %04o", path, info.Mode().Perm(), permissions)
	}
}

func assertPathType(t *testing.T, path string, kind os.FileMode) os.FileInfo {
	t.Helper()

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	if got := info.Mode() & os.ModeType; got != kind {
		t.Errorf("%q type = %v, want %v", path, got, kind)
	}
	return info
}

func assertPathDoesNotExist(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Lstat(%q) error = %v, want os.ErrNotExist", path, err)
	}
}

func assertSamePath(t *testing.T, path string, want os.FileInfo) {
	t.Helper()

	got, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %q: %v", path, err)
	}
	if !os.SameFile(got, want) {
		t.Fatalf("path %q was replaced", path)
	}
}

func requireSupportedUnix(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("Unix-socket daemon is supported on macOS and Linux, not %s", runtime.GOOS)
	}
}
