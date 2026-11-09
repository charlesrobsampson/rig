package egress

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/charlesrobsampson/rig/internal/docker"
)

// The proxy binaries (static, linux) are put here by `task build:egress` and embedded
// in the rig binary, so rig stays a single file. A build without them still compiles
// (the README keeps the pattern valid) and says what to do when a ticket needs them.
//
//go:embed all:embedded
var embedded embed.FS

// BinaryFinder returns the proxy binary for a linux architecture ("amd64", "arm64").
type BinaryFinder func(arch string) ([]byte, error)

// EmbeddedBinary is the BinaryFinder for what is embedded in this build.
func EmbeddedBinary(arch string) ([]byte, error) {
	data, err := fs.ReadFile(embedded, "embedded/rig-egress-linux-"+arch)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("this rig was built without the egress proxy for linux/%s; build rig with `task build` (or `task release`), which embeds it", arch)
	}
	return data, err
}

// Images builds the proxy's Docker image: FROM scratch plus the static binary, tagged by
// the binary's content so it is built once and rebuilt only when the proxy changes.
type Images struct {
	d      docker.Docker
	binary BinaryFinder
	arch   string
	mu     sync.Mutex
}

// NewImages returns an Images for the Docker daemon's architecture (the host's, which
// is where Docker Desktop's VM runs); a nil finder means the embedded binaries.
func NewImages(d docker.Docker, binary BinaryFinder, arch string) *Images {
	if binary == nil {
		binary = EmbeddedBinary
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	return &Images{d: d, binary: binary, arch: arch}
}

const dockerfile = `FROM scratch
COPY rig-egress /rig-egress
USER 65534:65534
EXPOSE 3128
ENTRYPOINT ["/rig-egress"]
`

// Ensure returns the proxy image's tag, building it first if it does not exist.
func (i *Images) Ensure(ctx context.Context) (tag string, built bool, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	bin, err := i.binary(i.arch)
	if err != nil {
		return "", false, err
	}
	sum := sha256.Sum256(append(bin, dockerfile...))
	tag = "rig-egress:" + hex.EncodeToString(sum[:])[:12]
	if ok, err := i.d.ImageExists(ctx, tag); err != nil {
		return "", false, err
	} else if ok {
		return tag, false, nil
	}

	dir, err := os.MkdirTemp("", "rig-egress-image")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "rig-egress"), bin, 0o755); err != nil {
		return "", false, err
	}
	err = i.d.BuildImage(ctx, docker.BuildSpec{Tag: tag, ContextDir: dir, Labels: map[string]string{docker.LabelRole: docker.RoleProxy, "rig.egress-image": "1"}})
	if err != nil {
		return "", false, fmt.Errorf("build the egress proxy image: %w", err)
	}
	return tag, true, nil
}
