// Package backends picks the terminal.Backend named in config.
package backends

import (
	"fmt"

	"github.com/charlesrobsampson/rig/internal/terminal"
	"github.com/charlesrobsampson/rig/internal/terminal/herdr"
	"github.com/charlesrobsampson/rig/internal/terminal/tmux"
)

// New returns the backend for a defaults.terminal_backend value; "" means herdr.
func New(name string) (terminal.Backend, error) {
	switch name {
	case "", "herdr":
		return herdr.New(herdr.Options{}), nil
	case "tmux":
		return tmux.New(tmux.Options{}), nil
	}
	return nil, fmt.Errorf("terminal backend %q is not supported (want herdr or tmux)", name)
}
