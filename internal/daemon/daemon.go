// Package daemon provides cronmate's process lifecycle and local HTTP server.
//
// The daemon listens only on a filesystem-backed Unix domain socket. Process
// startup and signal handling intentionally live outside this package so the
// core can be embedded and tested without owning global process state.
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// StatusEndpoint is served by every Daemon and cannot be overridden by the
	// application handler.
	StatusEndpoint = "/api/status"
	// ShutdownEndpoint accepts generation-bound shutdown requests from the
	// local CLI over the protected Unix socket.
	ShutdownEndpoint = "/api/shutdown"
	// InstanceIDHeader carries the expected daemon generation for shutdown.
	InstanceIDHeader = "X-Cronmate-Instance-ID"

	defaultSocketName      = "cronmate.sock"
	defaultPIDName         = "cronmate.pid"
	defaultShutdownTimeout = 10 * time.Second
)

var (
	// ErrAlreadyRunning means another process owns the data directory's daemon
	// lock or is accepting connections on its socket.
	ErrAlreadyRunning = errors.New("cronmate daemon is already running")

	// ErrNotRunning is returned by Wait when this Daemon has not been started.
	ErrNotRunning = errors.New("cronmate daemon is not running")
)

// State is the daemon's lifecycle state.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// Options configures a Daemon. DataDir must be an absolute path. Handler may
// serve application APIs; /api/status is always reserved for the daemon.
type Options struct {
	DataDir         string
	Handler         http.Handler
	ShutdownTimeout time.Duration
}

// Status is the response returned by GET /api/status.
type Status struct {
	State      State     `json:"state"`
	PID        int       `json:"pid"`
	InstanceID string    `json:"instance_id"`
	StartedAt  time.Time `json:"started_at"`
}

// Daemon owns one pid lock, Unix socket, and HTTP server. A Daemon may be
// started again after a complete shutdown.
type Daemon struct {
	dataDir         string
	socketPath      string
	pidPath         string
	application     http.Handler
	shutdownTimeout time.Duration

	mu      sync.RWMutex
	state   State
	current *daemonRun
	last    *daemonRun

	shutdownMu sync.Mutex
}

// daemonRun contains resources belonging to one successful Start call. Keeping
// them together prevents a delayed Wait or Shutdown from acting on a later
// restart of the same Daemon.
type daemonRun struct {
	instanceID string
	startedAt  time.Time
	pidFile    *os.File
	pidInfo    fs.FileInfo
	listener   *net.UnixListener
	socketInfo fs.FileInfo
	server     *http.Server
	done       chan struct{}

	mu          sync.RWMutex
	serveErr    error
	shutdownErr error
}

// New constructs a daemon without touching the filesystem.
func New(options Options) (*Daemon, error) {
	if strings.TrimSpace(options.DataDir) == "" {
		return nil, errors.New("daemon data directory cannot be empty")
	}
	if !filepath.IsAbs(options.DataDir) {
		return nil, fmt.Errorf("daemon data directory must be absolute: %q", options.DataDir)
	}

	dataDir := filepath.Clean(options.DataDir)
	if filepath.Dir(dataDir) == dataDir {
		return nil, fmt.Errorf("daemon data directory cannot be a filesystem root: %q", dataDir)
	}

	shutdownTimeout := options.ShutdownTimeout
	if shutdownTimeout < 0 {
		return nil, errors.New("daemon shutdown timeout cannot be negative")
	}
	if shutdownTimeout == 0 {
		shutdownTimeout = defaultShutdownTimeout
	}

	daemon := &Daemon{
		dataDir:         dataDir,
		socketPath:      filepath.Join(dataDir, defaultSocketName),
		pidPath:         filepath.Join(dataDir, defaultPIDName),
		application:     options.Handler,
		shutdownTimeout: shutdownTimeout,
		state:           StateStopped,
	}

	return daemon, nil
}

