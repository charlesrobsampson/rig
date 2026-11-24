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

// DefaultHome is the name of the home the default config describes.
const DefaultHome = "default"

// DefaultConfigFile is the config used when none is chosen.
func DefaultConfigFile(userHome string) string {
	return filepath.Join(userHome, ".config", "rig", "config.json")
}

// HomesFile keeps the named homes, next to the default config.
func HomesFile(userHome string) string {
	return filepath.Join(userHome, ".config", "rig", "homes.json")
}

// Homes maps a home's name to its config file.
type Homes map[string]string

// NamedHome is one home a person can choose by name.
type NamedHome struct {
	Name   string `json:"name"`
	Config string `json:"config"`
}

var homeName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// CheckHomeName says why a name cannot be given to a home, or nil.
func CheckHomeName(name string) error {
	switch {
	case name == DefaultHome:
		return fmt.Errorf("%q is the name of the home the default config describes", DefaultHome)
	case !homeName.MatchString(name):
		return fmt.Errorf("invalid home name %q (letters, digits, - and _)", name)
	}
	return nil
}

// LoadHomes reads the named homes; none registered is an empty map.
func LoadHomes(userHome string) (Homes, error) {
	path := HomesFile(userHome)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Homes{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file struct {
		Homes Homes `json:"homes"`
	}
	if err := decodeStrict(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Homes == nil {
		file.Homes = Homes{}
	}
	return file.Homes, nil
}

// SaveHomes writes the named homes, replacing the file whole.
func SaveHomes(userHome string, h Homes) error {
	path := HomesFile(userHome)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(struct {
		Homes Homes `json:"homes"`
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

// All is every home by name, sorted, with the default config as DefaultHome when its
// file exists.
func (h Homes) All(userHome string) []NamedHome {
	var out []NamedHome
	for name, config := range h {
		out = append(out, NamedHome{Name: name, Config: config})
	}
	if _, err := os.Stat(DefaultConfigFile(userHome)); err == nil {
		out = append(out, NamedHome{Name: DefaultHome, Config: DefaultConfigFile(userHome)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
