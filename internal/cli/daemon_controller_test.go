//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	daemoncore "cronmate/internal/daemon"
)

func TestSystemDaemonControllerStatusUsesCoreDaemon(t *testing.T) {
	dataDir, core := newDaemonControllerTestCore(t)
	startDaemonControllerTestCore(t, core)
	controller := newDaemonControllerTestController(dataDir, core)

	status, err := controller.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if status.State != daemoncore.StateRunning {
		t.Errorf("Status().State = %q, want %q", status.State, daemoncore.StateRunning)
	}
	if status.PID != os.Getpid() {
		t.Errorf("Status().PID = %d, want %d", status.PID, os.Getpid())
	}
	if status.InstanceID == "" {
		t.Error("Status().InstanceID is empty")
	}
	if status.StartedAt.IsZero() {
		t.Error("Status().StartedAt is zero")
	}
}

func TestSystemDaemonControllerStopUsesCoreShutdownEndpoint(t *testing.T) {
	dataDir, core := newDaemonControllerTestCore(t)
	startDaemonControllerTestCore(t, core)
	controller := newDaemonControllerTestController(dataDir, core)
	before := core.Status()
	if before.InstanceID == "" {
		t.Fatal("core status has an empty instance id before Stop()")
	}

	if err := controller.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	after := core.Status()
	if after.State != daemoncore.StateStopped {
		t.Errorf("core state after Stop() = %q, want %q", after.State, daemoncore.StateStopped)
	}
	if after.InstanceID != "" {
		t.Errorf("core instance id after Stop() = %q, want empty", after.InstanceID)
	}
	assertDaemonControllerTestPathMissing(t, core.SocketPath())
	assertDaemonControllerTestPathMissing(t, core.PIDPath())
}

func TestSystemDaemonControllerStartWaitsForInjectedChildCore(t *testing.T) {
	dataDir, core := newDaemonControllerTestCore(t)
	controller := newDaemonControllerTestController(dataDir, core)
	child := newDaemonControllerTestChild(os.Getpid())
	launchCalls := 0
	controller.launch = func(gotDataDir string) (daemonChild, error) {
		launchCalls++
		if gotDataDir != dataDir {
			return nil, errors.New("controller launched with an unexpected data directory")
		}
		if err := core.Start(); err != nil {
			return nil, err
		}
		return child, nil
	}
	t.Cleanup(func() {
		shutdownDaemonControllerTestCore(t, core)
	})

	status, err := controller.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", launchCalls)
	}
	if status.State != daemoncore.StateRunning || status.PID != os.Getpid() {
		t.Fatalf("Start() status = %#v, want running pid %d", status, os.Getpid())
	}
	if len(child.signals) != 0 {
		t.Fatalf("injected child received signals = %v, want none", child.signals)
	}
}

func TestSystemDaemonControllerStartWithPreCanceledContextDoesNotLaunch(t *testing.T) {
	launchCalls := 0
	controller := &systemDaemonController{
		launch: func(string) (daemonChild, error) {
			launchCalls++
			return newDaemonControllerTestChild(1234), nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, err := controller.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}
	if status != (daemoncore.Status{}) {
		t.Errorf("Start() status = %#v, want zero status", status)
	}
	if launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0", launchCalls)
	}
}

func TestSystemDaemonControllerStartStopsChildOnPIDMismatch(t *testing.T) {
	dataDir, core := newDaemonControllerTestCore(t)
	controller := newDaemonControllerTestController(dataDir, core)
	child := newDaemonControllerTestChild(os.Getpid() + 1)
	controller.launch = func(string) (daemonChild, error) {
		if err := core.Start(); err != nil {
			return nil, err
		}
		return child, nil
	}
	t.Cleanup(func() {
		shutdownDaemonControllerTestCore(t, core)
	})

	status, err := controller.Start(context.Background())
	if !errors.Is(err, daemoncore.ErrAlreadyRunning) {
		t.Fatalf("Start() error = %v, want ErrAlreadyRunning", err)
	}
	if status != (daemoncore.Status{}) {
		t.Errorf("Start() status = %#v, want zero status", status)
	}
	if !strings.Contains(err.Error(), "socket belongs to pid") {
		t.Errorf("Start() error = %q, want pid-mismatch detail", err)
	}
	if len(child.signals) != 1 || child.signals[0] != syscall.SIGTERM {
		t.Fatalf("child signals = %v, want one SIGTERM", child.signals)
	}
	select {
	case _, open := <-child.Exited():
		if open {
			t.Fatal("child exit channel returned a value, want it closed")
		}
	default:
		t.Fatal("child exit channel is still open after Start() returned")
	}
}

