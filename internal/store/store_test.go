package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/store"
)

func open(t *testing.T) (*store.FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := store.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func rec(s string) json.RawMessage { return json.RawMessage(s) }

func TestPutGetListDelete(t *testing.T) {
	s, _ := open(t)

	if _, err := s.Get("ticket-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get on an empty store = %v, want ErrNotFound", err)
	}
	for id, doc := range map[string]string{"ticket-b": `{"n":2}`, "ticket-a": `{"n":1}`, "ticket-c": `{"n":3}`} {
		if err := s.Put(id, rec(doc)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get("ticket-b")
	if err != nil || string(got) != `{"n":2}` {
		t.Fatalf("Get = %s, %v", got, err)
	}
	ids, err := s.List()
	if err != nil || !reflect.DeepEqual(ids, []string{"ticket-a", "ticket-b", "ticket-c"}) {
		t.Fatalf("List = %v, %v (want sorted ids)", ids, err)
	}

	if err := s.Put("ticket-b", rec(`{"n":20}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("ticket-b"); string(got) != `{"n":20}` {
		t.Errorf("Put did not replace: %s", got)
	}

	if err := s.Delete("ticket-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("ticket-b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted record still found: %v", err)
	}
	if err := s.Delete("ticket-b"); err != nil {
		t.Errorf("deleting a missing record should be a no-op, got %v", err)
	}
}

func TestPutRejectsBadInput(t *testing.T) {
	s, _ := open(t)
	tests := []struct {
		name string
		id   string
		doc  string
	}{
		{"invalid json", "ticket-1", `{not json`},
		{"bad ticket id", "../escape", `{}`},
		{"empty id", "", `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.Put(tt.id, rec(tt.doc)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestDataSurvivesReopening(t *testing.T) {
	s, path := open(t)
	if err := s.Put("ticket-1", rec(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	again, err := store.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Get("ticket-1"); err != nil || string(got) != `{"a":1}` {
		t.Fatalf("after reopen: %s, %v", got, err)
	}
}

func TestUpdateIsAllOrNothing(t *testing.T) {
	s, _ := open(t)
	if err := s.Put("ticket-a", rec(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}

	t.Run("applies every change together", func(t *testing.T) {
		err := s.Update(func(tx store.Tx) error {
			if err := tx.Put("ticket-a", rec(`{"v":2}`)); err != nil {
				return err
			}
			got, err := tx.Get("ticket-a")
			if err != nil || string(got) != `{"v":2}` {
				t.Errorf("the transaction does not see its own write: %s, %v", got, err)
			}
			if err := tx.Put("ticket-b", rec(`{"v":10}`)); err != nil {
				return err
			}
			return tx.Delete("ticket-a")
		})
		if err != nil {
			t.Fatal(err)
		}
		ids, _ := s.List()
		if !reflect.DeepEqual(ids, []string{"ticket-b"}) {
			t.Errorf("ids after update = %v", ids)
		}
	})

	t.Run("an error leaves the store untouched", func(t *testing.T) {
		boom := errors.New("boom")
		err := s.Update(func(tx store.Tx) error {
			tx.Put("ticket-z", rec(`{}`))
			tx.Delete("ticket-b")
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want boom", err)
		}
		ids, _ := s.List()
		if !reflect.DeepEqual(ids, []string{"ticket-b"}) {
			t.Errorf("a failed update changed the store: %v", ids)
		}
	})
}

func TestConcurrentWritersDoNotLoseUpdatesOrCorruptTheFile(t *testing.T) {
	s, path := open(t)
	if err := s.Put("counter", rec(`0`)); err != nil {
		t.Fatal(err)
	}
	const writers = 30
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(func(tx store.Tx) error {
				raw, err := tx.Get("counter")
				if err != nil {
					return err
				}
				n, _ := strconv.Atoi(string(raw))
				return tx.Put("counter", rec(strconv.Itoa(n+1)))
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got, _ := s.Get("counter"); string(got) != strconv.Itoa(writers) {
		t.Errorf("counter = %s, want %d", got, writers)
	}
	raw, _ := os.ReadFile(path)
	if !json.Valid(raw) {
		t.Errorf("store file is not valid JSON: %s", raw)
	}
}

func TestFileIsOwnerOnlyAndVersioned(t *testing.T) {
	s, path := open(t)
	if err := s.Put("ticket-1", rec(`{}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
	}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &doc); err != nil || doc.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, %v", doc.SchemaVersion, err)
	}
}

func TestLeftoverTempFilesFromACrashAreIgnored(t *testing.T) {
	s, path := open(t)
	if err := s.Put("ticket-1", rec(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".tmp-crashed", []byte(`{"schema_version":1,"records":{"ticket-9":{"half":`), 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := store.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := again.List()
	if err != nil || !reflect.DeepEqual(ids, []string{"ticket-1"}) {
		t.Fatalf("List = %v, %v; the partial temp file leaked in", ids, err)
	}
	if err := again.Put("ticket-2", rec(`{}`)); err != nil {
		t.Errorf("writes broke after a crash left a temp file: %v", err)
	}
}

func TestUnsupportedSchemaVersionsErrorClearly(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"older", `{"schema_version":0,"records":{}}`, "older"},
		{"newer", `{"schema_version":99,"records":{}}`, "newer"},
		{"missing", `{"records":{}}`, "schema version"},
		{"corrupt", `{"schema_version":1,"records":`, "store.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.json")
			if err := os.WriteFile(path, []byte(tt.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.OpenFile(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}
