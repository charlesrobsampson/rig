package domain_test

import (
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/domain"
)

func TestValidateTicketID(t *testing.T) {
	tests := []struct {
		id      string
		wantErr bool
	}{
		{"ticket-412", false},
		{"a", false},
		{"0abc", false},
		{"a-b-c", false},
		{strings.Repeat("a", 63), false},
		{"", true},
		{"Ticket", true},
		{"../etc", true},
		{"a/b", true},
		{"a b", true},
		{"-a", true},
		{"a_b", true},
		{".hidden", true},
		{strings.Repeat("a", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			err := domain.ValidateTicketID(tt.id)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateTicketID(%q) error = %v, wantErr %v", tt.id, err, tt.wantErr)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"backend", false},
		{"a", false},
		{"svc-1", false},
		{strings.Repeat("a", 31), false},
		{"", true},
		{"1svc", true},
		{"Back", true},
		{"a_b", true},
		{"a.b", true},
		{strings.Repeat("a", 32), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.ValidateName(tt.name)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}

func TestCheckUniqueNames(t *testing.T) {
	svc := func(names ...string) []domain.Service {
		var out []domain.Service
		for _, n := range names {
			out = append(out, domain.Service{Name: n})
		}
		return out
	}
	dep := func(names ...string) []domain.Dependency {
		var out []domain.Dependency
		for _, n := range names {
			out = append(out, domain.Dependency{Name: n})
		}
		return out
	}
	tests := []struct {
		name     string
		services []domain.Service
		deps     []domain.Dependency
		wantDup  string
	}{
		{"distinct", svc("api", "web"), dep("db"), ""},
		{"no dependencies", svc("api"), nil, ""},
		{"duplicate services", svc("api", "api"), nil, "api"},
		{"service and dependency share a namespace", svc("db"), dep("db"), "db"},
		{"duplicate dependencies", svc("api"), dep("db", "db"), "db"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := domain.CheckUniqueNames(tt.services, tt.deps)
			if tt.wantDup == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantDup) {
				t.Fatalf("error = %v, want one naming %q", err, tt.wantDup)
			}
		})
	}
}
