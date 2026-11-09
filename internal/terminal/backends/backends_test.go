package backends_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/terminal/backends"
	"github.com/charlesrobsampson/rig/internal/terminal/herdr"
	"github.com/charlesrobsampson/rig/internal/terminal/tmux"
)

func TestTheConfiguredBackendIsChosen(t *testing.T) {
	tests := []struct {
		name    string
		isTmux  bool
		isHerdr bool
		wantErr string
	}{
		{name: "", isHerdr: true},
		{name: "herdr", isHerdr: true},
		{name: "tmux", isTmux: true},
		{name: "zellij", wantErr: "zellij"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := backends.New(tt.name)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "tmux") {
					t.Errorf("error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			_, isTmux := b.(*tmux.Backend)
			_, isHerdr := b.(*herdr.Backend)
			if isTmux != tt.isTmux || isHerdr != tt.isHerdr {
				t.Errorf("got %T", b)
			}
		})
	}
}
