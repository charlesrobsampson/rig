package journal_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/journal"
)

func TestOnlyJSONObjectLinesAreRecords(t *testing.T) {
	for line, want := range map[string]bool{
		`{"kind":"feedback"}`:     true,
		`  {"kind":"feedback"}`:   true,
		`{broken`:                 true, // a record, and a bad one: never a state
		"working":                 false,
		"cost 0.42":               false,
		"":                        false,
		`["kind","feedback"]`:     false,
		`"{\"kind\":\"x\"}"`:      false,
		"\t{\"kind\":\"note\"}\r": true,
	} {
		if got := journal.IsRecordLine(line); got != want {
			t.Errorf("IsRecordLine(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestParseAcceptsWhatAnAgentWrites(t *testing.T) {
	tests := []struct {
		name, line string
		want       journal.Entry
	}{
		{"kind and text", `{"kind":"feedback","text":"npm registry blocked"}`, journal.Entry{Kind: "feedback", Text: "npm registry blocked"}},
		{"kind only", `{"kind":"checkpoint"}`, journal.Entry{Kind: "checkpoint"}},
		{"data object", `{"kind":"verdict","data":{"role":"reviewer","verdict":"PASS"}}`, journal.Entry{Kind: "verdict", Data: json.RawMessage(`{"role":"reviewer","verdict":"PASS"}`)}},
		{"null data is no data", `{"kind":"note","data":null}`, journal.Entry{Kind: "note"}},
		{"dotted and dashed kinds", `{"kind":"review.verdict-2_b"}`, journal.Entry{Kind: "review.verdict-2_b"}},
		{"escapes survive", `{"kind":"note","text":"a\\b \"q\" \n next"}`, journal.Entry{Kind: "note", Text: "a\\b \"q\" \n next"}},
		{"forged stamps are ignored", `{"kind":"note","ticket":"other","service":"x","at":"2001-01-01T00:00:00Z","source":"rig"}`, journal.Entry{Kind: "note"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := journal.Parse(tt.line)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != tt.want.Kind || got.Text != tt.want.Text || string(got.Data) != string(tt.want.Data) {
				t.Errorf("Parse = %+v (data %s), want %+v (data %s)", got, got.Data, tt.want, tt.want.Data)
			}
		})
	}
}

func TestParseRefusesBadRecords(t *testing.T) {
	tests := map[string]string{
		"not JSON":              `{kind: feedback}`,
		"no kind":               `{"text":"hello"}`,
		"empty kind":            `{"kind":""}`,
		"upper case kind":       `{"kind":"Feedback"}`,
		"kind with a space":     `{"kind":"my note"}`,
		"kind too long":         `{"kind":"` + strings.Repeat("a", 65) + `"}`,
		"kind not a string":     `{"kind":7}`,
		"text not a string":     `{"kind":"note","text":7}`,
		"data not an object":    `{"kind":"note","data":[1,2]}`,
		"data a string":         `{"kind":"note","data":"x"}`,
		"text too long":         `{"kind":"note","text":"` + strings.Repeat("a", journal.MaxTextBytes+1) + `"}`,
		"line too long":         `{"kind":"note","data":{"x":"` + strings.Repeat("a", journal.MaxLineBytes) + `"}}`,
		"trailing content":      `{"kind":"note"} {"kind":"note"}`,
		"invalid UTF-8 in text": "{\"kind\":\"note\",\"text\":\"\xff\"}",
	}
	for name, line := range tests {
		if e, err := journal.Parse(line); err == nil {
			t.Errorf("%s: Parse(%.60q) = %+v, want an error", name, line, e)
		}
	}
}

func record(at time.Time, ticket, service, kind, text string) api.JournalRecord {
	return api.JournalRecord{At: at, Ticket: ticket, Service: service, Source: api.JournalSourceContainer, Kind: kind, Text: text}
}

func TestAppendAndReadKeepOrderAndFields(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	first := record(at, "demo", "api", "feedback", "slow npm install")
	second := api.JournalRecord{At: at.Add(time.Minute), Ticket: "demo", Service: "web", Source: api.JournalSourceHost, Kind: "verdict", Data: json.RawMessage(`{"verdict":"PASS"}`)}
	for _, r := range []api.JournalRecord{first, second} {
		if err := journal.Append(dir, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := journal.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Text != "slow npm install" || got[1].Source != api.JournalSourceHost || string(got[1].Data) != `{"verdict":"PASS"}` || !got[1].At.Equal(second.At) {
		t.Errorf("read back %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, journal.FileName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("journal file mode: %v, %v; want 0600", info, err)
	}
}

func TestReadWithoutAJournalIsEmpty(t *testing.T) {
	got, err := journal.Read(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Errorf("Read = %v, %v", got, err)
	}
}

func TestReadSkipsADamagedLine(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	journal.Append(dir, record(at, "demo", "api", "note", "one"))
	f, _ := os.OpenFile(filepath.Join(dir, journal.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("{half a line\n")
	f.Close()
	journal.Append(dir, record(at, "demo", "api", "note", "two"))
	got, err := journal.Read(dir)
	if err != nil || len(got) != 2 || got[1].Text != "two" {
		t.Errorf("Read = %+v, %v; want the two good records", got, err)
	}
}

func TestOffsetsArePerServiceAndPersist(t *testing.T) {
	dir := t.TempDir()
	if got := journal.Offset(dir, "api"); got != 0 {
		t.Errorf("offset before any = %d", got)
	}
	if err := journal.SetOffset(dir, "api", 120); err != nil {
		t.Fatal(err)
	}
	if err := journal.SetOffset(dir, "web", 7); err != nil {
		t.Fatal(err)
	}
	if journal.Offset(dir, "api") != 120 || journal.Offset(dir, "web") != 7 {
		t.Errorf("offsets = %d, %d", journal.Offset(dir, "api"), journal.Offset(dir, "web"))
	}
	os.WriteFile(filepath.Join(dir, "journal.offsets.json"), []byte("garbage"), 0o600)
	if got := journal.Offset(dir, "api"); got != 0 {
		t.Errorf("a damaged offsets file must read as the start, got %d", got)
	}
}

func TestFilterAndLimit(t *testing.T) {
	at := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	records := []api.JournalRecord{
		record(at.Add(3*time.Minute), "b", "api", "feedback", "3"),
		record(at, "a", "api", "feedback", "0"),
		record(at.Add(time.Minute), "a", "web", "verdict", "1"),
		record(at.Add(2*time.Minute), "a", "api", "feedback", "2"),
	}
	texts := func(rs []api.JournalRecord) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Text)
		}
		return strings.Join(out, ",")
	}
	tests := []struct {
		name string
		p    api.JournalListParams
		want string
	}{
		{"everything, oldest first", api.JournalListParams{}, "0,1,2,3"},
		{"by kind", api.JournalListParams{Kind: "feedback"}, "0,2,3"},
		{"by service", api.JournalListParams{Service: "web"}, "1"},
		{"newest two", api.JournalListParams{Limit: 2}, "2,3"},
		{"limit over the count", api.JournalListParams{Limit: 9}, "0,1,2,3"},
		{"kind and limit", api.JournalListParams{Kind: "feedback", Limit: 1}, "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := texts(journal.Filter(records, tt.p)); got != tt.want {
				t.Errorf("Filter = %s, want %s", got, tt.want)
			}
		})
	}
}
