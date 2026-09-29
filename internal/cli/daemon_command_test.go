package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	daemoncore "cronmate/internal/daemon"

	"github.com/spf13/cobra"
)

func TestDaemonCommandTreeIsNested(t *testing.T) {
	factory := &recordingDaemonControllerFactory{controller: &recordingDaemonController{}}
	root := newDaemonTestRoot(factory.new)

	for _, path := range [][]string{
		{"daemon"},
		{"daemon", "start"},
		{"daemon", "status"},
		{"daemon", "stop"},
		{"daemon", "kill"},
		{"daemon", "restart"},
	} {
		command, remaining, err := root.Find(path)
		if err != nil {
			t.Fatalf("Find(%v) error = %v", path, err)
		}
		if len(remaining) != 0 {
			t.Fatalf("Find(%v) remaining args = %v, want none", path, remaining)
		}
		if got, want := command.CommandPath(), "cronmate "+strings.Join(path, " "); got != want {
			t.Fatalf("Find(%v) command path = %q, want %q", path, got, want)
		}
	}

	if factory.calls != 0 {
		t.Fatalf("inspecting the command tree constructed a controller %d times", factory.calls)
	}
}

func TestDaemonWithoutActionShowsHelpWithoutConstructingController(t *testing.T) {
	factory := &recordingDaemonControllerFactory{
		err: errors.New("controller must not be constructed"),
	}

	stdout, stderr, err := executeDaemonCommand(factory.new)
	if err != nil {
		t.Fatalf("daemon command error = %v", err)
	}
	if stderr != "" {
		t.Fatalf("daemon command stderr = %q, want empty", stderr)
	}
	for _, text := range []string{"Usage:", "start", "status", "stop", "kill", "restart"} {
		if !strings.Contains(stdout, text) {
			t.Errorf("daemon help output %q does not contain %q", stdout, text)
		}
	}
	lastPosition := -1
	for _, name := range []string{"status", "start", "restart", "stop", "kill"} {
		position := strings.Index(stdout, "\n  "+name+" ")
		if position == -1 {
			t.Fatalf("daemon help output does not list command %q: %q", name, stdout)
		}
		if position <= lastPosition {
			t.Fatalf("daemon help command %q is out of order: %q", name, stdout)
		}
		lastPosition = position
	}
	if factory.calls != 0 {
		t.Fatalf("bare daemon command constructed a controller %d times", factory.calls)
	}
}

func TestDaemonStartDispatchesBackgroundAndForegroundModes(t *testing.T) {
	startedAt := time.Date(2026, time.September, 29, 9, 8, 7, 0, time.FixedZone("PDT", -7*60*60))

	tests := []struct {
		name       string
		args       []string
		wantCalls  []string
		wantOutput string
	}{
		{
			name:       "background",
			args:       []string{"start"},
			wantCalls:  []string{"start"},
			wantOutput: "running pid=4321 started_at=2026-09-29T16:08:07Z\n",
		},
		{
			name:      "foreground long flag",
			args:      []string{"start", "--foreground"},
			wantCalls: []string{"run"},
		},
		{
			name:      "foreground short flag",
			args:      []string{"start", "-f"},
			wantCalls: []string{"run"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &recordingDaemonController{
				startStatus: daemoncore.Status{
					State:     daemoncore.StateRunning,
					PID:       4321,
					StartedAt: startedAt,
				},
			}
			factory := &recordingDaemonControllerFactory{controller: controller}

			stdout, stderr, err := executeDaemonCommand(factory.new, test.args...)
			if err != nil {
				t.Fatalf("daemon %v error = %v", test.args, err)
			}
			if stdout != test.wantOutput || stderr != "" {
				t.Fatalf(
					"daemon %v output = (%q, %q), want (%q, empty)",
					test.args,
					stdout,
					stderr,
					test.wantOutput,
				)
			}
			assertDaemonCalls(t, controller, test.wantCalls...)
			if factory.calls != 1 {
				t.Fatalf("daemon %v factory calls = %d, want 1", test.args, factory.calls)
			}
		})
	}
}

