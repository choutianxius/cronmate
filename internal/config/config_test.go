package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	got, err := Path()
	if err != nil {
		t.Fatalf("Path() error = %v", err)
	}

	want := filepath.Join(homeDir, ".config", "cronmate", "config.yaml")
	if got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
}

func TestLoadCurrentGetAndReadRaw(t *testing.T) {
	resetState(t)
	raw := []byte("data_dir: /tmp/cronmate\nlog_level: debug\nagent:\n  provider: openai\n  model: gpt-5\n")
	configPath := writeConfig(t, raw)

	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config before load: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.DataDir != "/tmp/cronmate" || loaded.LogLevel != "debug" {
		t.Fatalf("Load() = %#v", loaded)
	}
	if loaded.Agent.Provider != "openai" || loaded.Agent.Model != "gpt-5" {
		t.Fatalf("Load().Agent = %#v", loaded.Agent)
	}

	current, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if !reflect.DeepEqual(current, loaded) {
		t.Fatalf("Current() = %#v, want %#v", current, loaded)
	}

	provider, err := Get("agent.provider")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if provider != "openai" {
		t.Fatalf("Get() = %#v, want %q", provider, "openai")
	}

	gotRaw, err := ReadRaw()
	if err != nil {
		t.Fatalf("ReadRaw() error = %v", err)
	}
	if !reflect.DeepEqual(gotRaw, raw) {
		t.Fatalf("ReadRaw() = %q, want %q", gotRaw, raw)
	}

	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config after load: %v", err)
	}
	if before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("loading changed config metadata: before=%v after=%v", before, after)
	}
}

func TestLoadMissingConfigDoesNotCreateAnything(t *testing.T) {
	resetState(t)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	_, err := Load()
	if err == nil {
		t.Fatal("Load() error = nil, want missing-file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() error = %v, want os.ErrNotExist", err)
	}

	configDir := filepath.Join(homeDir, ".config")
	if _, err := os.Stat(configDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load() created %q or returned unexpected error: %v", configDir, err)
	}
	if _, err := Current(); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Current() error = %v, want ErrNotInitialized", err)
	}
}

func TestReloadUsesExternalChanges(t *testing.T) {
	resetState(t)
	configPath := writeConfig(t, []byte("log_level: info\n"))

	if err := Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o600); err != nil {
		t.Fatalf("update config: %v", err)
	}
	if err := Reload(); err != nil {
		t.Fatalf("Reload() error = %v", err)
	}

	current, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.LogLevel != "debug" {
		t.Fatalf("Current().LogLevel = %q, want debug", current.LogLevel)
	}
}

func TestFailedReloadKeepsLastValidConfig(t *testing.T) {
	resetState(t)
	configPath := writeConfig(t, []byte("log_level: info\n"))

	if err := Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := os.WriteFile(configPath, []byte("log_level: [\n"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if err := Reload(); err == nil {
		t.Fatal("Reload() error = nil, want YAML parse error")
	}

	current, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.LogLevel != "info" {
		t.Fatalf("Current().LogLevel = %q, want last valid value", current.LogLevel)
	}
	value, err := Get("log_level")
	if err != nil {
		t.Fatalf("Get() after failed reload error = %v", err)
	}
	if value != "info" {
		t.Fatalf("Get() after failed reload = %#v, want info", value)
	}
}

func TestInitRecoversAfterMissingConfig(t *testing.T) {
	resetState(t)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	if err := Init(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first Init() error = %v, want os.ErrNotExist", err)
	}

	configPath := filepath.Join(homeDir, ".config", "cronmate", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := Init(); err != nil {
		t.Fatalf("second Init() error = %v", err)
	}
	current, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if current.LogLevel != "debug" {
		t.Fatalf("Current().LogLevel = %q, want debug", current.LogLevel)
	}
}

func TestConcurrentLoadReloadAndGet(t *testing.T) {
	resetState(t)
	writeConfig(t, []byte("log_level: info\nagent:\n  provider: openai\n"))
	if err := Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	const goroutines = 32
	errs := make(chan error, goroutines)
	var waitGroup sync.WaitGroup
	for index := range goroutines {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			if index%2 == 0 {
				errs <- Reload()
				return
			}
			value, err := Get("agent.provider")
			if err == nil && value != "openai" {
				err = fmt.Errorf("provider = %#v, want openai", value)
			}
			errs <- err
		}()
	}
	waitGroup.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("concurrent config operation: %v", err)
		}
	}
}

func TestGetErrors(t *testing.T) {
	resetState(t)

	if _, err := Get("log_level"); !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("Get() error = %v, want ErrNotInitialized", err)
	}

	writeConfig(t, []byte("agent:\n  provider: openai\n"))
	if err := Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, key := range []string{"", "missing", "agent.provider.name"} {
		if _, err := Get(key); err == nil || !strings.Contains(err.Error(), "config key") {
			t.Errorf("Get(%q) error = %v, want config key error", key, err)
		}
	}
}

func writeConfig(t *testing.T, contents []byte) string {
	t.Helper()

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	configPath := filepath.Join(homeDir, ".config", "cronmate", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return configPath
}

func resetState(t *testing.T) {
	t.Helper()

	mu.Lock()
	cfg = nil
	v.SetConfigType("yaml")
	if err := v.ReadConfig(strings.NewReader("")); err != nil {
		mu.Unlock()
		t.Fatalf("clear Viper state: %v", err)
	}
	once = sync.Once{}
	raw = nil
	mu.Unlock()
}