func TestSystemDaemonControllerStartRecoversStaleSocketAndMalformedPID(t *testing.T) {
	dataDir, core := newDaemonControllerTestCore(t)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("create daemon data directory: %v", err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: core.SocketPath(), Net: "unix"})
	if err != nil {
		t.Fatalf("create stale unix socket: %v", err)
	}
	listener.SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale unix socket: %v", err)
	}
	if err := os.WriteFile(core.PIDPath(), []byte("malformed-pid\n"), 0o600); err != nil {
		t.Fatalf("create malformed pid file: %v", err)
	}

	controller := newDaemonControllerTestController(dataDir, core)
	child := newDaemonControllerTestChild(os.Getpid())
	launchCalls := 0
	controller.launch = func(string) (daemonChild, error) {
		launchCalls++
		if err := core.Start(); err != nil {
			return nil, err
		}
		return child, nil
	}
	t.Cleanup(func() {
		shutdownDaemonControllerTestCore(t, core)
	})

	status, err := controller.Start(context.Background())
	if err != nil {
		t.Fatalf("Start() with stale artifacts error = %v", err)
	}
	if launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", launchCalls)
	}
	if status.State != daemoncore.StateRunning || status.PID != os.Getpid() {
		t.Fatalf("Start() status = %#v, want running pid %d", status, os.Getpid())
	}

	contents, err := os.ReadFile(core.PIDPath())
	if err != nil {
		t.Fatalf("read recovered pid file: %v", err)
	}
	if got, want := strings.TrimSpace(string(contents)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("recovered pid file = %q, want %q", got, want)
	}
}

func TestReadDaemonPID(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cronmate.pid")
		if err := os.WriteFile(path, []byte("  4321 \n"), 0o600); err != nil {
			t.Fatalf("write pid file: %v", err)
		}

		pid, openedInfo, err := readDaemonPID(path)
		if err != nil {
			t.Fatalf("readDaemonPID() error = %v", err)
		}
		if pid != 4321 {
			t.Errorf("readDaemonPID() pid = %d, want 4321", pid)
		}
		pathInfo, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("lstat pid file: %v", err)
		}
		if !os.SameFile(openedInfo, pathInfo) {
			t.Error("readDaemonPID() returned information for a different file")
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, _, err := readDaemonPID(filepath.Join(t.TempDir(), "missing.pid"))
		if !errors.Is(err, errDaemonNotRunning) {
			t.Fatalf("readDaemonPID() error = %v, want errDaemonNotRunning", err)
		}
	})

	invalidContents := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "empty", contents: "", want: "invalid pid"},
		{name: "pid one", contents: "1\n", want: "invalid pid"},
		{name: "pid zero", contents: "0\n", want: "invalid pid"},
		{name: "negative", contents: "-2\n", want: "invalid pid"},
		{name: "not numeric", contents: "abc\n", want: "invalid pid"},
		{name: "too large", contents: strings.Repeat("9", 65), want: "too large"},
	}
	for _, test := range invalidContents {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cronmate.pid")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write pid file: %v", err)
			}

			_, _, err := readDaemonPID(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readDaemonPID() error = %v, want containing %q", err, test.want)
			}
		})
	}

	t.Run("non regular path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cronmate.pid")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create pid-path directory: %v", err)
		}
		_, _, err := readDaemonPID(path)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("readDaemonPID() error = %v, want non-regular-file error", err)
		}
	})
}