func TestDaemonStatusWritesStatus(t *testing.T) {
	tests := []struct {
		name       string
		status     daemoncore.Status
		wantOutput string
	}{
		{
			name: "running",
			status: daemoncore.Status{
				State:     daemoncore.StateRunning,
				PID:       91,
				StartedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
			},
			wantOutput: "running pid=91 started_at=2026-01-02T03:04:05Z\n",
		},
		{
			name: "stopped",
			status: daemoncore.Status{
				State: daemoncore.StateStopped,
			},
			wantOutput: "stopped pid=0 started_at=-\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &recordingDaemonController{status: test.status}
			factory := &recordingDaemonControllerFactory{controller: controller}

			stdout, stderr, err := executeDaemonCommand(factory.new, "status")
			if err != nil {
				t.Fatalf("daemon status error = %v", err)
			}
			if stdout != test.wantOutput || stderr != "" {
				t.Fatalf(
					"daemon status output = (%q, %q), want (%q, empty)",
					stdout,
					stderr,
					test.wantOutput,
				)
			}
			assertDaemonCalls(t, controller, "status")
		})
	}
}

func TestRunDaemonStatusWatchRefreshesOnInjectedTicks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	status := daemoncore.Status{
		State:     daemoncore.StateRunning,
		PID:       2468,
		StartedAt: time.Date(2026, time.November, 12, 13, 14, 15, 0, time.UTC),
	}
	controller := &recordingDaemonController{status: status}
	firstTick := newBufferedDaemonTicker()
	secondTick := newBufferedDaemonTicker()
	finalTicker := newControlledDaemonTicker()
	tickers := []*controlledDaemonTicker{firstTick, secondTick, finalTicker}
	factoryCalls := 0
	newTicker := func() daemonTicker {
		if factoryCalls >= len(tickers) {
			t.Fatalf("ticker factory called %d times, want at most %d", factoryCalls+1, len(tickers))
		}
		ticker := tickers[factoryCalls]
		factoryCalls++
		if factoryCalls == len(tickers) {
			cancel()
		}
		return ticker
	}
	output := bytes.Buffer{}

	if err := runDaemonStatus(ctx, &output, controller, true, newTicker); err != nil {
		t.Fatalf("runDaemonStatus(watch=true) error = %v", err)
	}

	wantLine := "running pid=2468 started_at=2026-11-12T13:14:15Z\n"
	if got, want := output.String(), strings.Repeat(wantLine, 3); got != want {
		t.Fatalf("watch output = %q, want %q", got, want)
	}
	assertDaemonCalls(t, controller, "status", "status", "status")
	if factoryCalls != 3 {
		t.Fatalf("ticker factory calls = %d, want 3", factoryCalls)
	}
	for index, ticker := range tickers {
		if ticker.stopCalls != 1 {
			t.Errorf("ticker %d Stop calls = %d, want 1", index, ticker.stopCalls)
		}
	}
}

func TestRunDaemonStatusWatchCancellationReturnsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	controller := &recordingDaemonController{
		status: daemoncore.Status{State: daemoncore.StateRunning, PID: 7},
	}
	ticker := newControlledDaemonTicker()
	factoryCalls := 0
	newTicker := func() daemonTicker {
		factoryCalls++
		cancel()
		return ticker
	}
	output := bytes.Buffer{}

	if err := runDaemonStatus(ctx, &output, controller, true, newTicker); err != nil {
		t.Fatalf("runDaemonStatus(watch=true) after cancellation error = %v", err)
	}
	if got, want := output.String(), "running pid=7 started_at=-\n"; got != want {
		t.Fatalf("watch output = %q, want %q", got, want)
	}
	assertDaemonCalls(t, controller, "status")
	if factoryCalls != 1 {
		t.Fatalf("ticker factory calls = %d, want 1", factoryCalls)
	}
	if ticker.stopCalls != 1 {
		t.Fatalf("ticker Stop calls = %d, want 1", ticker.stopCalls)
	}
}

