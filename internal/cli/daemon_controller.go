package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	daemoncore "cronmate/internal/daemon"
)

const (
	daemonStartTimeout   = 5 * time.Second
	daemonStopTimeout    = 15 * time.Second
	daemonChildTermWait  = 12 * time.Second
	daemonChildKillWait  = 2 * time.Second
	daemonRequestTimeout = 500 * time.Millisecond
	daemonPollInterval   = 50 * time.Millisecond
)

type daemonRunner interface {
	Run(context.Context) error
}

type daemonStatusClient interface {
	Status(context.Context) (daemoncore.Status, error)
	Shutdown(context.Context, string) error
	CloseIdleConnections()
}

type daemonChild interface {
	Signal(os.Signal) error
	PID() int
	Exited() <-chan error
}

type systemDaemonController struct {
	dataDir    string
	socketPath string
	pidPath    string
	logPath    string

	newRunner    func() (daemonRunner, error)
	newClient    func(string) (daemonStatusClient, error)
	launch       func(string) (daemonChild, error)
	startTimeout time.Duration
	stopTimeout  time.Duration
	pollInterval time.Duration
	requestLimit time.Duration
}

type daemonSnapshot struct {
	status     daemoncore.Status
	socketInfo fs.FileInfo
	pidInfo    fs.FileInfo
}

func newSystemDaemonController() (daemonController, error) {
	dataDir, err := configuredDaemonDataDir()
	if err != nil {
		return nil, err
	}

	prototype, err := daemoncore.New(daemoncore.Options{DataDir: dataDir})
	if err != nil {
		return nil, err
	}

	controller := &systemDaemonController{
		dataDir:    dataDir,
		socketPath: prototype.SocketPath(),
		pidPath:    prototype.PIDPath(),
		logPath:    filepath.Join(dataDir, "logs", "daemon.log"),
		newRunner: func() (daemonRunner, error) {
			return daemoncore.New(daemoncore.Options{DataDir: dataDir})
		},
		newClient: func(socketPath string) (daemonStatusClient, error) {
			return daemoncore.NewClient(socketPath)
		},
		launch:       launchBackgroundDaemon,
		startTimeout: daemonStartTimeout,
		stopTimeout:  daemonStopTimeout,
		pollInterval: daemonPollInterval,
		requestLimit: daemonRequestTimeout,
	}
	return controller, nil
}

