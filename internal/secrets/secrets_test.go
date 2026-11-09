package secrets_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/secrets"
)

var ctx = context.Background()

type fakeSource struct {
	name   string
	values map[string]string
	err    error
	asked  []string
}

func (f *fakeSource) Describe() string { return f.name }
func (f *fakeSource) Lookup(_ context.Context, name string) (string, bool, error) {
	f.asked = append(f.asked, name)
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.values[name]
	return v, ok, nil
}

func TestTheFirstSourceThatHasANameWins(t *testing.T) {
	keychain := &fakeSource{name: "keychain", values: map[string]string{"A": "from-keychain", "B": "only-keychain"}}
	envfile := &fakeSource{name: "env file", values: map[string]string{"A": "from-env", "C": "only-env"}}
	got, err := secrets.New(keychain, envfile).Resolve(ctx, []string{"A", "B", "C", "A"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "from-keychain", "B": "only-keychain", "C": "only-env"}
	if len(got) != 3 || got["A"] != want["A"] || got["B"] != want["B"] || got["C"] != want["C"] {
		t.Errorf("got %v, want %v", got, want)
	}
	if strings.Join(keychain.asked, ",") != "A,B,C" {
		t.Errorf("the keychain was asked %v; a repeated name is looked up once", keychain.asked)
	}
	if strings.Join(envfile.asked, ",") != "C" {
		t.Errorf("the env file was asked %v; it is only for what the keychain lacks", envfile.asked)
	}
}

func TestMissingSecretsAreNamedTogetherWithWhereWeLooked(t *testing.T) {
	r := secrets.New(&fakeSource{name: `macOS Keychain service "rig"`}, &fakeSource{name: "env file /home/u/.env", values: map[string]string{"HAVE": "x"}})
	got, err := r.Resolve(ctx, []string{"ZED", "HAVE", "ALPHA"})
	if got != nil || err == nil {
		t.Fatalf("Resolve = %v, %v; want no partial result and an error", got, err)
	}
	for _, want := range []string{"ALPHA, ZED", `macOS Keychain service "rig"`, "env file /home/u/.env", ", then "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "HAVE") {
		t.Errorf("a secret that was found is named as missing: %v", err)
	}
	if !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("a missing secret is not errors.Is ErrNotFound: %v", err)
	}
	failing := secrets.New(&fakeSource{name: "k", err: errors.New("locked")})
	if _, err := failing.Resolve(ctx, []string{"X"}); errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("a source that failed looks like a missing secret: %v", err)
	}
	if _, err := secrets.New().Resolve(ctx, []string{"X"}); err == nil || !strings.Contains(err.Error(), "no secret source") {
		t.Errorf("with no sources: %v", err)
	}
	if got, err := secrets.New().Resolve(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("no names = %v, %v", got, err)
	}
}

func TestASourceFailureIsReportedWithoutAnyValue(t *testing.T) {
	good := &fakeSource{name: "good", values: map[string]string{"A": "value-sentinel"}}
	bad := &fakeSource{name: "the keychain", err: errors.New("user cancelled the prompt")}
	_, err := secrets.New(bad, good).Resolve(ctx, []string{"A"})
	if err == nil || !strings.Contains(err.Error(), "the keychain") || !strings.Contains(err.Error(), "user cancelled") || !strings.Contains(err.Error(), "A") {
		t.Errorf("error = %v", err)
	}
	if strings.Contains(err.Error(), "value-sentinel") {
		t.Error("a value is in the error")
	}
}

