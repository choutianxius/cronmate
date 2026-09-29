//go:build !darwin && !linux

package daemon

import (
	"errors"
	"os"
)

var errUnsupportedPlatform = errors.New("cronmate daemon requires macOS or Linux")

func tryLockFile(_ *os.File) error {
	return errUnsupportedPlatform
}

func unlockFile(_ *os.File) error {
	return nil
}

func isLockContended(_ error) bool {
	return false
}
