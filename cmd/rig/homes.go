package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
)

// chooseHome turns a home chosen by name into its config. The order is --config,
// --home, $RIG_CONFIG, $RIG_HOME_NAME, then the default config.
func (a *app) chooseHome() error {
	name := a.homeName
	switch {
	case name != "" && a.configPath != "":
		return usageError(errors.New("--config and --home both choose a config; give one"))
	case name == "" && (a.configPath != "" || os.Getenv("RIG_CONFIG") != ""):
		return nil
	case name == "":
		if name = os.Getenv("RIG_HOME_NAME"); name == "" {
			return nil
		}
	}
	homes, user, err := loadHomes()
	if err != nil {
		return err
	}
	var known []string
	for _, h := range homes.All(user) {
		if h.Name == name {
			a.configPath = h.Config
			return nil
		}
		known = append(known, h.Name)
	}
	if len(known) == 0 {
		return usageError(fmt.Errorf("no home named %q: none are registered (rig homes add <name> <config>)", name))
	}
	return usageError(fmt.Errorf("no home named %q; known homes: %s (rig homes)", name, strings.Join(known, ", ")))
}

func loadHomes() (config.Homes, string, error) {
	user, err := os.UserHomeDir()
	if err != nil {
		return nil, "", fmt.Errorf("find home directory: %w", err)
	}
	homes, err := config.LoadHomes(user)
	return homes, user, err
}

// homeInfo is one home as `rig homes` shows it.
type homeInfo struct {
	Name    string `json:"name"`
	Config  string `json:"config"`
	Home    string `json:"home,omitempty"`
	Running bool   `json:"running"`
	Version string `json:"version,omitempty"`
	Tickets *int   `json:"tickets,omitempty"`
	Error   string `json:"error,omitempty"`
}

// inspectHome looks at a home without starting its daemon; tickets is filled only when
// it runs.
func (a *app) inspectHome(cmd *cobra.Command, h config.NamedHome) (homeInfo, []api.TicketSummary) {
	info := homeInfo{Name: h.Name, Config: h.Config}
	sub := *a
	sub.configPath, sub.homeName = h.Config, ""
	g, err := sub.global()
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	info.Home = g.Paths.Home
	c, err := sub.connect(cmd.Context(), false)
	if err != nil {
		return info, nil
	}
	defer c.Close()
	status, err := c.Status(cmd.Context())
	if err != nil {
		info.Error = err.Error()
		return info, nil
	}
	info.Running, info.Version = true, status.Version
	var res api.TicketListResult
	if err := c.Call(cmd.Context(), api.MethodTicketList, nil, &res); err != nil {
		info.Error = err.Error()
		return info, nil
	}
	n := len(res.Tickets)
	info.Tickets = &n
	return info, res.Tickets
}

func newHomesCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "homes",
		Short: "List the rig homes you can choose with --home, and whether their daemons run",
		Long: `List the rig homes you can choose with --home (or $RIG_HOME_NAME): each is a
config with its own home directory and daemon. No daemon is started to find out.
The default config is the home named "default" when it exists.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			homes, user, err := loadHomes()
			if err != nil {
				return err
			}
			var infos []homeInfo
			for _, h := range homes.All(user) {
				info, _ := a.inspectHome(cmd, h)
				infos = append(infos, info)
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), infos)
			}
			if len(infos) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no homes: add one with rig homes add <name> <config>")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tDAEMON\tTICKETS\tHOME\tCONFIG")
			for _, i := range infos {
				daemon, tickets := "stopped", "-"
				if i.Running {
					daemon = "running (" + i.Version + ")"
				}
				if i.Tickets != nil {
					tickets = fmt.Sprint(*i.Tickets)
				}
				if i.Error != "" {
					daemon = "error: " + i.Error
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.Name, daemon, tickets, i.Home, i.Config)
			}
			return tw.Flush()
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "add <name> <config>",
		Short: "Register a config as a home you can choose by name",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.CheckHomeName(name); err != nil {
				return usageError(err)
			}
			path, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			homes, user, err := loadHomes()
			if err != nil {
				return err
			}
			if old, ok := homes[name]; ok {
				return fmt.Errorf("a home named %q is already registered (%s); remove it first", name, old)
			}
			g, err := config.LoadGlobal(config.GlobalOptions{Path: path})
			if err != nil {
				return err
			}
			homes[name] = path
			if err := config.SaveHomes(user, homes); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "home %s: %s (config %s)\n", name, g.Paths.Home, path)
			return nil
		},
	}, &cobra.Command{
		Use:   "remove <name>",
		Short: "Forget a home's name (its config, files and daemon are not touched)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			homes, user, err := loadHomes()
			if err != nil {
				return err
			}
			if _, ok := homes[args[0]]; !ok {
				return fmt.Errorf("no home named %q is registered", args[0])
			}
			delete(homes, args[0])
			if err := config.SaveHomes(user, homes); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "forgot home %s\n", args[0])
			return nil
		},
	})
	return cmd
}

// homeTickets is one home in `rig list --all`.
type homeTickets struct {
	Home    string              `json:"home"`
	Running bool                `json:"running"`
	Tickets []api.TicketSummary `json:"tickets"`
	Error   string              `json:"error,omitempty"`
}

// listAll shows every running home's tickets, one section per home, starting nothing.
func (a *app) listAll(cmd *cobra.Command) error {
	homes, user, err := loadHomes()
	if err != nil {
		return err
	}
	var all []homeTickets
	for _, h := range homes.All(user) {
		info, tickets := a.inspectHome(cmd, h)
		all = append(all, homeTickets{Home: h.Name, Running: info.Running, Tickets: tickets, Error: info.Error})
	}
	if a.json {
		return printJSON(cmd.OutOrStdout(), all)
	}
	out := cmd.OutOrStdout()
	for i, h := range all {
		if i > 0 {
			fmt.Fprintln(out)
		}
		switch {
		case h.Error != "":
			fmt.Fprintf(out, "%s: %s\n", h.Home, h.Error)
		case !h.Running:
			fmt.Fprintf(out, "%s: daemon not running\n", h.Home)
		default:
			fmt.Fprintf(out, "%s\n", h.Home)
			writeTicketTable(out, h.Tickets)
		}
	}
	return nil
}