func TestDaemonStopAndKillDispatchStop(t *testing.T) {
	for _, name := range []string{"stop", "kill"} {
		t.Run(name, func(t *testing.T) {
			controller := &recordingDaemonController{}
			factory := &recordingDaemonControllerFactory{controller: controller}

			stdout, stderr, err := executeDaemonCommand(factory.new, name)
			if err != nil {
				t.Fatalf("daemon %s error = %v", name, err)
			}
			if stdout != "stopped\n" || stderr != "" {
				t.Fatalf("daemon %s output = (%q, %q), want (%q, empty)", name, stdout, stderr, "stopped\n")
			}
			assertDaemonCalls(t, controller, "stop")
			if factory.calls != 1 {
				t.Fatalf("daemon %s factory calls = %d, want 1", name, factory.calls)
			}
		})
	}
}

func TestDaemonRestartOrdersStopBeforeReplacement(t *testing.T) {
	status := daemoncore.Status{
		State:     daemoncore.StateRunning,
		PID:       987,
		StartedAt: time.Date(2026, time.October, 1, 2, 3, 4, 0, time.UTC),
	}

	tests := []struct {
		name       string
		args       []string
		stopErr    error
		wantCalls  []string
		wantOutput string
	}{
		{
			name:       "background",
			args:       []string{"restart"},
			wantCalls:  []string{"stop", "start"},
			wantOutput: "running pid=987 started_at=2026-10-01T02:03:04Z\n",
		},
		{
			name:      "foreground",
			args:      []string{"restart", "--foreground"},
			wantCalls: []string{"stop", "run"},
		},
		{
			name:       "already stopped",
			args:       []string{"restart"},
			stopErr:    fmt.Errorf("inspect daemon: %w", errDaemonNotRunning),
			wantCalls:  []string{"stop", "start"},
			wantOutput: "running pid=987 started_at=2026-10-01T02:03:04Z\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &recordingDaemonController{
				startStatus: status,
				stopErr:     test.stopErr,
			}
			factory := &recordingDaemonControllerFactory{controller: controller}

			stdout, stderr, err := executeDaemonCommand(factory.new, test.args...)
			if err != nil {
				t.Fatalf("daemon %v error = %v", test.args, err)
			}
			if stdout != test.wantOutput || stderr != "" {
				t.Fatalf(
					"daemon %v output = (%q, %q), want (%q, empty)",
					test.args,
					stdout,
					stderr,
					test.wantOutput,
				)
			}
			assertDaemonCalls(t, controller, test.wantCalls...)
			if factory.calls != 1 {
				t.Fatalf("daemon %v factory calls = %d, want 1", test.args, factory.calls)
			}
		})
	}
}

func TestDaemonRestartStopsOnUnexpectedStopError(t *testing.T) {
	stopErr := errors.New("cannot signal daemon")
	controller := &recordingDaemonController{stopErr: stopErr}
	factory := &recordingDaemonControllerFactory{controller: controller}

	stdout, _, err := executeDaemonCommand(factory.new, "restart")
	if !errors.Is(err, stopErr) {
		t.Fatalf("daemon restart error = %v, want %v", err, stopErr)
	}
	if stdout != "" {
		t.Fatalf("daemon restart output = %q, want empty", stdout)
	}
	assertDaemonCalls(t, controller, "stop")
}

