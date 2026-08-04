package cli

import "github.com/spf13/cobra"

const (
	promptsDirPathKey = "prompts_dir_path"
	dbPathKey         = "db_path"
)

func NewCmdConfig() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration for cronmate",
		Long:  `TODO: long description by composing from subcommands`,
	}

	// cmd.AddCommand(NewCmdConfigGet(...))
	return cmd
}
