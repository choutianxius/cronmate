package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"cronmate/internal/config"

	"github.com/mattn/go-shellwords"
	"github.com/spf13/cobra"
)

type configOptions struct {
	edit     bool
	show     bool
	reload   bool
	validate bool
	test     bool
}

func NewCmdConfig() *cobra.Command {
	options := configOptions{}
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and reload cronmate configuration",
		Long:  "Inspect, edit, reload, or validate ~/.config/cronmate/config.yaml.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCmdConfig(cmd, options)
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&options.edit, "edit", "e", false, "Open the config file in the system editor")
	flags.BoolVar(&options.show, "show", false, "Print the config file to stdout")
	flags.BoolVar(&options.reload, "reload", false, "Reload the config in the current process")
	flags.BoolVar(&options.validate, "validate", false, "Validate the config file")
	flags.BoolVarP(&options.test, "test", "t", false, "Alias for --validate")

	return cmd
}

func runCmdConfig(cmd *cobra.Command, options configOptions) error {
	validate := options.validate || options.test
	actions := 0
	for _, selected := range []bool{options.edit, options.show, options.reload, validate} {
		if selected {
			actions++
		}
	}

	switch {
	case actions == 0:
		return cmd.Help()
	case actions > 1:
		return errors.New("only one of --edit, --show, --reload, or --validate/--test may be used")
	case options.edit:
		return editConfig(cmd)
	case options.show:
		return showConfig(cmd)
	case options.reload:
		return config.Reload()
	default:
		return config.Init()
	}
}

func showConfig(cmd *cobra.Command) error {
	contents, err := config.ReadRaw()
	if err != nil {
		return err
	}

	written, err := cmd.OutOrStdout().Write(contents)
	if err != nil {
		return err
	}
	if written != len(contents) {
		return io.ErrShortWrite
	}

	return nil
}

func editConfig(cmd *cobra.Command) error {
	configPath, err := config.Path()
	if err != nil {
		return err
	}
	if err := ensureConfigFile(configPath); err != nil {
		return err
	}

	editor, err := editorCommand()
	if err != nil {
		return err
	}

	args := append([]string{}, editor[1:]...)
	args = append(args, configPath)
	process := exec.CommandContext(cmd.Context(), editor[0], args...)
	process.Stdin = cmd.InOrStdin()
	process.Stdout = cmd.OutOrStdout()
	process.Stderr = cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("run editor %q: %w", editor[0], err)
	}

	return nil
}

func ensureConfigFile(configPath string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	file, err := os.OpenFile(configPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create config file %q: %w", configPath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close config file %q: %w", configPath, err)
	}

	return nil
}

func editorCommand() ([]string, error) {
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		editor = "vi"
	}

	parsed, err := shellwords.Parse(editor)
	if err != nil {
		return nil, fmt.Errorf("parse editor command %q: %w", editor, err)
	}
	if len(parsed) == 0 {
		return nil, errors.New("editor command is empty")
	}

	return parsed, nil
}