func TestNoErrorEverContainsAValue(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(envPath, []byte("PRESENT=value-sentinel-1\n"), 0o600)
	k := &secrets.Keychain{Service: "rig", Run: func(context.Context, ...string) (string, string, int, error) {
		return "value-sentinel-2\n", "boom", 1, nil // a failing call that still printed something
	}}
	_, err := secrets.New(k, &secrets.EnvFile{Path: envPath}).Resolve(ctx, []string{"MISSING", "PRESENT"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "value-sentinel") {
		t.Errorf("error = %v", err)
	}
}

func TestKeychainLookups(t *testing.T) {
	var gotArgs []string
	run := func(code int, stdout, stderr string) secrets.SecurityRunner {
		return func(_ context.Context, args ...string) (string, string, int, error) {
			gotArgs = args
			return stdout, stderr, code, nil
		}
	}
	tests := []struct {
		name      string
		run       secrets.SecurityRunner
		wantValue string
		wantFound bool
		wantErr   string
	}{
		{"found", run(0, "s3cret\n", ""), "s3cret", true, ""},
		{"a value with spaces and an equals sign", run(0, "a b=c\n", ""), "a b=c", true, ""},
		{"not found by exit code", run(44, "", "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain."), "", false, ""},
		{"not found by message", run(1, "", "The specified item could not be found in the keychain."), "", false, ""},
		{"another failure", run(36, "", "User interaction is not allowed.\nmore"), "", false, "exited 36: User interaction is not allowed."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := &secrets.Keychain{Service: "rig", Run: tt.run}
			value, found, err := k.Lookup(ctx, "MY_SECRET")
			if value != tt.wantValue || found != tt.wantFound || (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Errorf("Lookup = %q, %v, %v", value, found, err)
			}
			if strings.Join(gotArgs, " ") != "find-generic-password -s rig -a MY_SECRET -w" {
				t.Errorf("security was run with %q", gotArgs)
			}
		})
	}

	disabled := &secrets.Keychain{Service: "rig", Disabled: true, Run: func(context.Context, ...string) (string, string, int, error) {
		t.Error("security was run on a system without a keychain")
		return "", "", 0, nil
	}}
	if _, found, err := disabled.Lookup(ctx, "X"); found || err != nil {
		t.Errorf("disabled keychain = %v, %v", found, err)
	}
	failing := &secrets.Keychain{Service: "rig", Run: func(context.Context, ...string) (string, string, int, error) {
		return "", "", 0, errors.New("no security tool")
	}}
	if _, _, err := failing.Lookup(ctx, "X"); err == nil {
		t.Error("a tool that cannot run must be an error, not 'not found'")
	}
}

func TestEnvFileParsing(t *testing.T) {
	doc := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"  SPACED  =  padded value  ",
		`DOUBLE="with # hash and = sign"`,
		`SINGLE='single quoted'`,
		"export EXPORTED=exp",
		"TRAILING=kept # but this is a comment",
		"EMPTY=",
		"EQUALS=a=b=c",
		"OVERRIDDEN=first",
		"OVERRIDDEN=second",
		"1BAD=ignored",
		"no equals sign",
		`UNCLOSED="oops`,
	}, "\n")
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte(doc), 0o600)
	e := &secrets.EnvFile{Path: path}
	tests := map[string]struct {
		value string
		found bool
	}{
		"PLAIN": {"value", true}, "SPACED": {"padded value", true}, "DOUBLE": {"with # hash and = sign", true},
		"SINGLE": {"single quoted", true}, "EXPORTED": {"exp", true}, "TRAILING": {"kept", true}, "EMPTY": {"", true},
		"EQUALS": {"a=b=c", true}, "OVERRIDDEN": {"second", true}, "1BAD": {"", false}, "ABSENT": {"", false},
		"UNCLOSED": {`"oops`, true},
	}
	for name, want := range tests {
		value, found, err := e.Lookup(ctx, name)
		if err != nil || value != want.value || found != want.found {
			t.Errorf("%s = %q, %v, %v; want %q, %v", name, value, found, err, want.value, want.found)
		}
	}
}

func TestEnvFilePermissionsAndAbsence(t *testing.T) {
	dir := t.TempDir()
	if _, found, err := (&secrets.EnvFile{Path: filepath.Join(dir, "missing.env")}).Lookup(ctx, "X"); found || err != nil {
		t.Errorf("a missing file = %v, %v; it simply has nothing", found, err)
	}
	if _, found, err := (&secrets.EnvFile{}).Lookup(ctx, "X"); found || err != nil {
		t.Errorf("no path = %v, %v", found, err)
	}
	open := filepath.Join(dir, "open.env")
	os.WriteFile(open, []byte("X=1\n"), 0o644)
	if _, _, err := (&secrets.EnvFile{Path: open}).Lookup(ctx, "X"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a world-readable file = %v, want a refusal that says how to fix it", err)
	}
	if err := os.Chmod(open, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&secrets.EnvFile{Path: open}).Lookup(ctx, "X"); err == nil {
		t.Error("a group-readable file was accepted")
	}
	if err := os.Chmod(open, 0o400); err != nil {
		t.Fatal(err)
	}
	if v, found, err := (&secrets.EnvFile{Path: open}).Lookup(ctx, "X"); err != nil || !found || v != "1" {
		t.Errorf("a read-only owner file = %q, %v, %v", v, found, err)
	}
}