func TestDaemonCommandErrorsAreReturnedWithoutSuccessOutput(t *testing.T) {
	operationErr := errors.New("operation failed")
	tests := []struct {
		name      string
		args      []string
		configure func(*recordingDaemonController)
		wantCalls []string
	}{
		{
			name: "start",
			args: []string{"start"},
			configure: func(controller *recordingDaemonController) {
				controller.startErr = operationErr
			},
			wantCalls: []string{"start"},
		},
		{
			name: "foreground start",
			args: []string{"start", "--foreground"},
			configure: func(controller *recordingDaemonController) {
				controller.runErr = operationErr
			},
			wantCalls: []string{"run"},
		},
		{
			name: "status",
			args: []string{"status"},
			configure: func(controller *recordingDaemonController) {
				controller.statusErr = operationErr
			},
			wantCalls: []string{"status"},
		},
		{
			name: "stop",
			args: []string{"stop"},
			configure: func(controller *recordingDaemonController) {
				controller.stopErr = operationErr
			},
			wantCalls: []string{"stop"},
		},
		{
			name: "restart start",
			args: []string{"restart"},
			configure: func(controller *recordingDaemonController) {
				controller.startErr = operationErr
			},
			wantCalls: []string{"stop", "start"},
		},
		{
			name: "foreground restart run",
			args: []string{"restart", "--foreground"},
			configure: func(controller *recordingDaemonController) {
				controller.runErr = operationErr
			},
			wantCalls: []string{"stop", "run"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &recordingDaemonController{}
			test.configure(controller)
			factory := &recordingDaemonControllerFactory{controller: controller}

			stdout, _, err := executeDaemonCommand(factory.new, test.args...)
			if !errors.Is(err, operationErr) {
				t.Fatalf("daemon %v error = %v, want %v", test.args, err, operationErr)
			}
			if stdout != "" {
				t.Fatalf("daemon %v output = %q, want empty", test.args, stdout)
			}
			assertDaemonCalls(t, controller, test.wantCalls...)
		})
	}
}

func TestDaemonControllerFactoryErrorsAreReturned(t *testing.T) {
	factoryErr := errors.New("construct controller")
	for _, args := range [][]string{
		{"start"},
		{"start", "--foreground"},
		{"status"},
		{"stop"},
		{"kill"},
		{"restart"},
		{"restart", "--foreground"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			factory := &recordingDaemonControllerFactory{err: factoryErr}

			stdout, _, err := executeDaemonCommand(factory.new, args...)
			if !errors.Is(err, factoryErr) {
				t.Fatalf("daemon %v error = %v, want %v", args, err, factoryErr)
			}
			if stdout != "" {
				t.Fatalf("daemon %v output = %q, want empty", args, stdout)
			}
			if factory.calls != 1 {
				t.Fatalf("daemon %v factory calls = %d, want 1", args, factory.calls)
			}
		})
	}
}

func TestDaemonCommandsPropagateOutputErrors(t *testing.T) {
	writeErr := errors.New("cannot write output")
	tests := []struct {
		name      string
		args      []string
		wantCalls []string
	}{
		{name: "start", args: []string{"start"}, wantCalls: []string{"start"}},
		{name: "status", args: []string{"status"}, wantCalls: []string{"status"}},
		{name: "stop", args: []string{"stop"}, wantCalls: []string{"stop"}},
		{name: "restart", args: []string{"restart"}, wantCalls: []string{"stop", "start"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &recordingDaemonController{
				startStatus: daemoncore.Status{State: daemoncore.StateRunning, PID: 12},
				status:      daemoncore.Status{State: daemoncore.StateRunning, PID: 12},
			}
			factory := &recordingDaemonControllerFactory{controller: controller}
			command := newCmdDaemon(factory.new)
			command.SetArgs(test.args)
			command.SetOut(errorWriter{err: writeErr})
			command.SetErr(io.Discard)
			command.SilenceErrors = true
			command.SilenceUsage = true

			err := command.Execute()
			if !errors.Is(err, writeErr) {
				t.Fatalf("daemon %v error = %v, want %v", test.args, err, writeErr)
			}
			assertDaemonCalls(t, controller, test.wantCalls...)
		})
	}
}

func TestDaemonLeafCommandsRejectArgumentsWithoutConstructingController(t *testing.T) {
	for _, args := range [][]string{
		{"start", "unexpected"},
		{"status", "unexpected"},
		{"stop", "unexpected"},
		{"kill", "unexpected"},
		{"restart", "unexpected"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			factory := &recordingDaemonControllerFactory{
				controller: &recordingDaemonController{},
			}

			_, _, err := executeDaemonCommand(factory.new, args...)
			if err == nil {
				t.Fatalf("daemon %v error = nil, want argument error", args)
			}
			if factory.calls != 0 {
				t.Fatalf("daemon %v constructed a controller %d times", args, factory.calls)
			}
		})
	}
}

