package main

import (
	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/config"
)

func newConfigCmd(a *app) *cobra.Command {
	var resolved bool
	show := func(cmd *cobra.Command, _ []string) error {
		g, src, err := config.LoadGlobalResolved(config.GlobalOptions{Path: a.configPath})
		if err != nil {
			return err
		}
		if !resolved {
			return printJSON(cmd.OutOrStdout(), g)
		}
		return printJSON(cmd.OutOrStdout(), struct {
			Config  config.Global  `json:"config"`
			Sources config.Sources `json:"sources"`
		}{g, src})
	}
	const resolvedHelp = "Also say which file each key came from: the vessel's config, the machine config (~/.config/rig/config.json) or the built-in defaults"
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect rig's configuration",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if resolved {
				return show(cmd, args)
			}
			return cmd.Help()
		},
	}
	cmd.Flags().BoolVar(&resolved, "resolved", false, resolvedHelp)
	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective global configuration as JSON: the vessel's config over the machine config",
		Args:  usageArgs(cobra.NoArgs),
		RunE:  show,
	}
	showCmd.Flags().BoolVar(&resolved, "resolved", false, resolvedHelp)
	cmd.AddCommand(showCmd)
	return cmd
}
