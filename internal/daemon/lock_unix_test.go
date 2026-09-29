//go:build darwin || linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestUnixFileLockContentionAndHandoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cronmate.pid")
	first := openLockTestFile(t, path)
	defer first.Close()
	second := openLockTestFile(t, path)
	defer second.Close()

	if err := tryLockFile(first); err != nil {
		t.Fatalf("tryLockFile(first) error = %v", err)
	}
	firstLocked := true
	defer func() {
		if firstLocked {
			_ = unlockFile(first)
		}
	}()

	if err := tryLockFile(second); !isLockContended(err) {
		t.Fatalf("tryLockFile(second) error = %v, want lock contention", err)
	}

	if err := unlockFile(first); err != nil {
		t.Fatalf("unlockFile(first) error = %v", err)
	}
	firstLocked = false

	if err := tryLockFile(second); err != nil {
		t.Fatalf("tryLockFile(second) after unlock error = %v", err)
	}
	defer func() {
		if err := unlockFile(second); err != nil {
			t.Errorf("unlockFile(second) error = %v", err)
		}
	}()
}

func TestIsLockContended(t *testing.T) {
	if !isLockContended(unix.EWOULDBLOCK) {
		t.Error("isLockContended(EWOULDBLOCK) = false, want true")
	}
	if !isLockContended(fmt.Errorf("wrapped: %w", unix.EAGAIN)) {
		t.Error("isLockContended(wrapped EAGAIN) = false, want true")
	}
	if isLockContended(errors.New("different error")) {
		t.Error("isLockContended(different error) = true, want false")
	}
	if isLockContended(nil) {
		t.Error("isLockContended(nil) = true, want false")
	}
}

func openLockTestFile(t *testing.T, path string) *os.File {
	t.Helper()

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock test file: %v", err)
	}
	return file
}
