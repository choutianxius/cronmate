package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestConfigWithoutActionShowsHelp(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	stdout, _, err := executeConfigCommand()
	if err != nil {
		t.Fatalf("config command error = %v", err)
	}
	if !strings.Contains(stdout, "Usage:") || !strings.Contains(stdout, "--show") {
		t.Fatalf("config help output = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(homeDir, ".config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config help accessed or created the config directory: %v", err)
	}
}

func TestConfigRejectsArgumentsAndConflictingActions(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "argument", args: []string{"unexpected"}},
		{name: "conflicting actions", args: []string{"--show", "--reload"}},
		{name: "validate alias conflict", args: []string{"--test", "--edit"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := executeConfigCommand(test.args...)
			if err == nil {
				t.Fatalf("config %v error = nil", test.args)
			}
		})
	}
}

func TestConfigShowPrintsExactMalformedFile(t *testing.T) {
	raw := []byte("log_level: [\n# no trailing newline")
	writeCLIConfig(t, raw)

	stdout, stderr, err := executeConfigCommand("--show")
	if err != nil {
		t.Fatalf("config --show error = %v", err)
	}
	if stdout != string(raw) {
		t.Fatalf("config --show output = %q, want %q", stdout, raw)
	}
	if stderr != "" {
		t.Fatalf("config --show stderr = %q, want empty", stderr)
	}
}

func TestConfigShowMissingFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	stdout, _, err := executeConfigCommand("--show")
	if err == nil {
		t.Fatal("config --show error = nil, want missing-file error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config --show error = %v, want os.ErrNotExist", err)
	}
	if stdout != "" {
		t.Fatalf("config --show output = %q, want empty", stdout)
	}
}

func TestConfigReloadAndValidationAreSilent(t *testing.T) {
	writeCLIConfig(t, []byte("log_level: info\nagent:\n  provider: openai\n"))

	for _, args := range [][]string{
		{"--reload"},
		{"--validate"},
		{"--test"},
		{"--validate", "--test"},
	} {
		stdout, stderr, err := executeConfigCommand(args...)
		if err != nil {
			t.Errorf("config %v error = %v", args, err)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("config %v output = (%q, %q), want silent", args, stdout, stderr)
		}
	}
}

func TestConfigReloadAndValidationRejectInvalidConfig(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "malformed YAML", config: "log_level: [\n"},
		{name: "incompatible type", config: "agent: openai\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			writeCLIConfig(t, []byte(test.config))
			for _, flag := range []string{"--reload", "--validate", "--test"} {
				if _, _, err := executeConfigCommand(flag); err == nil {
					t.Errorf("config %s error = nil", flag)
				}
			}
		})
	}
}

func TestConfigReloadAndValidationRejectMissingConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	for _, flag := range []string{"--reload", "--validate", "--test"} {
		_, _, err := executeConfigCommand(flag)
		if err == nil {
			t.Errorf("config %s error = nil, want missing-file error", flag)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("config %s error = %v, want os.ErrNotExist", flag, err)
		}
	}
}

