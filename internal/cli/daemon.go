package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cronmate/internal/config"
	daemoncore "cronmate/internal/daemon"

	"github.com/spf13/cobra"
)

const daemonWatchInterval = time.Second

var errDaemonNotRunning = errors.New("cronmate daemon is not running")

type daemonController interface {
	Start(context.Context) (daemoncore.Status, error)
	Run(context.Context) error
	Status(context.Context) (daemoncore.Status, error)
	Stop(context.Context) error
}

type daemonControllerFactory func() (daemonController, error)

// NewCmdDaemon returns the command group containing every daemon lifecycle
// operation. The root command intentionally has no implicit daemon behavior.
func NewCmdDaemon() *cobra.Command {
	return newCmdDaemon(newSystemDaemonController)
}

func newCmdDaemon(newController daemonControllerFactory) *cobra.Command {
	// Cobra sorts commands alphabetically by default. Lifecycle commands read
	// more naturally in operational order, with stop and kill adjacent.
	cobra.EnableCommandSorting = false

	command := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the cronmate daemon",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}

	command.AddCommand(
		newCmdDaemonStatus(newController),
		newCmdDaemonStart(newController),
		newCmdDaemonRestart(newController),
		newCmdDaemonStop(newController),
		newCmdDaemonKill(newController),
	)

	return command
}

func newCmdDaemonStart(newController daemonControllerFactory) *cobra.Command {
	var foreground bool
	command := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controller, err := newController()
			if err != nil {
				return err
			}
			if foreground {
				return controller.Run(command.Context())
			}

			status, err := controller.Start(command.Context())
			if err != nil {
				return err
			}
			return writeDaemonStatus(command.OutOrStdout(), status)
		},
	}
	command.Flags().BoolVarP(
		&foreground,
		"foreground",
		"f",
		false,
		"Run attached to the current terminal",
	)
	return command
}

func newCmdDaemonStatus(newController daemonControllerFactory) *cobra.Command {
	var watch bool
	command := &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controller, err := newController()
			if err != nil {
				return err
			}
			return runDaemonStatus(
				command.Context(),
				command.OutOrStdout(),
				controller,
				watch,
				newDaemonTicker,
			)
		},
	}
	command.Flags().BoolVarP(&watch, "watch", "w", false, "Refresh until interrupted")
	return command
}

func newCmdDaemonStop(newController daemonControllerFactory) *cobra.Command {
	return newCmdDaemonStopAction(
		newController,
		"stop",
		"Gracefully stop the daemon",
	)
}

func newCmdDaemonKill(newController daemonControllerFactory) *cobra.Command {
	return newCmdDaemonStopAction(
		newController,
		"kill",
		"Gracefully stop the daemon (alias for stop)",
	)
}

func newCmdDaemonStopAction(
	newController daemonControllerFactory,
	use string,
	short string,
) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controller, err := newController()
			if err != nil {
				return err
			}
			if err := controller.Stop(command.Context()); err != nil {
				return err
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), "stopped")
			return err
		},
	}
}

func newCmdDaemonRestart(newController daemonControllerFactory) *cobra.Command {
	var foreground bool
	command := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			controller, err := newController()
			if err != nil {
				return err
			}
			if err := controller.Stop(command.Context()); err != nil && !errors.Is(err, errDaemonNotRunning) {
				return err
			}
			if foreground {
				return controller.Run(command.Context())
			}

			status, err := controller.Start(command.Context())
			if err != nil {
				return err
			}
			return writeDaemonStatus(command.OutOrStdout(), status)
		},
	}
	command.Flags().BoolVarP(
		&foreground,
		"foreground",
		"f",
		false,
		"Run the replacement attached to the current terminal",
	)
	return command
}

type daemonTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type realDaemonTicker struct {
	*time.Ticker
}

func (ticker realDaemonTicker) Chan() <-chan time.Time {
	return ticker.C
}

func newDaemonTicker() daemonTicker {
	return realDaemonTicker{Ticker: time.NewTicker(daemonWatchInterval)}
}

func runDaemonStatus(
	ctx context.Context,
	output io.Writer,
	controller daemonController,
	watch bool,
	newTicker func() daemonTicker,
) error {
	for {
		status, err := controller.Status(ctx)
		if err != nil {
			return err
		}
		if err := writeDaemonStatus(output, status); err != nil {
			return err
		}
		if !watch {
			return nil
		}

		ticker := newTicker()
		select {
		case <-ctx.Done():
			ticker.Stop()
			return nil
		case <-ticker.Chan():
			ticker.Stop()
		}
	}
}

func writeDaemonStatus(output io.Writer, status daemoncore.Status) error {
	startedAt := "-"
	if !status.StartedAt.IsZero() {
		startedAt = status.StartedAt.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(
		output,
		"%s pid=%d started_at=%s\n",
		status.State,
		status.PID,
		startedAt,
	)
	return err
}

func configuredDaemonDataDir() (string, error) {
	configuration, err := config.Load()
	if err != nil {
		return "", err
	}
	return resolveDaemonDataDir(configuration.DataDir)
}

func resolveDaemonDataDir(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		return defaultDaemonDataDir()
	}

	if path == "~" || strings.HasPrefix(path, "~/") {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory for data_dir: %w", err)
		}
		if path == "~" {
			path = homeDir
		} else {
			path = filepath.Join(homeDir, strings.TrimPrefix(path, "~/"))
		}
	} else if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("config data_dir uses unsupported home syntax: %q", configured)
	}

	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("config data_dir must be absolute or start with ~/: %q", configured)
	}
	return filepath.Clean(path), nil
}

func defaultDaemonDataDir() (string, error) {
	// The XDG base-directory specification requires an absolute value. Ignore
	// a relative value and use the standard home-directory fallback.
	if dataHome := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dataHome) {
		return filepath.Join(filepath.Clean(dataHome), "cronmate"), nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for default data_dir: %w", err)
	}
	if !filepath.IsAbs(homeDir) {
		return "", fmt.Errorf("home directory for default data_dir must be absolute: %q", homeDir)
	}
	return filepath.Join(homeDir, ".local", "share", "cronmate"), nil
}