// SocketPath returns the filesystem path used for the Unix domain socket.
func (d *Daemon) SocketPath() string {
	return d.socketPath
}

// PIDPath returns the filesystem path used for the locked pid file.
func (d *Daemon) PIDPath() string {
	return d.pidPath
}

// Status returns an in-process snapshot of the daemon state.
func (d *Daemon) Status() Status {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var startedAt time.Time
	var instanceID string
	if d.current != nil {
		startedAt = d.current.startedAt
		instanceID = d.current.instanceID
	}
	return Status{
		State:      d.state,
		PID:        os.Getpid(),
		InstanceID: instanceID,
		StartedAt:  startedAt,
	}
}

// Start acquires singleton ownership, binds the Unix socket, and begins
// serving. It returns only after the socket is ready to accept connections.
func (d *Daemon) Start() (resultErr error) {
	_, resultErr = d.start()
	return resultErr
}

func (d *Daemon) start() (_ *daemonRun, resultErr error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.state != StateStopped {
		if d.state == StateStarting || d.state == StateRunning || d.state == StateStopping {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("start daemon from state %q", d.state)
	}
	d.state = StateStarting

	var (
		pidFile    *os.File
		pidInfo    fs.FileInfo
		listener   *net.UnixListener
		socketInfo fs.FileInfo
	)
	defer func() {
		if resultErr == nil {
			return
		}

		cleanupErr := cleanupResources(d.socketPath, listener, socketInfo, d.pidPath, pidFile, pidInfo)
		resultErr = errors.Join(resultErr, cleanupErr)
		d.state = StateStopped
	}()

	if err := ensureDataDirectory(d.dataDir); err != nil {
		return nil, err
	}

	var err error
	pidFile, pidInfo, err = acquirePIDFile(d.pidPath)
	if err != nil {
		return nil, err
	}

	if err := prepareSocketPath(d.socketPath); err != nil {
		return nil, err
	}

	listener, err = net.ListenUnix("unix", &net.UnixAddr{Name: d.socketPath, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on unix socket %q: %w", d.socketPath, err)
	}
	// Cleanup is explicit so that ownership can be checked before unlinking.
	listener.SetUnlinkOnClose(false)

	socketInfo, err = os.Lstat(d.socketPath)
	if err != nil {
		return nil, fmt.Errorf("inspect unix socket %q: %w", d.socketPath, err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("bound path %q is not a unix socket", d.socketPath)
	}
	if err := os.Chmod(d.socketPath, 0o600); err != nil {
		return nil, fmt.Errorf("set unix socket permissions on %q: %w", d.socketPath, err)
	}
	instanceID, err := newInstanceID()
	if err != nil {
		return nil, err
	}

	serveDone := make(chan struct{})
	server := &http.Server{
		Handler:           d.httpHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
	}

	run := &daemonRun{
		instanceID: instanceID,
		startedAt:  time.Now().UTC(),
		pidFile:    pidFile,
		pidInfo:    pidInfo,
		listener:   listener,
		socketInfo: socketInfo,
		server:     server,
		done:       serveDone,
	}
	d.current = run
	d.state = StateRunning

	go run.serve()

	return run, nil
}

// Run starts the daemon and blocks until ctx is cancelled, Shutdown is called,
// or the HTTP server exits. Context cancellation is treated as a normal,
// graceful stop.
func (d *Daemon) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("daemon run context cannot be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	run, err := d.start()
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
	case <-run.done:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
	defer cancel()
	_ = d.shutdownRun(shutdownCtx, run)
	return errors.Join(run.wait(), run.shutdownResult())
}

// Wait blocks until the current HTTP serving loop exits.
func (d *Daemon) Wait() error {
	d.mu.RLock()
	run := d.current
	if run == nil {
		run = d.last
	}
	d.mu.RUnlock()
	if run == nil {
		return ErrNotRunning
	}

	return run.wait()
}

// Shutdown gracefully stops HTTP serving and removes the owned socket and pid
// file. It is safe to call more than once or concurrently.
func (d *Daemon) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("daemon shutdown context cannot be nil")
	}

	d.mu.RLock()
	run := d.current
	d.mu.RUnlock()
	if run == nil {
		return nil
	}

	return d.shutdownRun(ctx, run)
}

func (d *Daemon) shutdownRun(ctx context.Context, run *daemonRun) error {
	d.shutdownMu.Lock()
	defer d.shutdownMu.Unlock()

	d.mu.Lock()
	// This shutdown call belongs to the run observed by its caller. A queued
	// call from an older run must not stop a replacement that has since started.
	if d.current != run {
		d.mu.Unlock()
		return nil
	}
	d.state = StateStopping
	d.mu.Unlock()

	var shutdownErr error
	if run.server != nil {
		if err := run.server.Shutdown(ctx); err != nil {
			shutdownErr = err
			shutdownErr = errors.Join(shutdownErr, run.server.Close())
		}
	}
	if run.done != nil {
		<-run.done
	}

	cleanupErr := cleanupResources(
		d.socketPath,
		run.listener,
		run.socketInfo,
		d.pidPath,
		run.pidFile,
		run.pidInfo,
	)

	d.mu.Lock()
	if d.current == run {
		d.current = nil
		d.last = run
		d.state = StateStopped
	}
	d.mu.Unlock()

	resultErr := errors.Join(shutdownErr, cleanupErr)
	run.mu.Lock()
	run.shutdownErr = resultErr
	run.mu.Unlock()
	return resultErr
}

func (run *daemonRun) serve() {
	err := run.server.Serve(run.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}

	run.mu.Lock()
	run.serveErr = err
	run.mu.Unlock()
	close(run.done)
}

func (run *daemonRun) wait() error {
	<-run.done

	run.mu.RLock()
	defer run.mu.RUnlock()
	return run.serveErr
}

func (run *daemonRun) shutdownResult() error {
	run.mu.RLock()
	defer run.mu.RUnlock()
	return run.shutdownErr
}

func (d *Daemon) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(StatusEndpoint, d.handleStatus)
	mux.HandleFunc(ShutdownEndpoint, d.handleShutdown)
	if d.application != nil {
		mux.Handle("/", d.application)
	}
	return mux
}

