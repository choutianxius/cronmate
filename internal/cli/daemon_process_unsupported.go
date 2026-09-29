//go:build !darwin && !linux

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

var errDaemonUnsupportedPlatform = errors.New("cronmate daemon requires macOS or Linux")

func configureDetachedDaemonProcess(_ *exec.Cmd) error {
	return errDaemonUnsupportedPlatform
}

func daemonSignalContext(_ context.Context) (context.Context, context.CancelFunc, error) {
	return nil, nil, errDaemonUnsupportedPlatform
}

func daemonTerminationSignal() (os.Signal, error) {
	return nil, errDaemonUnsupportedPlatform
}

func daemonKillSignal() (os.Signal, error) {
	return nil, errDaemonUnsupportedPlatform
}

func openDaemonLogFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