func (controller *systemDaemonController) Start(ctx context.Context) (daemoncore.Status, error) {
	if ctx == nil {
		return daemoncore.Status{}, errors.New("daemon start context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return daemoncore.Status{}, err
	}
	if _, err := controller.Status(ctx); err == nil {
		return daemoncore.Status{}, daemoncore.ErrAlreadyRunning
	} else if !errors.Is(err, errDaemonNotRunning) {
		return daemoncore.Status{}, err
	}
	if err := ctx.Err(); err != nil {
		return daemoncore.Status{}, err
	}

	child, err := controller.launch(controller.dataDir)
	if err != nil {
		return daemoncore.Status{}, err
	}

	return controller.waitUntilStarted(ctx, child)
}

func (controller *systemDaemonController) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("daemon run context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runner, err := controller.newRunner()
	if err != nil {
		return err
	}

	runContext, stopSignals, err := daemonSignalContext(ctx)
	if err != nil {
		return err
	}
	defer stopSignals()
	return runner.Run(runContext)
}

func (controller *systemDaemonController) Status(ctx context.Context) (daemoncore.Status, error) {
	if ctx == nil {
		return daemoncore.Status{}, errors.New("daemon status context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return daemoncore.Status{}, err
	}
	snapshot, err := controller.inspect(ctx)
	if err != nil {
		return daemoncore.Status{}, err
	}
	return snapshot.status, nil
}

func (controller *systemDaemonController) Stop(ctx context.Context) error {
	if ctx == nil {
		return errors.New("daemon stop context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snapshot, err := controller.inspect(ctx)
	if err != nil {
		return err
	}
	if snapshot.status.State == daemoncore.StateRunning {
		confirmed, err := controller.inspect(ctx)
		if err != nil {
			return err
		}
		if confirmed.status.PID != snapshot.status.PID ||
			confirmed.status.InstanceID != snapshot.status.InstanceID ||
			!os.SameFile(confirmed.socketInfo, snapshot.socketInfo) ||
			!os.SameFile(confirmed.pidInfo, snapshot.pidInfo) {
			return errors.New("daemon identity changed before it could be stopped")
		}

		client, err := controller.newClient(controller.socketPath)
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
		requestContext, cancel := context.WithTimeout(ctx, controller.requestLimit)
		defer cancel()
		if err := client.Shutdown(requestContext, snapshot.status.InstanceID); err != nil {
			return err
		}
	} else if snapshot.status.State != daemoncore.StateStopping {
		return fmt.Errorf("cannot stop daemon in state %q", snapshot.status.State)
	}

	stopContext, cancel := context.WithTimeout(ctx, controller.stopTimeout)
	defer cancel()
	return controller.waitUntilStopped(stopContext, snapshot)
}

func (controller *systemDaemonController) inspect(ctx context.Context) (daemonSnapshot, error) {
	socketInfo, err := os.Lstat(controller.socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return daemonSnapshot{}, errDaemonNotRunning
	}
	if err != nil {
		return daemonSnapshot{}, fmt.Errorf("inspect daemon socket %q: %w", controller.socketPath, err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 {
		return daemonSnapshot{}, fmt.Errorf("daemon socket path %q is not a unix socket", controller.socketPath)
	}

	client, err := controller.newClient(controller.socketPath)
	if err != nil {
		return daemonSnapshot{}, err
	}
	defer client.CloseIdleConnections()

	requestContext, cancel := context.WithTimeout(ctx, controller.requestLimit)
	defer cancel()
	status, err := client.Status(requestContext)
	if err != nil {
		if daemonConnectionUnavailable(err) {
			return daemonSnapshot{}, fmt.Errorf("%w: %v", errDaemonNotRunning, err)
		}
		return daemonSnapshot{}, err
	}

	// Only inspect the pidfile after proving the socket is live. A malformed
	// stale pidfile must not prevent Start from reaching the core's stale-file
	// recovery path.
	pid, pidInfo, err := readDaemonPID(controller.pidPath)
	if err != nil {
		return daemonSnapshot{}, err
	}
	if status.PID != pid {
		return daemonSnapshot{}, fmt.Errorf(
			"daemon identity mismatch: socket reports pid %d, pid file reports %d",
			status.PID,
			pid,
		)
	}
	if status.InstanceID == "" {
		return daemonSnapshot{}, errors.New("daemon status omitted its instance id")
	}
	if status.State != daemoncore.StateRunning && status.State != daemoncore.StateStopping {
		return daemonSnapshot{}, fmt.Errorf("daemon reported unexpected state %q", status.State)
	}

	if err := requireSamePath(controller.socketPath, socketInfo); err != nil {
		return daemonSnapshot{}, err
	}
	if err := requireSamePath(controller.pidPath, pidInfo); err != nil {
		return daemonSnapshot{}, err
	}

	return daemonSnapshot{status: status, socketInfo: socketInfo, pidInfo: pidInfo}, nil
}

func (controller *systemDaemonController) waitUntilStarted(
	ctx context.Context,
	child daemonChild,
) (daemoncore.Status, error) {
	waitContext, cancel := context.WithTimeout(ctx, controller.startTimeout)
	defer cancel()
	ticker := time.NewTicker(controller.pollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		status, err := controller.Status(waitContext)
		if err == nil {
			if status.PID != child.PID() {
				startErr := fmt.Errorf(
					"%w: socket belongs to pid %d, started child pid %d",
					daemoncore.ErrAlreadyRunning,
					status.PID,
					child.PID(),
				)
				return daemoncore.Status{}, errors.Join(startErr, stopChild(child))
			}
			if status.State != daemoncore.StateRunning {
				lastErr = fmt.Errorf("daemon reported state %q while starting", status.State)
			} else {
				if err := ctx.Err(); err != nil {
					return daemoncore.Status{}, errors.Join(err, stopChild(child))
				}
				return status, nil
			}
		} else {
			lastErr = err
		}

		select {
		case exitErr := <-child.Exited():
			if exitErr == nil {
				exitErr = errors.New("process exited successfully before becoming ready")
			}
			return daemoncore.Status{}, fmt.Errorf(
				"daemon process exited before becoming ready: %w (see %s)",
				exitErr,
				controller.logPath,
			)
		case <-waitContext.Done():
			if err := ctx.Err(); err != nil {
				return daemoncore.Status{}, errors.Join(err, stopChild(child))
			}
			startErr := fmt.Errorf(
				"daemon did not become ready within %s: %v (see %s)",
				controller.startTimeout,
				lastErr,
				controller.logPath,
			)
			return daemoncore.Status{}, errors.Join(startErr, stopChild(child))
		case <-ticker.C:
		}
	}
}

func (controller *systemDaemonController) waitUntilStopped(
	ctx context.Context,
	snapshot daemonSnapshot,
) error {
	ticker := time.NewTicker(controller.pollInterval)
	defer ticker.Stop()

	for {
		gone, err := ownedDaemonArtifactsGone(
			controller.socketPath,
			snapshot.socketInfo,
			controller.pidPath,
			snapshot.pidInfo,
		)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for daemon shutdown: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func readDaemonPID(path string) (int, fs.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil, errDaemonNotRunning
	}
	if err != nil {
		return 0, nil, fmt.Errorf("inspect daemon pid file %q: %w", path, err)
	}
	if !pathInfo.Mode().IsRegular() {
		return 0, nil, fmt.Errorf("daemon pid path %q is not a regular file", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("open daemon pid file %q: %w", path, err)
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return 0, nil, fmt.Errorf("inspect open daemon pid file %q: %w", path, err)
	}
	if !os.SameFile(pathInfo, fileInfo) {
		return 0, nil, fmt.Errorf("daemon pid file %q changed while opening it", path)
	}

	contents, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		return 0, nil, fmt.Errorf("read daemon pid file %q: %w", path, err)
	}
	if len(contents) > 64 {
		return 0, nil, fmt.Errorf("daemon pid file %q is too large", path)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(contents)))
	if err != nil || pid <= 1 {
		return 0, nil, fmt.Errorf("daemon pid file %q contains invalid pid %q", path, strings.TrimSpace(string(contents)))
	}
	return pid, fileInfo, nil
}

func requireSamePath(path string, expected fs.FileInfo) error {
	current, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("recheck daemon path %q: %w", path, err)
	}
	if !os.SameFile(expected, current) {
		return fmt.Errorf("daemon path %q changed during inspection", path)
	}
	return nil
}

func ownedDaemonArtifactsGone(
	socketPath string,
	socketInfo fs.FileInfo,
	pidPath string,
	pidInfo fs.FileInfo,
) (bool, error) {
	socketGone, err := ownedPathGone(socketPath, socketInfo)
	if err != nil {
		return false, err
	}
	pidGone, err := ownedPathGone(pidPath, pidInfo)
	if err != nil {
		return false, err
	}
	return socketGone && pidGone, nil
}

func ownedPathGone(path string, expected fs.FileInfo) (bool, error) {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect daemon path %q while stopping: %w", path, err)
	}
	if !os.SameFile(expected, current) {
		return false, fmt.Errorf("daemon path %q was replaced while stopping", path)
	}
	return false, nil
}

func daemonConnectionUnavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

type commandDaemonChild struct {
	command *exec.Cmd
	exited  <-chan error
}

func (child commandDaemonChild) PID() int {
	return child.command.Process.Pid
}

func (child commandDaemonChild) Signal(signal os.Signal) error {
	return child.command.Process.Signal(signal)
}

func (child commandDaemonChild) Exited() <-chan error {
	return child.exited
}

func launchBackgroundDaemon(dataDir string) (daemonChild, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve cronmate executable: %w", err)
	}

	logFile, err := openDaemonLog(dataDir)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	input, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("open null input: %w", err)
	}
	defer input.Close()

	command := exec.Command(executable, "daemon", "start", "--foreground")
	command.Stdin = input
	command.Stdout = logFile
	command.Stderr = logFile
	if err := configureDetachedDaemonProcess(command); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start daemon process: %w", err)
	}

	exited := make(chan error, 1)
	go func() {
		exited <- command.Wait()
		close(exited)
	}()

	return commandDaemonChild{command: command, exited: exited}, nil
}

func openDaemonLog(dataDir string) (*os.File, error) {
	if err := ensurePrivateDirectory(dataDir); err != nil {
		return nil, err
	}
	logDir := filepath.Join(dataDir, "logs")
	if err := ensurePrivateDirectory(logDir); err != nil {
		return nil, err
	}

	logPath := filepath.Join(logDir, "daemon.log")
	if info, err := os.Lstat(logPath); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("daemon log path %q is not a regular file", logPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect daemon log path %q: %w", logPath, err)
	}

	file, err := openDaemonLogFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("open daemon log %q: %w", logPath, err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect open daemon log %q: %w", logPath, err)
	}
	if !fileInfo.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("opened daemon log path %q is not a regular file", logPath)
	}
	pathInfo, err := os.Lstat(logPath)
	if err != nil || !os.SameFile(fileInfo, pathInfo) {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("verify daemon log path %q: %w", logPath, err)
		}
		return nil, fmt.Errorf("daemon log path %q changed while opening it", logPath)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("set daemon log permissions on %q: %w", logPath, err)
	}
	return file, nil
}

func ensurePrivateDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("daemon directory %q cannot be a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("daemon directory %q is not a directory", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect daemon directory %q: %w", path, err)
	}

	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create daemon directory %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set daemon directory permissions on %q: %w", path, err)
	}
	return nil
}

func signalChild(child daemonChild) error {
	signal, err := daemonTerminationSignal()
	if err != nil {
		return err
	}
	return child.Signal(signal)
}

func stopChild(child daemonChild) error {
	signalErr := signalChild(child)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}

	timer := time.NewTimer(daemonChildTermWait)
	defer timer.Stop()
	select {
	case <-child.Exited():
		return signalErr
	case <-timer.C:
	}

	killSignal, err := daemonKillSignal()
	if err != nil {
		return errors.Join(signalErr, err)
	}
	killErr := child.Signal(killSignal)
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}

	killTimer := time.NewTimer(daemonChildKillWait)
	defer killTimer.Stop()
	select {
	case <-child.Exited():
		return errors.Join(
			signalErr,
			killErr,
			errors.New("started daemon process required SIGKILL after SIGTERM timed out"),
		)
	case <-killTimer.C:
		return errors.Join(
			signalErr,
			killErr,
			errors.New("started daemon process did not exit after SIGKILL"),
		)
	}
}
