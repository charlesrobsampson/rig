// Command linuxtest runs rig's default test suite on Linux, from a Mac or any other
// machine with Docker: it cross-compiles every package's tests for Linux and runs them in
// a small Debian container (git and a shell, nothing else), as an ordinary user. It needs
// no Go toolchain inside the container. Run it with `task test:linux`; pass package
// patterns after `--` to test only some.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const image = "rig-linux-test:1"

const dockerfile = `FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates procps && rm -rf /var/lib/apt/lists/*
RUN useradd -m -u 1000 rig
USER rig
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "linuxtest:", err)
		os.Exit(1)
	}
}

type result struct {
	pkg    string
	ok     bool
	output string
	took   time.Duration
}

func run(patterns []string) error {
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	arch, err := dockerArch()
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := ensureImage(); err != nil {
		return err
	}
	pkgs, err := testPackages(patterns)
	if err != nil {
		return err
	}
	if len(pkgs) == 0 {
		return fmt.Errorf("no packages with tests match %v", patterns)
	}
	out, err := os.MkdirTemp("", "rig-linuxtest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(out)
	os.Chmod(out, 0o755)

	fmt.Printf("running %d package(s) on linux/%s in %s\n", len(pkgs), arch, image)
	results := make([]result, len(pkgs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(runtime.NumCPU()/2, 2))
	for i, p := range pkgs {
		wg.Add(1)
		go func(i int, p pkg) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runPackage(root, out, arch, i, p)
		}(i, p)
	}
	wg.Wait()

	failed := 0
	for _, r := range results {
		status := "ok  "
		if !r.ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s\t%s\t%.1fs\n", status, r.pkg, r.took.Seconds())
		if !r.ok {
			fmt.Println(indent(r.output))
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d package(s) failed on Linux", failed, len(pkgs))
	}
	fmt.Printf("all %d package(s) pass on linux/%s\n", len(pkgs), arch)
	return nil
}

type pkg struct{ importPath, dir string }

// testPackages lists the packages that have tests, sorted.
func testPackages(patterns []string) ([]pkg, error) {
	args := append([]string{"list", "-f", `{{.ImportPath}}|{{.Dir}}|{{len .TestGoFiles}}|{{len .XTestGoFiles}}`}, patterns...)
	raw, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var out []pkg
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Split(line, "|")
		if len(f) == 4 && (f[2] != "0" || f[3] != "0") {
			out = append(out, pkg{f[0], f[1]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].importPath < out[j].importPath })
	return out, nil
}

func runPackage(root, out, arch string, i int, p pkg) result {
	start := time.Now()
	rel, err := filepath.Rel(root, p.dir)
	if err != nil {
		return result{p.importPath, false, err.Error(), time.Since(start)}
	}
	name := fmt.Sprintf("t%03d.test", i)
	build := exec.Command("go", "test", "-c", "-o", filepath.Join(out, name), p.importPath)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		return result{p.importPath, false, "cannot build the tests for Linux:\n" + string(b), time.Since(start)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	// the repository is read-only and the user is not root, so a test that writes where it
	// should not, or relies on root's permissions, fails here
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"-v", root+":/src:ro", "-v", out+":/out:ro", "-w", "/src/"+filepath.ToSlash(rel),
		"--tmpfs", "/tmp:exec,mode=1777", "-e", "HOME=/tmp/home", "-e", "TMPDIR=/tmp",
		image, "/out/"+name, "-test.count=1", "-test.timeout=240s")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err = cmd.Run()
	return result{p.importPath, err == nil, buf.String(), time.Since(start)}
}

func dockerArch() (string, error) {
	raw, err := exec.Command("docker", "info", "--format", "{{.Architecture}}").Output()
	if err != nil {
		return "", fmt.Errorf("docker is not available: %w", err)
	}
	switch strings.TrimSpace(string(raw)) {
	case "aarch64", "arm64":
		return "arm64", nil
	case "x86_64", "amd64":
		return "amd64", nil
	}
	return "", fmt.Errorf("unsupported Docker architecture %q", strings.TrimSpace(string(raw)))
}

func ensureImage() error {
	if exec.Command("docker", "image", "inspect", image).Run() == nil {
		return nil
	}
	fmt.Println("building", image, "(git on Debian; once)")
	cmd := exec.Command("docker", "build", "-t", image, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 60 {
		lines = append(lines[:30], append([]string{"    ..."}, lines[len(lines)-30:]...)...)
	}
	for i, l := range lines {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}
