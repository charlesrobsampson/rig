package lifecycle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOnlyPathsInsideTheTicketsDirectoryAreTrusted(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "tickets")
	inside := filepath.Join(root, "demo", "svc1")
	outside := filepath.Join(base, "elsewhere")
	sneaky := filepath.Join(root, "demo", "link-out")
	for _, d := range []string{inside, outside} {
		os.MkdirAll(d, 0o700)
	}
	os.Symlink(outside, sneaky)

	if got, err := trustablePaths(root, inside); err != nil || len(got) != 1 || got[0] != inside {
		t.Errorf("inside: %v, %v", got, err)
	}
	for name, p := range map[string]string{"outside": outside, "a link leading out": sneaky, "the parent": base} {
		if _, err := trustablePaths(root, inside, p); err == nil {
			t.Errorf("%s (%s) was accepted", name, p)
		}
	}
}
