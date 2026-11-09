//go:build llm

package summary_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/summary"
)

// TestRealOllama talks to an Ollama server for real. Run it with `task test:llm`; it
// skips when no server answers or no model is installed. RIG_LLM_ENDPOINT and
// RIG_LLM_MODEL choose the server and model (default: localhost and the first model).
func TestRealOllama(t *testing.T) {
	endpoint := os.Getenv("RIG_LLM_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:11434"
	}
	model := os.Getenv("RIG_LLM_MODEL")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(endpoint + "/api/tags")
	if err != nil {
		t.Skipf("no Ollama at %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct{ Name string } `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&tags)
	if model == "" {
		if len(tags.Models) == 0 {
			t.Skip("Ollama has no models installed; pull one or set RIG_LLM_MODEL")
		}
		model = tags.Models[0].Name
	}
	s, err := summary.NewOllama(endpoint, model, summary.ProviderOptions{Timeout: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.Summarize(context.Background(), input())
	if err != nil || len(text) < 10 {
		t.Fatalf("summary %q, err %v", text, err)
	}
	t.Logf("%s said:\n%s", model, text)
}
