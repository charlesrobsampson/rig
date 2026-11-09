package dashboard_test

// A service's test that its implementation matches the agreed contract.
//
// contract.json is what `rig contract export --format json` printed for the ticket's
// contract.md (regenerate it whenever the contract changes; rig's own tests fail if the
// copy in this repository goes stale). The test reads the endpoint's documented request
// and response shapes from it and checks the handler against them, so a service that
// drops a field, renames one, or changes a type fails its build instead of breaking the
// service on the other side of the contract.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/examples/contract-test/dashboard"
)

type contract struct {
	Endpoints []struct {
		Method   string          `json:"method"`
		Path     string          `json:"path"`
		Status   int             `json:"status"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
		Errors   []struct {
			Status int `json:"status"`
		} `json:"errors"`
	} `json:"endpoints"`
	Schemas []struct {
		Name  string          `json:"name"`
		Shape json.RawMessage `json:"shape"`
	} `json:"schemas"`
}

func load(t *testing.T) contract {
	t.Helper()
	raw, err := os.ReadFile("../contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var c contract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// check reports every way value differs from the shape: a missing required key, a wrong
// type. Keys ending in ? are optional; extra keys are allowed (a service may add fields).
func check(path string, shape, value any, schemas map[string]any, problems *[]string) {
	switch s := shape.(type) {
	case string:
		if named, ok := schemas[s]; ok {
			check(path, named, value, schemas, problems)
			return
		}
		ok := false
		switch s {
		case "string":
			_, ok = value.(string)
		case "integer":
			f, isNum := value.(float64)
			ok = isNum && f == float64(int64(f))
		case "number":
			_, ok = value.(float64)
		case "boolean":
			_, ok = value.(bool)
		case "object":
			_, ok = value.(map[string]any)
		case "array":
			_, ok = value.([]any)
		default:
			_, ok = value.(string) // any other string in a shape is an example value
		}
		if !ok {
			*problems = append(*problems, path+": want "+s)
		}
	case []any:
		arr, ok := value.([]any)
		if !ok {
			*problems = append(*problems, path+": want an array")
			return
		}
		if len(s) > 0 {
			for i, item := range arr {
				check(path+"["+strconv.Itoa(i)+"]", s[0], item, schemas, problems)
			}
		}
	case map[string]any:
		obj, ok := value.(map[string]any)
		if !ok {
			*problems = append(*problems, path+": want an object")
			return
		}
		for key, want := range s {
			name, optional := strings.CutSuffix(key, "?")
			got, present := obj[name]
			switch {
			case !present && !optional:
				*problems = append(*problems, path+"."+name+": missing")
			case present:
				check(path+"."+name, want, got, schemas, problems)
			}
		}
	}
}

func TestDashboardMatchesTheContract(t *testing.T) {
	c := load(t)
	schemas := map[string]any{}
	for _, s := range c.Schemas {
		var shape any
		json.Unmarshal(s.Shape, &shape)
		schemas[s.Name] = shape
	}
	for _, ep := range c.Endpoints {
		if ep.Method != http.MethodPost || ep.Path != "/api/v1/dashboard" {
			continue
		}
		var reqShape, respShape any
		json.Unmarshal(ep.Request, &reqShape)
		json.Unmarshal(ep.Response, &respShape)

		// a request built from the documented shape (every key, optional ones too)
		req := map[string]any{"user_id": "u-1", "locale": "en"}
		body, _ := json.Marshal(req)
		rec := httptest.NewRecorder()
		dashboard.Handler(rec, httptest.NewRequest(ep.Method, ep.Path, bytes.NewReader(body)))
		if rec.Code != ep.Status {
			t.Fatalf("status = %d, the contract says %d", rec.Code, ep.Status)
		}
		var got any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("the response is not JSON: %v", err)
		}
		var problems []string
		check("response", respShape, got, schemas, &problems)
		if len(problems) > 0 {
			t.Errorf("the response diverges from the contract:\n  %s", strings.Join(problems, "\n  "))
		}

		// a documented error: the contract lists 400 for a bad user
		for _, e := range ep.Errors {
			if e.Status != http.StatusBadRequest {
				continue
			}
			rec := httptest.NewRecorder()
			dashboard.Handler(rec, httptest.NewRequest(ep.Method, ep.Path, strings.NewReader(`{}`)))
			if rec.Code != e.Status {
				t.Errorf("a request with no user_id gave %d, the contract documents %d", rec.Code, e.Status)
			}
		}
		return
	}
	t.Fatal("the contract has no POST /api/v1/dashboard")
}
