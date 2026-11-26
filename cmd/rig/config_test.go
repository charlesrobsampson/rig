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

func TestConfigShowResolvedSaysWhereEachKeyCameFrom(t *testing.T) {
	user := t.TempDir()
	t.Setenv("HOME", user)
	t.Setenv("RIG_HOME", "")
	t.Setenv("RIG_CONFIG", "")
	machine := filepath.Join(user, ".config", "rig", "config.json")
	os.MkdirAll(filepath.Dir(machine), 0o700)
	os.WriteFile(machine, []byte(`{"limits":{"max_active_tickets":3},"docker":{"agent_base":"m:1"}}`), 0o600)
	vessel := filepath.Join(t.TempDir(), "v.json")
	os.WriteFile(vessel, []byte(`{"docker":{"agent_base":"v:1"}}`), 0o600)

	for _, args := range [][]string{{"config", "show", "--resolved", "--config", vessel}, {"config", "--resolved", "--config", vessel}} {
		out, err := runRig(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Config struct {
				Limits struct {
					Max int `json:"max_active_tickets"`
				} `json:"limits"`
			} `json:"config"`
			Sources struct {
				Files map[string]string `json:"files"`
				Keys  map[string]string `json:"keys"`
			} `json:"sources"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err, out)
		}
		if got.Config.Limits.Max != 3 || got.Sources.Keys["limits.max_active_tickets"] != "machine" ||
			got.Sources.Keys["docker.agent_base"] != "vessel" || got.Sources.Files["machine"] != machine {
			t.Errorf("%v: %s", args, out)
		}
	}
}
