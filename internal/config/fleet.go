package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// DefaultVessel is the name of the vessel the default config describes.
const DefaultVessel = "default"

// DefaultConfigFile is the config used when none is chosen.
func DefaultConfigFile(userHome string) string {
	return filepath.Join(userHome, ".config", "rig", "config.json")
}

// FleetFile keeps the named vessels, next to the default config.
func FleetFile(userHome string) string {
	return filepath.Join(userHome, ".config", "rig", "fleet.json")
}

// Fleet maps a vessel's name to its config file. A vessel is one rig instance: a
// config with its own home directory, daemon, agents and record.
type Fleet map[string]string

// NamedVessel is one vessel a person can choose by name.
type NamedVessel struct {
	Name   string `json:"name"`
	Config string `json:"config"`
}

var vesselName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// CheckVesselName says why a name cannot be given to a vessel, or nil.
func CheckVesselName(name string) error {
	switch {
	case name == DefaultVessel:
		return fmt.Errorf("%q is the name of the vessel the default config describes", DefaultVessel)
	case !vesselName.MatchString(name):
		return fmt.Errorf("invalid vessel name %q (letters, digits, - and _)", name)
	}
	return nil
}

// LoadFleet reads the named vessels; none registered is an empty map.
func LoadFleet(userHome string) (Fleet, error) {
	path := FleetFile(userHome)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Fleet{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Vessels Fleet `json:"vessels"`
	}
	if err := decodeStrict(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Vessels == nil {
		file.Vessels = Fleet{}
	}
	return file.Vessels, nil
}

// SaveFleet writes the named vessels, replacing the file whole.
func SaveFleet(userHome string, h Fleet) error {
	path := FleetFile(userHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Vessels Fleet `json:"vessels"`
	}{h}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// All is every vessel by name, sorted, with the default config as DefaultVessel when its
// file exists.
func (h Fleet) All(userHome string) []NamedVessel {
	var out []NamedVessel
	for name, config := range h {
		out = append(out, NamedVessel{Name: name, Config: config})
	}
	if _, err := os.Stat(DefaultConfigFile(userHome)); err == nil {
		out = append(out, NamedVessel{Name: DefaultVessel, Config: DefaultConfigFile(userHome)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
