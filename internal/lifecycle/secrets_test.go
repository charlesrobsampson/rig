package lifecycle_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/config"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/lifecycle"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

func TestAMissingSecretFailsTheStartNamingItBeforeAnythingIsCreated(t *testing.T) {
	var asked []string
	f, dk, _ := agentsFixture(t, func(o *lifecycle.Options) {
		o.Secrets = func(names []string) (map[string]string, error) {
			asked = names
			return nil, errString("secrets not found: DB_PASSWORD, WORK_TOKEN (looked in macOS Keychain service \"rig\")")
		}
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"work": {Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}}}
	})
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("secrets: [DB_PASSWORD]\nauth: work\n")...), 0o600)

	_, err := f.start("demo", domain.SandboxContainer)
	if apiCode(t, err) != api.CodeInvalid || !strings.Contains(err.Error(), "DB_PASSWORD") || !strings.Contains(err.Error(), "WORK_TOKEN") {
		t.Fatalf("error = %v, want invalid naming both secrets", err)
	}
	if strings.Join(asked, ",") != "DB_PASSWORD,WORK_TOKEN" {
		t.Errorf("the resolver was asked for %v: the ticket's secrets and the profile's, once each, sorted", asked)
	}
	if exists(filepath.Join(path, "svc1")) {
		t.Error("a checkout was created")
	}
	if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Error("a network was created")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSecretPrecedenceFollowsServiceThenTicketThenDefaultProfile(t *testing.T) {
	var asked []string
	f, _, _ := agentsFixture(t, func(o *lifecycle.Options) {
		o.Secrets = func(names []string) (map[string]string, error) {
			if asked == nil { // the start's own check asks first, for everything
				asked = names
			}
			out := map[string]string{}
			for _, n := range names {
				out[n] = "v"
			}
			return out, nil
		}
		o.Global.Auth = config.Auth{Default: "base", Profiles: map[string]config.AuthProfile{
			"base":    {Type: domain.AuthAPIKey, Secrets: []string{"BASE_KEY"}},
			"ticket":  {Type: domain.AuthAPIKey, Secrets: []string{"TICKET_KEY"}},
			"service": {Type: domain.AuthAPIKey, Secrets: []string{"SERVICE_KEY"}},
		}}
	})
	r1, r2, r3 := testkit.NewRepo(t), testkit.NewRepo(t), testkit.NewRepo(t)
	path := f.create("demo", r1, r2, r3)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	doc := strings.Replace(string(raw), "      branch: rig/demo/svc2", "      auth: service\n      branch: rig/demo/svc2", 1)
	doc = strings.Replace(doc, "      branch: rig/demo/svc3", "      auth: \"\"\n      branch: rig/demo/svc3", 1)
	os.WriteFile(filepath.Join(path, "ticket.yaml"), []byte(doc+"auth: ticket\n"), 0o600)
	f.mustStart("demo", domain.SandboxContainer)
	// svc1 inherits the ticket's profile, svc2 has its own; svc3 explicitly sets none
	got := strings.Join(asked, ",")
	if got != "SERVICE_KEY,TICKET_KEY" {
		t.Errorf("secrets asked for = %s, want SERVICE_KEY,TICKET_KEY (the default profile loses to the ticket's)", got)
	}
}

func TestSecretValuesNeverReachStateEventsOrResults(t *testing.T) {
	const sentinel = "value-sentinel-9f3a"
	f, _, term := agentsFixture(t, func(o *lifecycle.Options) {
		o.Secrets = func(names []string) (map[string]string, error) {
			out := map[string]string{}
			for _, n := range names {
				out[n] = sentinel
			}
			return out, nil
		}
		o.Global.Auth = config.Auth{Profiles: map[string]config.AuthProfile{"work": {Type: domain.AuthOAuthToken, Secrets: []string{"WORK_TOKEN"}}}}
	})
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte("secrets: [DB_PASSWORD]\nauth: work\n")...), 0o600)
	f.events()
	res := f.mustStart("demo", domain.SandboxContainer)

	if got := term.Spawned()[0].Env["CLAUDE_CODE_OAUTH_TOKEN"]; got != sentinel {
		t.Fatalf("the agent did not get its token: %q", got)
	}
	encoded, _ := json.Marshal(res)
	var events []string
	for _, ev := range f.events() {
		events = append(events, string(ev.Data))
	}
	dryRun, err := f.lc.Start(ctx, api.TicketStartParams{ID: "demo", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	dry, _ := json.Marshal(dryRun)
	for what, text := range map[string]string{"the start result": string(encoded), "the events": strings.Join(events, "\n"), "the dry run": string(dry)} {
		if strings.Contains(text, sentinel) {
			t.Errorf("a secret value is in %s", what)
		}
	}
	err = filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || strings.Contains(p, "/svc1/") {
			return nil // the checkout is the agent's own workspace
		}
		if data, _ := os.ReadFile(p); strings.Contains(string(data), sentinel) {
			t.Errorf("a secret value was written to %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
