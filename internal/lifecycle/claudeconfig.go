package lifecycle

import (
	"fmt"
	"path/filepath"
	"strings"
)

// agentConfigDir is where an agent's own harness configuration lives on the host.
func agentConfigDir(ticketDir, service string) string {
	return filepath.Join(ticketDir, ".rig-claude", service)
}

// trustablePaths resolves paths to the real paths a harness keys trust by, refusing
// any that leave root: rig only trusts what it created for a ticket.
func trustablePaths(root string, paths ...string) ([]string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve the tickets directory: %w", err)
	}
	var out []string
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", p, err)
		}
		if !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s is outside the tickets directory %s; rig will not mark it trusted", real, realRoot)
		}
		out = append(out, real)
	}
	return out, nil
}