func TestDaemonLifecycleCommandsAreNotAvailableAtRoot(t *testing.T) {
	for _, name := range []string{"start", "status", "stop", "kill", "restart"} {
		t.Run(name, func(t *testing.T) {
			factory := &recordingDaemonControllerFactory{
				controller: &recordingDaemonController{},
			}
			root := newDaemonTestRoot(factory.new)
			stdout := bytes.Buffer{}
			stderr := bytes.Buffer{}
			root.SetArgs([]string{name})
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SilenceErrors = true
			root.SilenceUsage = true

			err := root.Execute()
			if err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("cronmate %s error = %v, want unknown-command error", name, err)
			}
			if factory.calls != 0 {
				t.Fatalf("cronmate %s constructed a daemon controller %d times", name, factory.calls)
			}
		})
	}
}

func TestDaemonCommandsPassCommandContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "command context")
	controller := &recordingDaemonController{
		startStatus: daemoncore.Status{State: daemoncore.StateRunning},
	}
	factory := &recordingDaemonControllerFactory{controller: controller}
	command := newCmdDaemon(factory.new)
	command.SetArgs([]string{"restart"})
	command.SetContext(ctx)
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SilenceErrors = true
	command.SilenceUsage = true

	if err := command.Execute(); err != nil {
		t.Fatalf("daemon restart error = %v", err)
	}
	if len(controller.contexts) != 2 {
		t.Fatalf("daemon restart contexts = %d, want 2", len(controller.contexts))
	}
	for index, got := range controller.contexts {
		if got.Value(contextKey{}) != "command context" {
			t.Errorf("daemon restart context %d did not preserve the command context", index)
		}
	}
}

func TestResolveDaemonDataDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "")

	tests := []struct {
		name       string
		configured string
		want       string
		wantError  string
	}{
		{
			name:       "absolute path is trimmed and cleaned",
			configured: "  /var/lib/cronmate/../cronmate-runtime  ",
			want:       "/var/lib/cronmate-runtime",
		},
		{
			name:       "home directory",
			configured: "~",
			want:       homeDir,
		},
		{
			name:       "path below home directory",
			configured: "~/var/../cronmate",
			want:       filepath.Join(homeDir, "cronmate"),
		},
		{
			name: "empty",
			want: filepath.Join(homeDir, ".local", "share", "cronmate"),
		},
		{
			name:       "whitespace",
			configured: " \t\n ",
			want:       filepath.Join(homeDir, ".local", "share", "cronmate"),
		},
		{
			name:       "relative",
			configured: "var/cronmate",
			wantError:  "must be absolute or start with ~/",
		},
		{
			name:       "named user home",
			configured: "~someone/cronmate",
			wantError:  "unsupported home syntax",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDaemonDataDir(test.configured)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("resolveDaemonDataDir(%q) error = %v, want containing %q", test.configured, err, test.wantError)
				}
				if got != "" {
					t.Fatalf("resolveDaemonDataDir(%q) = %q with error, want empty", test.configured, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveDaemonDataDir(%q) error = %v", test.configured, err)
			}
			if got != test.want {
				t.Fatalf("resolveDaemonDataDir(%q) = %q, want %q", test.configured, got, test.want)
			}
		})
	}
}

func TestResolveDaemonDataDirUsesXDGDataHomeForEmptyConfig(t *testing.T) {
	homeDir := t.TempDir()
	xdgDataHome := filepath.Join(t.TempDir(), "xdg", "..", "data")
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", xdgDataHome)

	got, err := resolveDaemonDataDir("")
	if err != nil {
		t.Fatalf("resolveDaemonDataDir(\"\") error = %v", err)
	}
	want := filepath.Join(filepath.Clean(xdgDataHome), "cronmate")
	if got != want {
		t.Fatalf("resolveDaemonDataDir(\"\") = %q, want %q", got, want)
	}
}