func TestEditorCommandPrecedenceAndArguments(t *testing.T) {
	tests := []struct {
		name   string
		visual string
		editor string
		want   []string
	}{
		{
			name:   "visual",
			visual: `code --wait "profile name"`,
			editor: "nano",
			want:   []string{"code", "--wait", "profile name"},
		},
		{
			name:   "editor",
			visual: "  ",
			editor: "nano -w",
			want:   []string{"nano", "-w"},
		},
		{
			name: "fallback",
			want: []string{"vi"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("VISUAL", test.visual)
			t.Setenv("EDITOR", test.editor)

			got, err := editorCommand()
			if err != nil {
				t.Fatalf("editorCommand() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("editorCommand() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestEditorCommandRejectsMalformedValue(t *testing.T) {
	t.Setenv("VISUAL", `code "unterminated`)

	if _, err := editorCommand(); err == nil {
		t.Fatal("editorCommand() error = nil, want parse error")
	}
}

func TestConfigEditUsesVisualAndDoesNotParseConfig(t *testing.T) {
	original := []byte("log_level: [\n")
	configPath := writeCLIConfig(t, original)
	recordPath := filepath.Join(t.TempDir(), "editor-args")
	configureEditorHelper(t, recordPath, false, "--from-visual", "two words")
	t.Setenv("EDITOR", "editor-must-not-run")

	stdout, stderr, err := executeConfigCommand("-e")
	if err != nil {
		t.Fatalf("config -e error = %v", err)
	}
	if stdout != "editor stdout" || stderr != "editor stderr" {
		t.Fatalf("config -e output = (%q, %q)", stdout, stderr)
	}

	got, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read editor arguments: %v", err)
	}
	want := strings.Join([]string{"--from-visual", "two words", configPath}, "\n")
	if string(got) != want {
		t.Fatalf("editor arguments = %q, want %q", got, want)
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read existing config: %v", err)
	}
	if !bytes.Equal(contents, original) {
		t.Fatalf("existing config changed before editor: got %q, want %q", contents, original)
	}
}

func TestConfigEditCreatesMissingConfigPath(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	recordPath := filepath.Join(t.TempDir(), "editor-args")
	configureEditorHelper(t, recordPath, false)

	if _, _, err := executeConfigCommand("--edit"); err != nil {
		t.Fatalf("config --edit error = %v", err)
	}

	configDir := filepath.Join(homeDir, ".config", "cronmate")
	directoryInfo, err := os.Stat(configDir)
	if err != nil {
		t.Fatalf("stat config directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("config directory mode = %o, want 700", directoryInfo.Mode().Perm())
	}

	configPath := filepath.Join(configDir, "config.yaml")
	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config file: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("config file mode = %o, want 600", fileInfo.Mode().Perm())
	}
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if len(contents) != 0 {
		t.Fatalf("new config file = %q, want empty", contents)
	}
}

func TestConfigEditPropagatesEditorFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	configureEditorHelper(t, filepath.Join(t.TempDir(), "editor-args"), true)

	_, _, err := executeConfigCommand("--edit")
	if err == nil || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("config --edit error = %v, want editor exit status", err)
	}
}

func TestEditorHelperProcess(t *testing.T) {
	if os.Getenv("CRONMATE_EDITOR_HELPER") != "1" {
		return
	}

	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator == -1 {
		os.Exit(2)
	}

	contents := strings.Join(os.Args[separator+1:], "\n")
	if err := os.WriteFile(os.Getenv("CRONMATE_EDITOR_RECORD"), []byte(contents), 0o600); err != nil {
		os.Exit(3)
	}
	if os.Getenv("CRONMATE_EDITOR_FAIL") == "1" {
		os.Exit(7)
	}

	fmt.Fprint(os.Stdout, "editor stdout")
	fmt.Fprint(os.Stderr, "editor stderr")
	os.Exit(0)
}

func executeConfigCommand(args ...string) (string, string, error) {
	cmd := NewCmdConfig()
	stdout := bytes.Buffer{}
	stderr := bytes.Buffer{}
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true

	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func writeCLIConfig(t *testing.T, contents []byte) string {
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

func configureEditorHelper(t *testing.T, recordPath string, fail bool, args ...string) {
	t.Helper()

	parts := []string{strconv.Quote(os.Args[0]), "-test.run=^TestEditorHelperProcess$", "--"}
	for _, arg := range args {
		parts = append(parts, strconv.Quote(arg))
	}
	t.Setenv("VISUAL", strings.Join(parts, " "))
	t.Setenv("CRONMATE_EDITOR_HELPER", "1")
	t.Setenv("CRONMATE_EDITOR_RECORD", recordPath)
	if fail {
		t.Setenv("CRONMATE_EDITOR_FAIL", "1")
	} else {
		t.Setenv("CRONMATE_EDITOR_FAIL", "")
	}
}
