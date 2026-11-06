// Package store keeps the daemon's runtime records, one JSON document per ticket.
// It is a cache: state.json in each ticket directory stays the source of truth, so a
// lost or stale store can always be rebuilt.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/fsutil"
)

const schemaVersion = 1

var ErrNotFound = errors.New("record not found")

type Store interface {
	Get(id string) (json.RawMessage, error)
	Put(id string, rec json.RawMessage) error
	Delete(id string) error
	List() ([]string, error)
	// Update applies every change made through the Tx, or none if fn returns an error.
	Update(fn func(Tx) error) error
}

type Tx interface {
	Get(id string) (json.RawMessage, error)
	Put(id string, rec json.RawMessage) error
	Delete(id string) error
	List() []string
}

// FileStore keeps all records in one file (compacted JSON), rewritten atomically under a file lock, so
// several goroutines or processes can share it. Reads need no lock because a rename
// swaps the whole file at once.
type FileStore struct {
	path string
}

var _ Store = (*FileStore)(nil)

type document struct {
	SchemaVersion *int                       `json:"schema_version"`
	Records       map[string]json.RawMessage `json:"records"`
}

// OpenFile prepares the store at path. A missing file is an empty store; an
// unreadable or wrong-version file is an error.
func OpenFile(path string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	s := &FileStore{path: path}
	if _, err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) load() (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store %s: %w", s.path, err)
	}
	var doc document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read store %s: %w", s.path, err)
	}
	switch {
	case doc.SchemaVersion == nil:
		return nil, fmt.Errorf("store %s has no schema version", s.path)
	case *doc.SchemaVersion < schemaVersion:
		return nil, fmt.Errorf("store %s has schema version %d, older than the supported %d; it is a cache, so remove it to rebuild", s.path, *doc.SchemaVersion, schemaVersion)
	case *doc.SchemaVersion > schemaVersion:
		return nil, fmt.Errorf("store %s has schema version %d, newer than the supported %d; upgrade rig", s.path, *doc.SchemaVersion, schemaVersion)
	}
	if doc.Records == nil {
		doc.Records = map[string]json.RawMessage{}
	}
	return doc.Records, nil
}

func (s *FileStore) save(records map[string]json.RawMessage) error {
	v := schemaVersion
	raw, err := json.Marshal(document{SchemaVersion: &v, Records: records})
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.path, raw)
}

func (s *FileStore) Get(id string) (json.RawMessage, error) {
	records, err := s.load()
	if err != nil {
		return nil, err
	}
	return getFrom(records, id)
}

func (s *FileStore) List() ([]string, error) {
	records, err := s.load()
	if err != nil {
		return nil, err
	}
	return idsOf(records), nil
}

func (s *FileStore) Put(id string, rec json.RawMessage) error {
	return s.Update(func(tx Tx) error { return tx.Put(id, rec) })
}

func (s *FileStore) Delete(id string) error {
	return s.Update(func(tx Tx) error { return tx.Delete(id) })
}

func (s *FileStore) Update(fn func(Tx) error) error {
	return fsutil.WithLock(s.path+".lock", func() error {
		records, err := s.load()
		if err != nil {
			return err
		}
		if err := fn(&tx{records: records}); err != nil {
			return err
		}
		return s.save(records)
	})
}

type tx struct{ records map[string]json.RawMessage }

func (t *tx) Get(id string) (json.RawMessage, error) { return getFrom(t.records, id) }
func (t *tx) List() []string                         { return idsOf(t.records) }
func (t *tx) Delete(id string) error                 { delete(t.records, id); return nil }

func (t *tx) Put(id string, rec json.RawMessage) error {
	if err := domain.ValidateTicketID(id); err != nil {
		return err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, rec); err != nil {
		return fmt.Errorf("record %q is not valid JSON: %w", id, err)
	}
	t.records[id] = compact.Bytes()
	return nil
}

func getFrom(records map[string]json.RawMessage, id string) (json.RawMessage, error) {
	rec, ok := records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return append(json.RawMessage(nil), rec...), nil
}

func idsOf(records map[string]json.RawMessage) []string {
	ids := make([]string, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
