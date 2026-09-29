//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureDetachedDaemonProcess(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return nil
}

func daemonSignalContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	return ctx, stop, nil
}

func daemonTerminationSignal() (os.Signal, error) {
	return syscall.SIGTERM, nil
}

func daemonKillSignal() (os.Signal, error) {
	return syscall.SIGKILL, nil
}

func openDaemonLogFile(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_APPEND|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
