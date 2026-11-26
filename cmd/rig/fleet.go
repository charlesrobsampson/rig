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
	"github.com/charlesrobsampson/rig/internal/domain"
)

// chooseVessel turns a vessel chosen by name into its config. The order is --config,
// --vessel, $RIG_CONFIG, $RIG_VESSEL, then the default config.
func (a *app) chooseVessel(fromFlag bool) error {
	a.vesselFromFlag = fromFlag
	name := a.vesselName
	switch {
	case name != "" && a.configPath != "":
		return usageError(errors.New("--config and --vessel both choose a config; give one"))
	case name == "" && (a.configPath != "" || os.Getenv("RIG_CONFIG") != ""):
		return nil
	case name == "":
		if name = os.Getenv("RIG_VESSEL"); name == "" {
			return nil
		}
		a.vesselName = name
	}
	fleet, user, err := loadFleet()
	if err != nil {
		return err
	}
	var known []string
	for _, h := range fleet.All(user) {
		if h.Name == name {
			a.configPath = h.Config
			return nil
		}
		known = append(known, h.Name)
	}
	if len(known) == 0 {
		return usageError(fmt.Errorf("no vessel named %q: none are registered (rig fleet add <name> <config>)", name))
	}
	return usageError(fmt.Errorf("no vessel named %q; known vessels: %s (rig fleet)", name, strings.Join(known, ", ")))
}

// visibleVessels is the fleet as listed and picked from: every vessel, except the default
// config when it holds no tickets (it is the machine layer first, a vessel only if used).
// Choosing it by name still works. Tickets are counted on disk, so no daemon is needed.
func visibleVessels(fleet config.Fleet, user string) []config.NamedVessel {
	var out []config.NamedVessel
	for _, h := range fleet.All(user) {
		if h.Name == config.DefaultVessel && !hasTickets(h) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// hasTickets reports whether the vessel's home has a ticket directory.
func hasTickets(h config.NamedVessel) bool {
	g, err := config.LoadGlobal(config.GlobalOptions{Path: h.Config})
	if err != nil {
		return true // a config that does not load is worth showing, with its error
	}
	entries, err := os.ReadDir(g.TicketsDir())
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(g.TicketsDir(), e.Name(), "ticket.yaml")); err == nil {
			return true
		}
	}
	return false
}

func loadFleet() (config.Fleet, string, error) {
	user, err := os.UserHomeDir()
	if err != nil {
		return nil, "", fmt.Errorf("find home directory: %w", err)
	}
	fleet, err := config.LoadFleet(user)
	return fleet, user, err
}

// ticketRef reads a ticket argument, which may name its vessel ("osmo/t14"): it returns
// the app for that vessel and the ticket's own id. A vessel given with -V that is not the
// ticket's is a mistake; one from $RIG_VESSEL gives way to the ticket's.
func (a *app) ticketRef(ref string) (*app, string, error) {
	name, id, ok := strings.Cut(ref, "/")
	if !ok {
		return a, ref, nil
	}
	if a.vesselFromFlag && a.vesselName != name {
		return nil, "", usageError(fmt.Errorf("ticket %s is on vessel %s, not on -V %s", ref, name, a.vesselName))
	}
	fleet, user, err := loadFleet()
	if err != nil {
		return nil, "", err
	}
	for _, v := range fleet.All(user) {
		if v.Name == name {
			target := *a
			target.configPath, target.vesselName = v.Config, v.Name
			return &target, id, nil
		}
	}
	return nil, "", usageError(fmt.Errorf("no vessel named %q (rig fleet)", name))
}

