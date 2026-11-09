package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/testkit"
)

const exportable = "# Contract: demo\n\n## Agreed\nThe dashboard API.\n\n" +
	"```endpoint\nPOST /api/v1/dashboard\nrequest: {\"user_id\": \"string\"}\nresponse: {\"widgets\": [\"Widget\"]}\nerrors: 400 invalid_user\n```\n\n" +
	"```schema\nWidget\n{\"id\": \"integer\"}\n```\n\n## Proposed changes\n\n## Changelog\n"

func writeContractFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contract.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExportWorksOnAFileWithoutADaemon(t *testing.T) {
	rigEnv(t) // no daemon is started: --file never needs one
	path := writeContractFile(t, exportable)

	stdout, stderr, code := rig(t, deps{}, "contract", "export", "--file", path)
	if code != 0 || !strings.Contains(stdout, "openapi: 3.0.3") || !strings.Contains(stdout, "/api/v1/dashboard:") || !strings.Contains(stdout, "invalid_user") || !strings.Contains(stdout, "#/components/schemas/Widget") {
		t.Fatalf("yaml: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	stdout, _, code = rig(t, deps{}, "contract", "export", "--file", path, "--format", "openapi-json")
	var doc map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &doc) != nil || doc["openapi"] != "3.0.3" {
		t.Errorf("openapi-json: code %d, %q", code, stdout)
	}
	stdout, _, code = rig(t, deps{}, "contract", "export", "--file", path, "--format", "json")
	var spec struct {
		Endpoints []struct{ Method, Path string }
		Schemas   []struct{ Name string }
	}
	if code != 0 || json.Unmarshal([]byte(stdout), &spec) != nil || len(spec.Endpoints) != 1 || spec.Endpoints[0].Method != "POST" || len(spec.Schemas) != 1 {
		t.Errorf("json: code %d, %q", code, stdout)
	}
	out := filepath.Join(t.TempDir(), "contract.openapi.json")
	if stdout, _, code := rig(t, deps{}, "contract", "export", "--file", path, "--format", "openapi-json", "-o", out); code != 0 || stdout != "" {
		t.Errorf("-o: code %d, stdout %q", code, stdout)
	}
	if raw, _ := os.ReadFile(out); !strings.Contains(string(raw), "openapi") {
		t.Errorf("the output file = %q", raw)
	}
}

func TestExportOfATicketsContractGoesThroughTheDaemon(t *testing.T) {
	home, d, repo := newEnv(t)
	newTicket(t, d, "multi", repo, testkit.NewRepo(t))
	os.WriteFile(filepath.Join(home, "tickets", "multi", "contract.md"), []byte(exportable), 0o600)
	stdout, stderr, code := rig(t, d, "contract", "export", "multi", "--format", "openapi-json")
	if code != 0 || !strings.Contains(stdout, "/api/v1/dashboard") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stdout, _, code := rig(t, d, "contract", "check", "multi"); code != 0 || !strings.Contains(stdout, "multi/contract.md: ok, 1 endpoint(s), 1 schema(s)") {
		t.Errorf("check: code %d, %q", code, stdout)
	}
	// a single-service ticket has no contract
	newTicket(t, d, "solo", testkit.NewRepo(t))
	if _, _, code := rig(t, d, "contract", "export", "solo"); code != 5 {
		t.Errorf("no contract: code %d, want 5", code)
	}
}

func TestMalformedBlocksFailWithLineNumbersAndWriteNothing(t *testing.T) {
	rigEnv(t)
	bad := "# C\n\n## Agreed\n```endpoint\nFETCH /x\n```\n\n```schema\nOk\n{\"a\": \"string\"}\n```\n\n## Proposed changes\n\n## Changelog\n"
	path := writeContractFile(t, bad)
	for _, args := range [][]string{{"export"}, {"export", "--format", "json"}, {"check"}} {
		stdout, stderr, code := rig(t, deps{}, append([]string{"contract"}, append(args, "--file", path)...)...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, path+":5: ") || !strings.Contains(stderr, "not an HTTP method") || !strings.Contains(stderr, "1 problem(s)") {
			t.Errorf("%v: code %d, stdout %q, stderr %q", args, code, stdout, stderr)
		}
	}
	// the file itself being malformed (a proposal without a reason) fails too
	badProposal := "# C\n\n## Agreed\n\n## Proposed changes\n### P-a-1 · from a · open\n**What:** only what\n\n## Changelog\n"
	if _, stderr, code := rig(t, deps{}, "contract", "check", "--file", writeContractFile(t, badProposal)); code != 2 || !strings.Contains(stderr, "Why") {
		t.Errorf("a malformed proposal: code %d, stderr %q", code, stderr)
	}
}

func TestAContractWithNoBlocksExportsAnEmptyDocumentWithANote(t *testing.T) {
	rigEnv(t)
	path := writeContractFile(t, "# C\n\n## Agreed\nJust prose.\n\n## Proposed changes\n\n## Changelog\n")
	stdout, stderr, code := rig(t, deps{}, "contract", "export", "--file", path, "--format", "openapi-json")
	if code != 0 || !strings.Contains(stdout, `"paths": {}`) || !strings.Contains(stderr, "has no endpoint or schema blocks") {
		t.Errorf("code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if stdout, _, code := rig(t, deps{}, "contract", "check", "--file", path); code != 0 || !strings.Contains(stdout, "ok, 0 endpoint(s), 0 schema(s)") {
		t.Errorf("check: code %d, %q", code, stdout)
	}
}

func TestExportUsageErrors(t *testing.T) {
	rigEnv(t)
	path := writeContractFile(t, exportable)
	for name, args := range map[string][]string{
		"no source":         {"contract", "export"},
		"both sources":      {"contract", "export", "demo", "--file", path},
		"an unknown format": {"contract", "export", "--file", path, "--format", "xml"},
		"a missing file":    {"contract", "export", "--file", "/no/such/contract.md"},
	} {
		_, _, code := rig(t, deps{}, args...)
		want := 2
		if name == "a missing file" {
			want = 1
		}
		if code != want {
			t.Errorf("%s: code %d, want %d", name, code, want)
		}
	}
}

func TestTheExampleContractJSONIsCurrent(t *testing.T) {
	rigEnv(t)
	want, _, code := rig(t, deps{}, "contract", "export", "--file", "../../examples/contract-test/contract.md", "--format", "json")
	if code != 0 {
		t.Fatalf("export failed: code %d", code)
	}
	got, err := os.ReadFile("../../examples/contract-test/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("examples/contract-test/contract.json is stale: regenerate it with\n  rig contract export --file examples/contract-test/contract.md --format json --output examples/contract-test/contract.json")
	}
}