func TestResolveDaemonDataDirIgnoresRelativeXDGDataHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("XDG_DATA_HOME", "relative/data")

	got, err := resolveDaemonDataDir("")
	if err != nil {
		t.Fatalf("resolveDaemonDataDir(\"\") error = %v", err)
	}
	want := filepath.Join(homeDir, ".local", "share", "cronmate")
	if got != want {
		t.Fatalf("resolveDaemonDataDir(\"\") = %q, want %q", got, want)
	}
}

func TestResolveDaemonDataDirExplicitPathOverridesXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg-data"))
	explicit := filepath.Join(t.TempDir(), "explicit-data")

	got, err := resolveDaemonDataDir(explicit)
	if err != nil {
		t.Fatalf("resolveDaemonDataDir(%q) error = %v", explicit, err)
	}
	if got != explicit {
		t.Fatalf("resolveDaemonDataDir(%q) = %q, want explicit path", explicit, got)
	}
}

type recordingDaemonControllerFactory struct {
	controller daemonController
	err        error
	calls      int
}

func (factory *recordingDaemonControllerFactory) new() (daemonController, error) {
	factory.calls++
	if factory.err != nil {
		return nil, factory.err
	}
	return factory.controller, nil
}

type recordingDaemonController struct {
	startStatus daemoncore.Status
	status      daemoncore.Status
	startErr    error
	runErr      error
	statusErr   error
	stopErr     error
	calls       []string
	contexts    []context.Context
}

func (controller *recordingDaemonController) Start(ctx context.Context) (daemoncore.Status, error) {
	controller.record("start", ctx)
	return controller.startStatus, controller.startErr
}

func (controller *recordingDaemonController) Run(ctx context.Context) error {
	controller.record("run", ctx)
	return controller.runErr
}

func (controller *recordingDaemonController) Status(ctx context.Context) (daemoncore.Status, error) {
	controller.record("status", ctx)
	return controller.status, controller.statusErr
}

func (controller *recordingDaemonController) Stop(ctx context.Context) error {
	controller.record("stop", ctx)
	return controller.stopErr
}

func (controller *recordingDaemonController) record(operation string, ctx context.Context) {
	controller.calls = append(controller.calls, operation)
	controller.contexts = append(controller.contexts, ctx)
}

func assertDaemonCalls(t *testing.T, controller *recordingDaemonController, want ...string) {
	t.Helper()

	if got := strings.Join(controller.calls, ","); got != strings.Join(want, ",") {
		t.Fatalf("daemon controller calls = %v, want %v", controller.calls, want)
	}
}

func executeDaemonCommand(
	newController daemonControllerFactory,
	args ...string,
) (string, string, error) {
	command := newCmdDaemon(newController)
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	command.SetArgs(args)
	command.SetIn(strings.NewReader(""))
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SilenceErrors = true
	command.SilenceUsage = true

	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

func newDaemonTestRoot(newController daemonControllerFactory) *cobra.Command {
	root := newCmdRoot()
	root.AddCommand(NewCmdConfig(), newCmdDaemon(newController))
	return root
}

type errorWriter struct {
	err error
}

func (writer errorWriter) Write(_ []byte) (int, error) {
	return 0, writer.err
}

type controlledDaemonTicker struct {
	ticks     chan time.Time
	stopCalls int
}

func newControlledDaemonTicker() *controlledDaemonTicker {
	return &controlledDaemonTicker{ticks: make(chan time.Time, 1)}
}

func newBufferedDaemonTicker() *controlledDaemonTicker {
	ticker := newControlledDaemonTicker()
	ticker.ticks <- time.Time{}
	return ticker
}

func (ticker *controlledDaemonTicker) Chan() <-chan time.Time {
	return ticker.ticks
}

func (ticker *controlledDaemonTicker) Stop() {
	ticker.stopCalls++
}
