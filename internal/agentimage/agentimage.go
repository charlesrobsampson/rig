// Package agentimage builds the image agents run in: a service's toolchain image plus
// git, the corporate CA certificates and Claude Code. The result is cached under a tag
// derived from everything that goes into it, so it is rebuilt exactly when the base
// image, the Claude version, the certificates or this recipe change.
package agentimage

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/charlesrobsampson/rig/internal/docker"
)

//go:embed Dockerfile
var dockerfile []byte

type Builder struct {
	d             docker.Docker
	claudeVersion string
	certs         []string

	mu sync.Mutex // one build at a time, so two tickets starting together do not both build
}

// New returns a Builder. claudeVersion is what the installer is asked for ("latest"
// when empty); certs are PEM files baked into the image's trust store.
func New(d docker.Docker, claudeVersion string, certs []string) *Builder {
	if claudeVersion == "" {
		claudeVersion = "latest"
	}
	return &Builder{d: d, claudeVersion: claudeVersion, certs: certs}
}

// Ensure returns the tag of the agent image for base, building it first if it does not
// exist yet.
func (b *Builder) Ensure(ctx context.Context, base string) (tag string, built bool, err error) {
	if base == "" {
		return "", false, errors.New("an agent image needs a base image")
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	certData := make([][]byte, len(b.certs))
	for i, path := range b.certs {
		if certData[i], err = os.ReadFile(path); err != nil {
			return "", false, fmt.Errorf("read CA certificate %s: %w", path, err)
		}
	}

	// A base that is not local yet is pulled by the build; until then it is known by name.
	baseID, err := b.d.ImageID(ctx, base)
	if errors.Is(err, docker.ErrNotFound) {
		baseID, err = "ref:"+base, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect the base image %s: %w", base, err)
	}

	h := sha256.New()
	for _, part := range [][]byte{[]byte(baseID), []byte(b.claudeVersion), dockerfile} {
		h.Write([]byte(fmt.Sprintf("%d:", len(part))))
		h.Write(part)
	}
	for _, c := range certData {
		h.Write([]byte(fmt.Sprintf("%d:", len(c))))
		h.Write(c)
	}
	tag = "rig-agent:" + hex.EncodeToString(h.Sum(nil))[:12]

	if ok, err := b.d.ImageExists(ctx, tag); err != nil {
		return "", false, err
	} else if ok {
		return tag, false, nil
	}

	dir, err := os.MkdirTemp("", "rig-agent-image")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), dockerfile, 0o600); err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		return "", false, err
	}
	// update-ca-certificates only takes files ending in .crt.
	os.WriteFile(filepath.Join(dir, "certs", "keep"), nil, 0o600)
	for i, c := range certData {
		if err := os.WriteFile(filepath.Join(dir, "certs", fmt.Sprintf("rig-extra-%d.crt", i)), c, 0o600); err != nil {
			return "", false, err
		}
	}

	err = b.d.BuildImage(ctx, docker.BuildSpec{
		Tag: tag, ContextDir: dir,
		Args:   map[string]string{"BASE": base, "CLAUDE_VERSION": b.claudeVersion},
		Labels: map[string]string{docker.LabelRole: docker.RoleAgent, "rig.agent-image": "1"},
	})
	if err != nil {
		hint := ""
		if len(b.certs) == 0 {
			hint = "\nrig hint: if this network inspects TLS (a corporate proxy), set docker.extra_ca_certs in the rig config to the proxy's root certificate so the image can download packages"
		}
		return "", false, fmt.Errorf("build the agent image on %s: %w%s", base, err, hint)
	}
	return tag, true, nil
}
