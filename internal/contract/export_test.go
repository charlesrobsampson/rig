package contract_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/contract"
	"gopkg.in/yaml.v3"
)

func md(agreed string) string {
	return "# Contract: demo\n\n## Agreed\n" + agreed + "\n\n## Proposed changes\n\n## Changelog\n"
}

func spec(t *testing.T, agreed string) contract.Spec {
	t.Helper()
	return contract.Parse(md(agreed)).Spec()
}

func problemLines(s contract.Spec) []int {
	var out []int
	for _, p := range s.Problems {
		out = append(out, p.Line)
	}
	return out
}

const fullContract = "Prose.\n\n" +
	"```endpoint\nPOST /api/v1/dashboard/{id}\nsummary: build a dashboard\nstatus: 201\nrequest: {\"user_id\": \"string\", \"locale?\": \"string\", \"tags\": [\"string\"]}\nresponse: {\"widgets\": [\"Widget\"], \"count\": 3, \"ratio\": 0.5, \"ok\": true, \"note\": null}\nerrors: 400 invalid_user, 404 not_found\n```\n\n" +
	"```endpoint\nGET /api/v1/ping\n```\n\n" +
	"```schema\nWidget\n{\"id\": \"integer\", \"label\": \"string\"}\n```\n"

func TestAContractWithNoBlocksHasAnEmptySpec(t *testing.T) {
	for name, agreed := range map[string]string{"nothing": "", "prose only": "Just words.\n\n```go\nfunc main() {}\n```\n"} {
		s := spec(t, agreed)
		if len(s.Endpoints) != 0 || len(s.Schemas) != 0 || len(s.Problems) != 0 || s.Endpoints == nil || s.Problems == nil {
			t.Errorf("%s: %+v", name, s)
		}
		doc := s.OpenAPI()
		if len(doc["paths"].(map[string]any)) != 0 || doc["components"] != nil {
			t.Errorf("%s: openapi = %v", name, doc)
		}
	}
}

