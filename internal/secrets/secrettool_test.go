package secrets_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/secrets"
)

func TestSecretToolLookups(t *testing.T) {
	run := func(code int, stdout, stderr string) secrets.SecurityRunner {
		return func(context.Context, ...string) (string, string, int, error) { return stdout, stderr, code, nil }
	}
	tests := []struct {
		name      string
		runner    secrets.SecurityRunner
		wantValue string
		wantFound bool
		wantErr   string
	}{
		{"a found secret", run(0, "s3cr3t\n", ""), "s3cr3t", true, ""},
		{"a value with trailing newlines trimmed, inner kept", run(0, "line1\nline2\n\n", ""), "line1\nline2", true, ""},
		{"a found but empty value counts as missing", run(0, "", ""), "", false, ""},
		{"no such item: exit 1 and nothing said", run(1, "", ""), "", false, ""},
		{"no such item: whitespace only", run(1, "", "  \n"), "", false, ""},
		{"no D-Bus session (headless)", run(1, "", "secret-tool: Cannot autolaunch D-Bus without X11 $DISPLAY"), "", false, ""},
		{"no keyring daemon", run(1, "", "secret-tool: The name org.freedesktop.secrets was not provided by any .service files"), "", false, ""},
		{"a failure that is not about the service being absent", run(2, "", "secret-tool: something broke\nsecond line"), "", false, "secret-tool exited 2: secret-tool: something broke"},
		{"secret-tool not installed", func(context.Context, ...string) (string, string, int, error) {
			return "", "", 0, exec.ErrNotFound
		}, "", false, ""},
		{"the tool cannot run for another reason", func(context.Context, ...string) (string, string, int, error) {
			return "", "", 0, errors.New("permission denied")
		}, "", false, "permission denied"},
	}
	for _, tt := range tests {
		s := &secrets.SecretTool{Service: "rig", Run: tt.runner}
		value, found, err := s.Lookup(context.Background(), "API_KEY")
		if value != tt.wantValue || found != tt.wantFound || (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: value %q, found %v, err %v; want %q, %v, %q", tt.name, value, found, err, tt.wantValue, tt.wantFound, tt.wantErr)
		}
	}
}

func TestSecretToolAsksForTheRightItemAndNeverPutsValuesInErrors(t *testing.T) {
	var got []string
	s := &secrets.SecretTool{Service: "myrig", Run: func(_ context.Context, args ...string) (string, string, int, error) {
		got = args
		return "super-secret-value", "secret-tool: failed with super-secret-value in it", 3, nil
	}}
	_, _, err := s.Lookup(context.Background(), "DB_PASSWORD")
	if strings.Join(got, " ") != "lookup service myrig account DB_PASSWORD" {
		t.Errorf("secret-tool was run with %v", got)
	}
	if err == nil {
		t.Fatal("a failure was swallowed")
	}
	// the error carries secret-tool's first stderr line, which is its own diagnostic, and
	// never stdout (where a value would be)
	if strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("the secret's name leaked into the error: %v", err)
	}
	if !strings.Contains(s.Describe(), "myrig") || !strings.Contains(s.Describe(), "secret-tool") {
		t.Errorf("Describe = %q", s.Describe())
	}
}

func TestSecretToolIsDisabledOffLinuxAndWorksInAResolver(t *testing.T) {
	off := &secrets.SecretTool{Service: "rig", Disabled: true, Run: func(context.Context, ...string) (string, string, int, error) {
		t.Error("a disabled source ran a command")
		return "", "", 0, nil
	}}
	if _, found, err := off.Lookup(context.Background(), "X"); found || err != nil {
		t.Errorf("disabled: %v %v", found, err)
	}

	store := &secrets.SecretTool{Service: "rig", Run: func(_ context.Context, args ...string) (string, string, int, error) {
		if args[len(args)-1] == "FROM_KEYRING" {
			return "kr-value\n", "", 0, nil
		}
		return "", "", 1, nil
	}}
	env := &fakeSource{name: ".env", values: map[string]string{"FROM_ENV": "env-value"}}
	got, err := secrets.New(store, env).Resolve(context.Background(), []string{"FROM_KEYRING", "FROM_ENV"})
	if err != nil || got["FROM_KEYRING"] != "kr-value" || got["FROM_ENV"] != "env-value" {
		t.Errorf("resolved %v, %v: the Secret Service first, then the .env file", got, err)
	}
	// a headless machine falls through to .env instead of failing
	headless := &secrets.SecretTool{Service: "rig", Run: func(context.Context, ...string) (string, string, int, error) {
		return "", "secret-tool: Cannot autolaunch D-Bus without X11 $DISPLAY", 1, nil
	}}
	if got, err := secrets.New(headless, env).Resolve(context.Background(), []string{"FROM_ENV"}); err != nil || got["FROM_ENV"] != "env-value" {
		t.Errorf("headless: %v, %v", got, err)
	}
}
