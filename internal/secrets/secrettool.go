package secrets

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

// SecretTool reads secrets from the freedesktop Secret Service (GNOME Keyring, KWallet,
// KeePassXC) through libsecret's `secret-tool`, the Linux counterpart of the macOS
// Keychain: items of attribute service=<Service>, account=<the secret's name>. Add one
// with:
//
//	secret-tool store --label="rig NAME" service rig account NAME
//
// (it prompts for the value, so it never appears in shell history).
//
// A machine without a Secret Service (a headless server, a container, no secret-tool
// installed) is not an error: the source finds nothing and the .env file is consulted.
type SecretTool struct {
	Service string
	// Run is the seam for tests; nil runs `secret-tool` from PATH.
	Run SecurityRunner
	// Disabled makes every lookup find nothing (not Linux).
	Disabled bool
}

// NewSecretTool returns a source that finds nothing on systems that are not Linux.
func NewSecretTool(service string) *SecretTool {
	return &SecretTool{Service: service, Disabled: runtime.GOOS != "linux"}
}

func (s *SecretTool) Describe() string {
	return fmt.Sprintf("the Secret Service (secret-tool) with service %q", s.Service)
}

// noServiceRe matches what secret-tool says when there is no Secret Service to ask: no
// D-Bus session, no keyring daemon. It is the headless machine, not a failed lookup.
var noServiceRe = regexp.MustCompile(`(?i)d-?bus|org\.freedesktop\.(secrets|dbus)|cannot autolaunch|no such interface|could not connect|not provided by any`)

func (s *SecretTool) Lookup(ctx context.Context, name string) (string, bool, error) {
	if s.Disabled {
		return "", false, nil
	}
	run := s.Run
	if run == nil {
		run = runSecretTool
	}
	stdout, stderr, code, err := run(ctx, "lookup", "service", s.Service, "account", name)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", false, nil // secret-tool is not installed: nothing to ask
	case err != nil:
		return "", false, err
	case code == 0:
		value := strings.TrimRight(stdout, "\n")
		if value == "" {
			return "", false, nil
		}
		return value, true, nil
	case strings.TrimSpace(stderr) == "":
		return "", false, nil // exit 1 with nothing to say is "no such item"
	case noServiceRe.MatchString(stderr):
		return "", false, nil // no Secret Service on this machine
	}
	return "", false, fmt.Errorf("secret-tool exited %d: %s", code, firstLine(stderr))
}

func runSecretTool(ctx context.Context, args ...string) (string, string, int, error) {
	return runCommand(ctx, "secret-tool", args...)
}