func TestEndpointsAndSchemasAreExtracted(t *testing.T) {
	s := spec(t, fullContract)
	if len(s.Problems) != 0 {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if len(s.Endpoints) != 2 || len(s.Schemas) != 1 {
		t.Fatalf("endpoints %d, schemas %d", len(s.Endpoints), len(s.Schemas))
	}
	e := s.Endpoints[0]
	if e.Method != "POST" || e.Path != "/api/v1/dashboard/{id}" || e.Status != 201 || e.Summary != "build a dashboard" || len(e.Errors) != 2 || e.Errors[1].Status != 404 || e.Errors[1].Code != "not_found" {
		t.Errorf("endpoint = %+v", e)
	}
	if ping := s.Endpoints[1]; ping.Status != 200 || ping.Request != nil || ping.Response != nil {
		t.Errorf("a bare endpoint = %+v", ping)
	}
	if s.Schemas[0].Name != "Widget" {
		t.Errorf("schema = %+v", s.Schemas[0])
	}
}

func TestOpenAPIOutputMapsShapesToJSONSchema(t *testing.T) {
	raw, err := contract.Parse(md(fullContract)).Spec().MarshalOpenAPIJSON()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.0.3" || doc["info"].(map[string]any)["title"] != "Contract: demo" {
		t.Errorf("header = %v %v", doc["openapi"], doc["info"])
	}
	op := doc["paths"].(map[string]any)["/api/v1/dashboard/{id}"].(map[string]any)["post"].(map[string]any)
	params := op["parameters"].([]any)
	if len(params) != 1 || params[0].(map[string]any)["name"] != "id" || params[0].(map[string]any)["in"] != "path" {
		t.Errorf("parameters = %v", params)
	}
	req := op["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)
	if req["type"] != "object" || len(req["required"].([]any)) != 2 || req["required"].([]any)[0] != "tags" || req["required"].([]any)[1] != "user_id" {
		t.Errorf("request schema = %v; locale? is optional so only tags and user_id are required", req)
	}
	props := req["properties"].(map[string]any)
	if props["user_id"].(map[string]any)["type"] != "string" || props["locale"] == nil || props["locale?"] != nil || props["tags"].(map[string]any)["type"] != "array" {
		t.Errorf("properties = %v", props)
	}
	responses := op["responses"].(map[string]any)
	if responses["201"] == nil || responses["200"] != nil || responses["400"].(map[string]any)["description"] != "invalid_user" || responses["404"] == nil {
		t.Errorf("responses = %v", responses)
	}
	resp := responses["201"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	if resp["widgets"].(map[string]any)["items"].(map[string]any)["$ref"] != "#/components/schemas/Widget" {
		t.Errorf("widgets = %v; a schema name should be a $ref", resp["widgets"])
	}
	if c := resp["count"].(map[string]any); c["type"] != "integer" || c["example"] != float64(3) {
		t.Errorf("count = %v", c)
	}
	if r := resp["ratio"].(map[string]any); r["type"] != "number" {
		t.Errorf("ratio = %v", r)
	}
	if resp["ok"].(map[string]any)["type"] != "boolean" || resp["note"].(map[string]any)["nullable"] != true {
		t.Errorf("ok/note = %v %v", resp["ok"], resp["note"])
	}
	if ping := doc["paths"].(map[string]any)["/api/v1/ping"].(map[string]any)["get"].(map[string]any); ping["requestBody"] != nil || ping["responses"].(map[string]any)["200"] == nil {
		t.Errorf("ping = %v", ping)
	}
	widget := doc["components"].(map[string]any)["schemas"].(map[string]any)["Widget"].(map[string]any)
	if widget["properties"].(map[string]any)["id"].(map[string]any)["type"] != "integer" {
		t.Errorf("Widget = %v", widget)
	}
}

func TestTheYAMLFormIsTheSameDocument(t *testing.T) {
	s := contract.Parse(md(fullContract)).Spec()
	rawJSON, _ := s.MarshalOpenAPIJSON()
	rawYAML, err := s.MarshalOpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	var fromYAML, fromJSON map[string]any
	if err := yaml.Unmarshal(rawYAML, &fromYAML); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(rawJSON, &fromJSON)
	a, _ := json.Marshal(normalize(fromYAML))
	b, _ := json.Marshal(normalize(fromJSON))
	if string(a) != string(b) {
		t.Errorf("the YAML and JSON documents differ:\n%s\n%s", a, b)
	}
	if !strings.HasPrefix(string(rawYAML), "components:") && !strings.Contains(string(rawYAML), "openapi: 3.0.3") {
		t.Errorf("yaml = %s", rawYAML)
	}
}

// normalize makes numbers comparable: YAML decodes whole numbers as int.
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[k] = normalize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalize(val)
		}
		return out
	case int:
		return float64(x)
	}
	return v
}

func TestInvalidBlocksAreReportedWithTheirLineNumbers(t *testing.T) {
	// line 1: "# Contract: demo", 2 blank, 3: "## Agreed", so the agreed text starts on line 4
	agreed := strings.Join([]string{
		"```endpoint",                  // 4
		"FETCH /x",                     // 5: not a method
		"```",                          // 6
		"",                             // 7
		"```endpoint",                  // 8
		"GET api/no-slash",             // 9: bad path
		"```",                          // 10
		"",                             // 11
		"```endpoint",                  // 12
		"POST /ok",                     // 13
		"request: {\"a\": ",            // 14: broken JSON
		"status: abc",                  // 15
		"errors: 400 bad extra tokens", // 16
		"colour: red",                  // 17: unknown field
		"```",                          // 18
		"",                             // 19
		"```schema",                    // 20
		"not a name!",                  // 21
		"{}",                           // 22
		"```",                          // 23
		"",                             // 24
		"```schema",                    // 25
		"Thing",                        // 26
		"{\"a\": \"string\"} trailing", // 27
		"```",                          // 28
		"",                             // 29
		"```endpoint",                  // 30
		"```",                          // 31: empty
	}, "\n")
	s := spec(t, agreed)
	want := map[int]string{
		5:  "not an HTTP method",
		9:  "must start with /",
		14: "request: not valid JSON",
		15: "status must be an HTTP status",
		16: "not an error like",
		17: "not a field of the endpoint",
		21: "not a schema name",
		27: "not valid JSON",
		30: "the endpoint block is empty",
	}
	got := map[int]string{}
	for _, p := range s.Problems {
		got[p.Line] += p.Message + "|"
	}
	for line, text := range want {
		if !strings.Contains(got[line], text) {
			t.Errorf("line %d: %q, want it to mention %q (all: %v)", line, got[line], text, s.Problems)
		}
	}
	if len(s.Endpoints) != 0 || len(s.Schemas) != 0 {
		t.Errorf("an invalid block was used: %+v / %+v", s.Endpoints, s.Schemas)
	}
	// problems come in file order
	lines := problemLines(s)
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("problems out of order: %v", lines)
		}
	}
}

