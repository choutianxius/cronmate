package cli

import "github.com/spf13/cobra"

func New() *cobra.Command {
	cmdRoot := newCmdRoot()

	cmdRoot.AddCommand(NewCmdConfig(), NewCmdDaemon())

	return cmdRoot
}

func newCmdRoot() *cobra.Command {
	return &cobra.Command{
		Use:           "cronmate",
		Short:         "LLM-based cron job manager",
		Long:          `Cronmate is a daemon and CLI to manage LLM-based jobs as prompts.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
}
