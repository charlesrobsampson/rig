package main

import (
	"encoding/json"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/config"
)

func newConfigCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect rig's configuration"}
	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the effective global configuration as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			g, err := config.LoadGlobal(config.GlobalOptions{Path: *configPath})
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(g)
		},
	})
	return cmd
}