// standing sums up where a vessel's tickets stand: "13 (2 need you · 1 running · 10 done)".
func standing(tickets []api.TicketSummary) string {
	var needYou, running, done int
	for _, t := range tickets {
		switch {
		case t.PhaseRequest != nil || len(t.Drift) > 0 || t.Error != "",
			t.Review != nil && (t.Review.Status == domain.ReviewStopped || t.Review.Status == domain.ReviewPassed && t.Phase != domain.PhaseDone):
			needYou++
		case t.Phase == domain.PhaseDone:
			done++
		default:
			running++
		}
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{needYou, "need you"}, {running, "running"}, {done, "done"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if len(parts) == 0 {
		return "0"
	}
	return fmt.Sprintf("%d (%s)", len(tickets), strings.Join(parts, " · "))
}

// vesselInfo is one vessel as `rig fleet` shows it.
type vesselInfo struct {
	Name    string `json:"name"`
	Config  string `json:"config"`
	Home    string `json:"home,omitempty"`
	Running bool   `json:"running"`
	Version string `json:"version,omitempty"`
	Tickets *int   `json:"tickets,omitempty"`
	// Standing says where they stand, as `rig fleet` prints it.
	Standing string `json:"standing,omitempty"`
	Error    string `json:"error,omitempty"`
}

// inspectVessel looks at a vessel without starting its daemon; tickets is filled only when
// it runs.
func (a *app) inspectVessel(cmd *cobra.Command, h config.NamedVessel) (vesselInfo, []api.TicketSummary) {
	info := vesselInfo{Name: h.Name, Config: h.Config}
	sub := *a
	sub.configPath, sub.vesselName = h.Config, ""
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
	info.Tickets, info.Standing = &n, standing(res.Tickets)
	return info, res.Tickets
}

func newFleetCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fleet",
		Short: "List your vessels (rig instances) you can choose with --vessel, and whether their daemons run",
		Long: `List your vessels: the rig instances you can choose with --vessel (or $RIG_VESSEL).
Each is a config with its own home directory, daemon, agents and record; in jybe, one
per product. No daemon is started to find out. The default config is the vessel named
"default" when it exists.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			fleet, user, err := loadFleet()
			if err != nil {
				return err
			}
			var infos []vesselInfo
			for _, h := range visibleVessels(fleet, user) {
				info, _ := a.inspectVessel(cmd, h)
				infos = append(infos, info)
			}
			if a.json {
				return printJSON(cmd.OutOrStdout(), infos)
			}
			if len(infos) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no vessels: add one with rig fleet add <name> <config>")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VESSEL\tDAEMON\tTICKETS\tHOME\tCONFIG")
			for _, i := range infos {
				daemon, tickets := "stopped", "-"
				if i.Running {
					daemon = "running (" + i.Version + ")"
				}
				if i.Tickets != nil {
					tickets = i.Standing
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
		Short: "Register a config as a vessel you can choose by name",
		Args:  usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.CheckVesselName(name); err != nil {
				return usageError(err)
			}
			path, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			fleet, user, err := loadFleet()
			if err != nil {
				return err
			}
			if old, ok := fleet[name]; ok {
				return fmt.Errorf("a vessel named %q is already registered (%s); remove it first", name, old)
			}
			g, err := config.LoadGlobal(config.GlobalOptions{Path: path})
			if err != nil {
				return err
			}
			fleet[name] = path
			if err := config.SaveFleet(user, fleet); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "vessel %s: home %s (config %s)\n", name, g.Paths.Home, path)
			return nil
		},
	}, &cobra.Command{
		Use:   "remove <name>",
		Short: "Forget a vessel's name (its config, files and daemon are not touched)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			fleet, user, err := loadFleet()
			if err != nil {
				return err
			}
			if _, ok := fleet[args[0]]; !ok {
				return fmt.Errorf("no vessel named %q is registered", args[0])
			}
			delete(fleet, args[0])
			if err := config.SaveFleet(user, fleet); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "forgot vessel %s\n", args[0])
			return nil
		},
	})
	return cmd
}

// vesselTickets is one vessel in `rig list --all`.
type vesselTickets struct {
	Vessel  string              `json:"vessel"`
	Running bool                `json:"running"`
	Tickets []api.TicketSummary `json:"tickets"`
	Error   string              `json:"error,omitempty"`
}

// listAll shows every running vessel's tickets, one section per vessel, starting nothing.
func (a *app) listAll(cmd *cobra.Command) error {
	fleet, user, err := loadFleet()
	if err != nil {
		return err
	}
	var all []vesselTickets
	for _, h := range visibleVessels(fleet, user) {
		info, tickets := a.inspectVessel(cmd, h)
		all = append(all, vesselTickets{Vessel: h.Name, Running: info.Running, Tickets: tickets, Error: info.Error})
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
			fmt.Fprintf(out, "%s: %s\n", h.Vessel, h.Error)
		case !h.Running:
			fmt.Fprintf(out, "%s: daemon not running\n", h.Vessel)
		default:
			fmt.Fprintf(out, "%s\n", h.Vessel)
			writeTicketTable(out, h.Tickets, a.now(), a.termWidth())
		}
	}
	return nil
}
