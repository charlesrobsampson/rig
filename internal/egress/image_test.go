package egress_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/egress"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

func finder(content string) egress.BinaryFinder {
	return func(arch string) ([]byte, error) { return []byte(content + "-" + arch), nil }
}

func TestTheProxyImageIsBuiltOnceFromTheBinary(t *testing.T) {
	d := testkit.NewFakeDocker()
	var recipe, binary string
	var mode os.FileMode
	d.OnBuild(func(s docker.BuildSpec) error {
		raw, _ := os.ReadFile(filepath.Join(s.ContextDir, "Dockerfile"))
		recipe = string(raw)
		bin, _ := os.ReadFile(filepath.Join(s.ContextDir, "rig-egress"))
		binary = string(bin)
		info, _ := os.Stat(filepath.Join(s.ContextDir, "rig-egress"))
		mode = info.Mode().Perm()
		return nil
	})
	images := egress.NewImages(d, finder("proxy-v1"), "arm64")
	tag, built, err := images.Ensure(ctx)
	if err != nil || !built || !strings.HasPrefix(tag, "rig-egress:") || len(tag) != len("rig-egress:")+12 {
		t.Fatalf("Ensure = %q, %v, %v", tag, built, err)
	}
	if binary != "proxy-v1-arm64" || mode != 0o755 {
		t.Errorf("the context holds %q with mode %o", binary, mode)
	}
	for _, want := range []string{"FROM scratch", "COPY rig-egress /rig-egress", "USER 65534:65534", `ENTRYPOINT ["/rig-egress"]`} {
		if !strings.Contains(recipe, want) {
			t.Errorf("the recipe lacks %q:\n%s", want, recipe)
		}
	}
	again, builtAgain, _ := images.Ensure(ctx)
	if again != tag || builtAgain || len(d.Builds()) != 1 {
		t.Errorf("second Ensure = %q (built %v), %d builds; want the cached image", again, builtAgain, len(d.Builds()))
	}
}

func TestTheTagFollowsTheBinaryAndTheArchitecture(t *testing.T) {
	d := testkit.NewFakeDocker()
	tag := func(content, arch string) string {
		tag, _, err := egress.NewImages(d, finder(content), arch).Ensure(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tag
	}
	a, b, c := tag("v1", "arm64"), tag("v2", "arm64"), tag("v1", "amd64")
	if a == b || a == c || b == c {
		t.Errorf("tags %s %s %s must all differ", a, b, c)
	}
	if tag("v1", "arm64") != a {
		t.Error("the same binary must give the same tag")
	}
}

func TestAMissingEmbeddedBinaryExplainsHowToGetIt(t *testing.T) {
	_, err := egress.EmbeddedBinary("riscv64")
	if err == nil || !strings.Contains(err.Error(), "task build") || !strings.Contains(err.Error(), "linux/riscv64") {
		t.Errorf("error = %v", err)
	}
	d := testkit.NewFakeDocker()
	_, _, err = egress.NewImages(d, func(string) ([]byte, error) { return nil, errors.New("no proxy binary") }, "arm64").Ensure(ctx)
	if err == nil || !strings.Contains(err.Error(), "no proxy binary") {
		t.Errorf("Ensure = %v", err)
	}
}

func TestBuildFailuresAndDockerDown(t *testing.T) {
	d := testkit.NewFakeDocker()
	d.OnBuild(func(docker.BuildSpec) error { return errors.New("disk full") })
	if _, _, err := egress.NewImages(d, finder("x"), "arm64").Ensure(ctx); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("build failure = %v", err)
	}
	d2 := testkit.NewFakeDocker()
	d2.Down(errors.New("stopped"))
	if _, _, err := egress.NewImages(d2, finder("x"), "arm64").Ensure(ctx); !errors.Is(err, docker.ErrUnavailable) {
		t.Errorf("docker down = %v", err)
	}
}

func TestConcurrentEnsuresBuildOnce(t *testing.T) {
	d := testkit.NewFakeDocker()
	images := egress.NewImages(d, finder("x"), "arm64")
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); images.Ensure(ctx) }()
	}
	wg.Wait()
	if n := len(d.Builds()); n != 1 {
		t.Errorf("%d builds, want 1", n)
	}
}