func (d *Daemon) handleShutdown(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	d.mu.RLock()
	run := d.current
	validInstance := run != nil && request.Header.Get(InstanceIDHeader) == run.instanceID
	d.mu.RUnlock()
	if !validInstance {
		http.Error(writer, "daemon instance does not match", http.StatusConflict)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(writer).Encode(map[string]string{"state": string(StateStopping)})
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), d.shutdownTimeout)
		defer cancel()
		_ = d.shutdownRun(ctx, run)
	}()
}

func (d *Daemon) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(writer).Encode(d.Status()); err != nil {
		return
	}
}

func ensureDataDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("daemon data directory %q cannot be a symbolic link", path)
		}
		if !info.IsDir() {
			return fmt.Errorf("daemon data directory %q is not a directory", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect daemon data directory %q: %w", path, err)
	}

	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create daemon data directory %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set daemon data directory permissions on %q: %w", path, err)
	}

	return nil
}

func acquirePIDFile(path string) (*os.File, fs.FileInfo, error) {
	const maxHandoffAttempts = 8

	for attempt := 0; attempt < maxHandoffAttempts; attempt++ {
		if info, err := os.Lstat(path); err == nil {
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("daemon pid path %q is not a regular file", path)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("inspect daemon pid path %q: %w", path, err)
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("open daemon pid file %q: %w", path, err)
		}

		if err := tryLockFile(file); err != nil {
			_ = file.Close()
			if isLockContended(err) {
				pid := strings.TrimSpace(readSmallFile(path, 64))
				if pid != "" {
					return nil, nil, fmt.Errorf("%w (pid %s)", ErrAlreadyRunning, pid)
				}
				return nil, nil, ErrAlreadyRunning
			}
			return nil, nil, fmt.Errorf("lock daemon pid file %q: %w", path, err)
		}

		info, err := file.Stat()
		if err != nil {
			_ = unlockFile(file)
			_ = file.Close()
			return nil, nil, fmt.Errorf("inspect daemon pid file %q: %w", path, err)
		}

		// A previous owner can unlink the pid path between this process opening
		// the old inode and acquiring its lock. Never proceed with an unlinked
		// lock: retry against the path that future starters will actually open.
		current, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !os.SameFile(info, current)) {
			_ = unlockFile(file)
			_ = file.Close()
			continue
		}
		if err != nil {
			_ = unlockFile(file)
			_ = file.Close()
			return nil, nil, fmt.Errorf("verify daemon pid file %q: %w", path, err)
		}

		releaseOnError := func(cause error) (*os.File, fs.FileInfo, error) {
			cleanupErr := removeOwnedPath(path, info)
			cleanupErr = errors.Join(cleanupErr, unlockFile(file), file.Close())
			return nil, nil, errors.Join(cause, cleanupErr)
		}
		if err := file.Chmod(0o600); err != nil {
			return releaseOnError(fmt.Errorf("set daemon pid file permissions on %q: %w", path, err))
		}
		if err := file.Truncate(0); err != nil {
			return releaseOnError(fmt.Errorf("truncate daemon pid file %q: %w", path, err))
		}
		if _, err := file.Seek(0, 0); err != nil {
			return releaseOnError(fmt.Errorf("seek daemon pid file %q: %w", path, err))
		}
		if _, err := file.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
			return releaseOnError(fmt.Errorf("write daemon pid file %q: %w", path, err))
		}
		if err := file.Sync(); err != nil {
			return releaseOnError(fmt.Errorf("sync daemon pid file %q: %w", path, err))
		}
		return file, info, nil
	}

	return nil, nil, fmt.Errorf("daemon pid file %q changed repeatedly during lock acquisition", path)
}

func prepareSocketPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect daemon socket path %q: %w", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket path %q", path)
	}

	connection, dialErr := net.DialTimeout("unix", path, 250*time.Millisecond)
	if dialErr == nil {
		_ = connection.Close()
		return ErrAlreadyRunning
	}
	if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
		return fmt.Errorf("probe existing unix socket %q: %w", path, dialErr)
	}

	if err := removeOwnedPath(path, info); err != nil {
		return fmt.Errorf("remove stale unix socket %q: %w", path, err)
	}
	return nil
}

func cleanupResources(
	socketPath string,
	listener *net.UnixListener,
	socketInfo fs.FileInfo,
	pidPath string,
	pidFile *os.File,
	pidInfo fs.FileInfo,
) error {
	var cleanupErr error
	if listener != nil {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close unix socket: %w", err))
		}
	}
	if err := removeOwnedPath(socketPath, socketInfo); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}

	// Remove the pid path while its inode is still locked. Unlocking first can
	// race with a successor that opens the old path before it is removed.
	if err := removeOwnedPath(pidPath, pidInfo); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if pidFile != nil {
		if err := unlockFile(pidFile); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unlock daemon pid file: %w", err))
		}
		if err := pidFile.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("close daemon pid file: %w", err))
		}
	}

	return cleanupErr
}

func removeOwnedPath(path string, owner fs.FileInfo) error {
	if owner == nil {
		return nil
	}

	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect owned path %q during cleanup: %w", path, err)
	}
	if !os.SameFile(owner, current) {
		return fmt.Errorf("refusing to remove replaced path %q", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove owned path %q: %w", path, err)
	}
	return nil
}

func readSmallFile(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	buffer := make([]byte, limit)
	count, _ := file.Read(buffer)
	return string(buffer[:count])
}

func newInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate daemon instance id: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}
