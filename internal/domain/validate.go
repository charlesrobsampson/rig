package domain

import (
	"fmt"
	"regexp"
)

var (
	ticketIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	namePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// ValidateTicketID keeps ids safe to use as directory names, network names, and labels.
func ValidateTicketID(id string) error {
	if !ticketIDPattern.MatchString(id) {
		return fmt.Errorf("invalid ticket id %q: use 1-63 lowercase letters, digits, or dashes, starting with a letter or digit", id)
	}
	return nil
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("invalid name %q: use 1-31 lowercase letters, digits, or dashes, starting with a letter", name)
	}
	return nil
}

// CheckUniqueNames treats services and dependencies as one namespace, because both
// become container hostnames on the ticket network.
func CheckUniqueNames(services []Service, deps []Dependency) error {
	seen := map[string]string{}
	add := func(kind, name string) error {
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("duplicate name %q: used by a %s and a %s", name, prev, kind)
		}
		seen[name] = kind
		return nil
	}
	for _, s := range services {
		if err := add("service", s.Name); err != nil {
			return err
		}
	}
	for _, d := range deps {
		if err := add("dependency", d.Name); err != nil {
			return err
		}
	}
	return nil
}
