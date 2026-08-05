package cli

import (
	"fmt"

	"cronmate/internal/config"

	"github.com/spf13/cobra"
)

func NewCmdConfig() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration for cronmate",
		Long: `Manage configuration for cronmate.
Check subcommands for detailed usage.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return config.Init()
		},
	}

	cmd.AddCommand(newCmdConfigGet())
	// other commands...
	return cmd
}

func newCmdConfigGet() *cobra.Command {
	cmdGet := &cobra.Command{
		Use:   "get <key>",
		Short: "Read the config value at the given key",
		Long: `Read the current cronmate config value at the given key.
A key is a dotted string representing nested fields in the config, e.g., agent.provider`,
		Args: cobra.MaximumNArgs(1),
		RunE: runCmdConfigGet,
	}
	return cmdGet
}

func runCmdConfigGet(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	value, err := config.Get(args[0])
	if err != nil {
		return err
	}
	fmt.Fprint(cmd.OutOrStdout(), value)
	return nil
}