func TestOneBadBlockDoesNotHideTheGoodOnes(t *testing.T) {
	agreed := "```endpoint\nBOGUS /x\n```\n\n```endpoint\nGET /fine\nresponse: {\"a\": \"string\"}\n```\n\n```schema\nOk\n{\"id\": \"integer\"}\n```\n"
	s := spec(t, agreed)
	if len(s.Problems) != 1 || len(s.Endpoints) != 1 || s.Endpoints[0].Path != "/fine" || len(s.Schemas) != 1 {
		t.Errorf("spec = %+v", s)
	}
}

func TestDuplicatesAreReported(t *testing.T) {
	agreed := "```endpoint\nGET /x\n```\n\n```endpoint\nGET /x\nresponse: {}\n```\n\n```endpoint\nPOST /x\n```\n\n```schema\nA\n{}\n```\n\n```schema\nA\n{\"b\": 1}\n```\n"
	s := spec(t, agreed)
	if len(s.Endpoints) != 2 || len(s.Schemas) != 1 || len(s.Problems) != 2 {
		t.Fatalf("spec = %+v", s)
	}
	if !strings.Contains(s.Problems[0].Message, "GET /x is described twice (first at line 4)") || !strings.Contains(s.Problems[1].Message, `schema "A" is defined twice`) {
		t.Errorf("problems = %+v", s.Problems)
	}
}

func TestMultilineShapesAndRepeatedErrorLines(t *testing.T) {
	agreed := "```endpoint\nPUT /things/{id}\nrequest: {\n  \"name\": \"string\",\n  \"size\": \"integer\"\n}\nresponse: {\"ok\": \"boolean\"}\nerrors: 400 bad_name\nerrors: 409 conflict, 500\n```\n"
	s := spec(t, agreed)
	if len(s.Problems) != 0 || len(s.Endpoints) != 1 {
		t.Fatalf("spec = %+v", s)
	}
	e := s.Endpoints[0]
	if m := e.Request.(map[string]any); len(m) != 2 {
		t.Errorf("request = %v", e.Request)
	}
	if len(e.Errors) != 3 || e.Errors[2].Status != 500 || e.Errors[2].Code != "" {
		t.Errorf("errors = %+v", e.Errors)
	}
}

func TestStringsThatAreNotTypeNamesAreExamples(t *testing.T) {
	raw, _ := contract.Parse(md("```endpoint\nGET /x\nresponse: {\"currency\": \"EUR\", \"kind\": \"object\"}\n```\n")).Spec().MarshalOpenAPIJSON()
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	props := doc["paths"].(map[string]any)["/x"].(map[string]any)["get"].(map[string]any)["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["properties"].(map[string]any)
	if c := props["currency"].(map[string]any); c["type"] != "string" || c["example"] != "EUR" {
		t.Errorf("currency = %v", c)
	}
	if k := props["kind"].(map[string]any); k["type"] != "object" || k["example"] != nil {
		t.Errorf("kind = %v", k)
	}
}

func TestOnlyTheAgreedSectionCountsNotProposals(t *testing.T) {
	text := "# C\n\n## Agreed\n```endpoint\nGET /agreed\n```\n\n## Proposed changes\n### P-a-1 · from a · open\n**What:**\n```endpoint\nGET /proposed\n```\n**Why:** y\n\n## Changelog\n"
	s := contract.Parse(text).Spec()
	if len(s.Endpoints) != 1 || s.Endpoints[0].Path != "/agreed" {
		t.Errorf("endpoints = %+v; a proposal's blocks are not agreed yet", s.Endpoints)
	}
}
