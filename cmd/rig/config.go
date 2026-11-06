package main

import (
	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/config"
)

func newConfigCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect rig's configuration",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective global configuration as JSON",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			g, err := config.LoadGlobal(config.GlobalOptions{Path: a.configPath})
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), g)
		},
	})
	return cmd
}
