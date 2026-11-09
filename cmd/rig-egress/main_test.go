package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestPolicyFromEnvironment(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	tests := []struct {
		name    string
		env     map[string]string
		hosts   []string
		ports   []int
		wantErr string
	}{
		{"defaults", map[string]string{}, nil, []int{443}, ""},
		{"hosts and ports", map[string]string{"ALLOW_HOSTS": " API.anthropic.com, *.npmjs.org ,,", "ALLOW_PORTS": "443, 8443"}, []string{"api.anthropic.com", "*.npmjs.org"}, []int{443, 8443}, ""},
		{"a bad port", map[string]string{"ALLOW_PORTS": "https"}, nil, nil, "not a port number"},
		{"a bad host", map[string]string{"ALLOW_HOSTS": "bad host"}, nil, nil, "not a hostname"},
		{"a port out of range", map[string]string{"ALLOW_PORTS": "99999"}, nil, nil, "between 1 and 65535"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := policyFromEnv(env(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(p.Hosts, tt.hosts) || !reflect.DeepEqual(p.Ports, tt.ports) || p.AllowPrivate {
				t.Errorf("policy = %+v, %v", p, err)
			}
		})
	}
	if p, _ := policyFromEnv(env(map[string]string{"ALLOW_PRIVATE": "1"})); !p.AllowPrivate {
		t.Error("ALLOW_PRIVATE=1 was ignored")
	}
}
