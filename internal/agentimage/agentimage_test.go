package agentimage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/agentimage"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

func fake(t *testing.T) *testkit.FakeDocker {
	t.Helper()
	d := testkit.NewFakeDocker()
	d.AddImage("golang:1.24")
	return d
}

func ensure(t *testing.T, b *agentimage.Builder, base string) (string, bool) {
	t.Helper()
	tag, built, err := b.Ensure(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	return tag, built
}

func TestTheImageIsBuiltOnceAndReused(t *testing.T) {
	d := fake(t)
	b := agentimage.New(d, "", nil)
	tag, built := ensure(t, b, "golang:1.24")
	if !built || !strings.HasPrefix(tag, "rig-agent:") || len(tag) != len("rig-agent:")+12 {
		t.Fatalf("tag = %q, built = %v", tag, built)
	}
	again, builtAgain := ensure(t, b, "golang:1.24")
	if builtAgain || again != tag {
		t.Errorf("second Ensure = %q, built %v; want the cached image", again, builtAgain)
	}
	if n := len(d.Builds()); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
	if ok, _ := d.ImageExists(ctx, tag); !ok {
		t.Error("the tagged image does not exist")
	}
}

func TestTheTagFollowsEverythingThatGoesIntoTheImage(t *testing.T) {
	cert := filepath.Join(t.TempDir(), "corp.pem")
	os.WriteFile(cert, []byte("CERT-ONE"), 0o600)
	d := fake(t)
	d.AddImage("golang:1.23")
	base := agentimage.New(d, "1.0.0", []string{cert})
	tagBase, _ := ensure(t, base, "golang:1.24")

	tests := []struct {
		name string
		tag  func() string
	}{
		{"another base image", func() string { tag, _ := ensure(t, base, "golang:1.23"); return tag }},
		{"another Claude version", func() string {
			tag, _ := ensure(t, agentimage.New(d, "1.0.1", []string{cert}), "golang:1.24")
			return tag
		}},
		{"no certificate", func() string { tag, _ := ensure(t, agentimage.New(d, "1.0.0", nil), "golang:1.24"); return tag }},
		{"a changed certificate", func() string {
			os.WriteFile(cert, []byte("CERT-TWO"), 0o600)
			defer os.WriteFile(cert, []byte("CERT-ONE"), 0o600)
			tag, _ := ensure(t, agentimage.New(d, "1.0.0", []string{cert}), "golang:1.24")
			return tag
		}},
		{"the base tag moving to a new image", func() string {
			d.BuildImage(ctx, docker.BuildSpec{Tag: "golang:1.24", ContextDir: dockerfileDir(t)}) // same tag, new content
			tag, _ := ensure(t, base, "golang:1.24")
			return tag
		}},
	}
	seen := map[string]string{tagBase: "the original"}
	for _, tt := range tests {
		tag := tt.tag()
		if prev, dup := seen[tag]; dup {
			t.Errorf("%s produced the same tag as %s: %s", tt.name, prev, tag)
		}
		seen[tag] = tt.name
	}
	if same, _ := ensure(t, agentimage.New(d, "1.0.0", []string{cert}), "golang:1.23"); same == "" {
		t.Fatal("unreachable")
	}
}

func dockerfileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o600)
	return dir
}

