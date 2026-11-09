package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// notFoundExit is what `security find-generic-password` exits with for a missing item.
const notFoundExit = 44

// SecurityRunner runs the macOS `security` tool and returns its stdout, stderr and exit code.
type SecurityRunner func(ctx context.Context, args ...string) (stdout, stderr string, exitCode int, err error)

// Keychain reads generic passwords of one service from the macOS Keychain, the account
// being the secret's name. Add one with:
//
//	security add-generic-password -s rig -a NAME -w
//
// (the tool prompts for the value, so it never appears in shell history).
type Keychain struct {
	Service string
	// Run is the seam for tests; nil runs /usr/bin/security.
	Run SecurityRunner
	// Disabled makes every lookup find nothing (not macOS).
	Disabled bool
}

// NewKeychain returns a Keychain source that finds nothing on systems without one.
func NewKeychain(service string) *Keychain {
	return &Keychain{Service: service, Disabled: runtime.GOOS != "darwin"}
}

func (k *Keychain) Describe() string { return fmt.Sprintf("macOS Keychain service %q", k.Service) }

func (k *Keychain) Lookup(ctx context.Context, name string) (string, bool, error) {
	if k.Disabled {
		return "", false, nil
	}
	run := k.Run
	if run == nil {
		run = runSecurity
	}
	// -w prints only the password. The value is read from stdout, never passed on a
	// command line.
	stdout, stderr, code, err := run(ctx, "find-generic-password", "-s", k.Service, "-a", name, "-w")
	if err != nil {
		return "", false, err
	}
	switch {
	case code == 0:
		return strings.TrimRight(stdout, "\n"), true, nil
	case code == notFoundExit || strings.Contains(stderr, "could not be found"):
		return "", false, nil
	}
	return "", false, fmt.Errorf("security exited %d: %s", code, firstLine(stderr))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func runSecurity(ctx context.Context, args ...string) (string, string, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/usr/bin/security", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0, nil
	case ctx.Err() != nil:
		return "", "", 0, ctx.Err()
	case errors.As(err, &exitErr):
		return stdout.String(), stderr.String(), exitErr.ExitCode(), nil
	}
	return "", "", 0, err
}
