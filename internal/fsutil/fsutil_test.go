package fsutil_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/fsutil"
)

func TestAtomicWrite(t *testing.T) {
	t.Run("creates the file with owner-only permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.json")
		if err := fsutil.AtomicWrite(path, []byte("hello")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, want 0600", info.Mode().Perm())
		}
		if got, _ := os.ReadFile(path); string(got) != "hello" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("replaces existing content and leaves no temp files", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "a.json")
		for _, content := range []string{"first", "second"} {
			if err := fsutil.AtomicWrite(path, []byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if got, _ := os.ReadFile(path); string(got) != "second" {
			t.Errorf("content = %q, want second", got)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("directory has %d entries, want only the target file", len(entries))
		}
	})

	t.Run("fails cleanly when the directory is missing", func(t *testing.T) {
		if err := fsutil.AtomicWrite(filepath.Join(t.TempDir(), "missing", "a.json"), []byte("x")); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestWithLockSerializesReadModifyWrite(t *testing.T) {
	dir := t.TempDir()
	counter, lock := filepath.Join(dir, "counter"), filepath.Join(dir, "counter.lock")
	if err := os.WriteFile(counter, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	const workers = 40
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := fsutil.WithLock(lock, func() error {
				raw, err := os.ReadFile(counter)
				if err != nil {
					return err
				}
				n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
				return os.WriteFile(counter, []byte(strconv.Itoa(n+1)), 0o600)
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile(counter)
	if string(raw) != strconv.Itoa(workers) {
		t.Errorf("counter = %s, want %d (lost updates)", raw, workers)
	}
}

func TestWithLockPropagatesTheCallbackError(t *testing.T) {
	want := os.ErrInvalid
	if got := fsutil.WithLock(filepath.Join(t.TempDir(), "x.lock"), func() error { return want }); got != want {
		t.Errorf("error = %v, want %v", got, want)
	}
}