func TestTheBuildContextHasTheRecipeAndTheCertificates(t *testing.T) {
	cert := filepath.Join(t.TempDir(), "zscaler.pem")
	os.WriteFile(cert, []byte("-----BEGIN CERTIFICATE-----\nXYZ\n"), 0o600)
	d := fake(t)
	var spec docker.BuildSpec
	var recipe string
	var certs []string
	d.OnBuild(func(s docker.BuildSpec) error {
		spec = s
		raw, _ := os.ReadFile(filepath.Join(s.ContextDir, "Dockerfile"))
		recipe = string(raw)
		entries, _ := os.ReadDir(filepath.Join(s.ContextDir, "certs"))
		for _, e := range entries {
			content, _ := os.ReadFile(filepath.Join(s.ContextDir, "certs", e.Name()))
			certs = append(certs, e.Name()+"="+string(content))
		}
		return nil
	})
	ensure(t, agentimage.New(d, "1.2.3", []string{cert}), "golang:1.24")

	if spec.Args["BASE"] != "golang:1.24" || spec.Args["CLAUDE_VERSION"] != "1.2.3" {
		t.Errorf("build args = %v", spec.Args)
	}
	if spec.Labels["rig.role"] != "agent" || spec.Dockerfile != "" {
		t.Errorf("spec = %+v", spec)
	}
	if strings.Index(recipe, "COPY certs/") > strings.Index(recipe, "apt-get install") {
		t.Error("the certificates must be in place before packages are downloaded")
	}
	for _, want := range []string{"ARG BASE", "FROM ${BASE}", "apt-get", "apk add", "update-ca-certificates", "claude.ai/install.sh", "command -v claude", "ENV HOME=/home/agent", "WORKDIR /work", "ripgrep"} {
		if !strings.Contains(recipe, want) {
			t.Errorf("the recipe lacks %q", want)
		}
	}
	if strings.Join(certs, "|") != "keep=|rig-extra-0.crt=-----BEGIN CERTIFICATE-----\nXYZ\n" {
		t.Errorf("certs in the context = %q (update-ca-certificates needs the .crt extension)", certs)
	}
	if _, err := os.Stat(spec.ContextDir); !os.IsNotExist(err) {
		t.Error("the temporary build context was not removed")
	}
}

func TestAMissingCertificateOrBaseOrFailedBuildIsAClearError(t *testing.T) {
	d := fake(t)
	if _, _, err := agentimage.New(d, "", []string{"/no/such/ca.pem"}).Ensure(ctx, "golang:1.24"); err == nil || !strings.Contains(err.Error(), "/no/such/ca.pem") {
		t.Errorf("missing cert: %v", err)
	}
	if _, _, err := agentimage.New(d, "", nil).Ensure(ctx, ""); err == nil {
		t.Error("an empty base was accepted")
	}
	d.OnBuild(func(docker.BuildSpec) error { return errors.New("apk: no such package") })
	_, _, err := agentimage.New(d, "", nil).Ensure(ctx, "golang:1.24")
	if err == nil || !strings.Contains(err.Error(), "golang:1.24") || !strings.Contains(err.Error(), "no such package") || !strings.Contains(err.Error(), "docker.extra_ca_certs") {
		t.Errorf("failed build without certificates: %v (want a hint about docker.extra_ca_certs)", err)
	}
	cert := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(cert, []byte("CERT"), 0o600)
	_, _, err = agentimage.New(d, "", []string{cert}).Ensure(ctx, "golang:1.24")
	if err == nil || strings.Contains(err.Error(), "docker.extra_ca_certs") {
		t.Errorf("failed build with certificates configured: %v (the hint would not help)", err)
	}
	d.Down(errors.New("daemon stopped"))
	if _, _, err := agentimage.New(d, "", nil).Ensure(ctx, "golang:1.24"); !errors.Is(err, docker.ErrUnavailable) {
		t.Errorf("docker down: %v", err)
	}
}

func TestABaseThatIsNotLocalYetIsPulledByTheBuild(t *testing.T) {
	d := testkit.NewFakeDocker() // knows no images
	tag, built, err := agentimage.New(d, "", nil).Ensure(ctx, "alpine:3.22")
	if err != nil || !built || tag == "" {
		t.Fatalf("Ensure = %q, %v, %v", tag, built, err)
	}
	again, builtAgain, _ := agentimage.New(d, "", nil).Ensure(ctx, "alpine:3.22")
	if again != tag || builtAgain {
		t.Errorf("a second Ensure = %q (built %v), want the cached %q", again, builtAgain, tag)
	}
}

func TestConcurrentEnsuresBuildOnce(t *testing.T) {
	d := fake(t)
	b := agentimage.New(d, "", nil)
	var wg sync.WaitGroup
	tags := make([]string, 8)
	for i := range tags {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tags[i], _, _ = b.Ensure(ctx, "golang:1.24")
		}()
	}
	wg.Wait()
	if n := len(d.Builds()); n != 1 {
		t.Errorf("%d builds for eight concurrent callers, want 1", n)
	}
	for _, tag := range tags {
		if tag != tags[0] || tag == "" {
			t.Errorf("tags = %v", tags)
			break
		}
	}
}