func TestDaemonArtifactIdentityHelpers(t *testing.T) {
	t.Run("same path", func(t *testing.T) {
		path := daemonControllerTestFile(t, "artifact")
		expected := daemonControllerTestPathInfo(t, path)

		if err := requireSamePath(path, expected); err != nil {
			t.Fatalf("requireSamePath() error = %v", err)
		}
		gone, err := ownedPathGone(path, expected)
		if err != nil {
			t.Fatalf("ownedPathGone() error = %v", err)
		}
		if gone {
			t.Error("ownedPathGone() = true while the owned path still exists")
		}
	})

	t.Run("removed path", func(t *testing.T) {
		path := daemonControllerTestFile(t, "artifact")
		expected := daemonControllerTestPathInfo(t, path)
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove artifact: %v", err)
		}

		gone, err := ownedPathGone(path, expected)
		if err != nil {
			t.Fatalf("ownedPathGone() error = %v", err)
		}
		if !gone {
			t.Error("ownedPathGone() = false after the owned path was removed")
		}
	})

	t.Run("replaced path", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "artifact")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatalf("write original artifact: %v", err)
		}
		expected := daemonControllerTestPathInfo(t, path)
		replacement := filepath.Join(directory, "replacement")
		if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
			t.Fatalf("write replacement artifact: %v", err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatalf("replace artifact: %v", err)
		}

		if err := requireSamePath(path, expected); err == nil || !strings.Contains(err.Error(), "changed") {
			t.Fatalf("requireSamePath() error = %v, want identity-change error", err)
		}
		if gone, err := ownedPathGone(path, expected); err == nil || gone || !strings.Contains(err.Error(), "replaced") {
			t.Fatalf("ownedPathGone() = (%t, %v), want (false, replacement error)", gone, err)
		}
	})

	t.Run("both daemon artifacts removed", func(t *testing.T) {
		socketPath := daemonControllerTestFile(t, "socket")
		pidPath := daemonControllerTestFile(t, "pid")
		socketInfo := daemonControllerTestPathInfo(t, socketPath)
		pidInfo := daemonControllerTestPathInfo(t, pidPath)

		if err := os.Remove(socketPath); err != nil {
			t.Fatalf("remove socket artifact: %v", err)
		}
		gone, err := ownedDaemonArtifactsGone(socketPath, socketInfo, pidPath, pidInfo)
		if err != nil {
			t.Fatalf("ownedDaemonArtifactsGone() error = %v", err)
		}
		if gone {
			t.Error("ownedDaemonArtifactsGone() = true while the pid artifact remains")
		}

		if err := os.Remove(pidPath); err != nil {
			t.Fatalf("remove pid artifact: %v", err)
		}
		gone, err = ownedDaemonArtifactsGone(socketPath, socketInfo, pidPath, pidInfo)
		if err != nil {
			t.Fatalf("ownedDaemonArtifactsGone() after removal error = %v", err)
		}
		if !gone {
			t.Error("ownedDaemonArtifactsGone() = false after both artifacts were removed")
		}
	})
}

type daemonControllerTestProcess struct {
	signal  func(os.Signal) error
	signals []os.Signal
}

func (process *daemonControllerTestProcess) Signal(signal os.Signal) error {
	process.signals = append(process.signals, signal)
	if process.signal == nil {
		return nil
	}
	return process.signal(signal)
}

type daemonControllerTestChild struct {
	daemonControllerTestProcess
	pid      int
	exited   chan error
	exitOnce sync.Once
}

func newDaemonControllerTestChild(pid int) *daemonControllerTestChild {
	return &daemonControllerTestChild{pid: pid, exited: make(chan error)}
}

func (child *daemonControllerTestChild) PID() int {
	return child.pid
}

func (child *daemonControllerTestChild) Signal(signal os.Signal) error {
	err := child.daemonControllerTestProcess.Signal(signal)
	if err == nil {
		child.exitOnce.Do(func() {
			close(child.exited)
		})
	}
	return err
}

func (child *daemonControllerTestChild) Exited() <-chan error {
	return child.exited
}

func newDaemonControllerTestCore(t *testing.T) (string, *daemoncore.Daemon) {
	t.Helper()

	dataDir, err := os.MkdirTemp("", "cronmate-cli-")
	if err != nil {
		t.Fatalf("create short daemon data directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dataDir); err != nil {
			t.Errorf("remove daemon data directory: %v", err)
		}
	})

	core, err := daemoncore.New(daemoncore.Options{
		DataDir:         dataDir,
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("daemon.New() error = %v", err)
	}
	return dataDir, core
}

func newDaemonControllerTestController(
	dataDir string,
	core *daemoncore.Daemon,
) *systemDaemonController {
	return &systemDaemonController{
		dataDir:      dataDir,
		socketPath:   core.SocketPath(),
		pidPath:      core.PIDPath(),
		logPath:      filepath.Join(dataDir, "logs", "daemon.log"),
		newClient:    func(path string) (daemonStatusClient, error) { return daemoncore.NewClient(path) },
		startTimeout: time.Second,
		stopTimeout:  time.Second,
		pollInterval: time.Millisecond,
		requestLimit: 250 * time.Millisecond,
	}
}

func startDaemonControllerTestCore(t *testing.T, core *daemoncore.Daemon) {
	t.Helper()
	if err := core.Start(); err != nil {
		t.Fatalf("daemon Start() error = %v", err)
	}
	t.Cleanup(func() {
		shutdownDaemonControllerTestCore(t, core)
	})
}

func shutdownDaemonControllerTestCore(t *testing.T, core *daemoncore.Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := core.Shutdown(ctx); err != nil {
		t.Errorf("daemon Shutdown() error = %v", err)
	}
}

func assertDaemonControllerTestPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Lstat(%q) error = %v, want os.ErrNotExist", path, err)
	}
}

func daemonControllerTestFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
		t.Fatalf("write test artifact: %v", err)
	}
	return path
}

func daemonControllerTestPathInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat test artifact: %v", err)
	}
	return info
}
