package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func runRig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCmd(deps{})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestConfigShowPrintsTheEffectiveConfigAsJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RIG_HOME", "")
	t.Setenv("RIG_CONFIG", "")
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"limits":{"max_active_tickets":4}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		env  string
	}{
		{"--config flag", []string{"config", "show", "--config", file}, ""},
		{"RIG_CONFIG", []string{"config", "show"}, file},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RIG_CONFIG", tt.env)
			out, err := runRig(t, tt.args...)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Limits struct {
					MaxActiveTickets int `json:"max_active_tickets"`
				} `json:"limits"`
				Defaults struct {
					Harness string `json:"harness"`
				} `json:"defaults"`
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if got.Limits.MaxActiveTickets != 4 || got.Defaults.Harness != "claude-code" {
				t.Errorf("effective config = %+v", got)
			}
		})
	}
}

func TestConfigShowExplainsABrokenConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(file, []byte(`{"limits":{"max_active_tickets":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runRig(t, "config", "show", "--config", file); err == nil {
		t.Fatal("a config with max_active_tickets 0 should fail")
	}
}
