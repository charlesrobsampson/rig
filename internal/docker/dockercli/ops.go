package dockercli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charlesrobsampson/rig/internal/docker"
)

func labelArgs(flag string, labels map[string]string) []string {
	var args []string
	for _, k := range sortedKeys(labels) {
		args = append(args, flag, k+"="+labels[k])
	}
	return args
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func filterArgs(labels map[string]string) []string {
	var args []string
	for _, k := range sortedKeys(labels) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	return args
}

// Networks

func (c *Client) CreateNetwork(ctx context.Context, spec docker.NetworkSpec) (docker.NetworkInfo, error) {
	if spec.Name == "" {
		return docker.NetworkInfo{}, fmt.Errorf("%w: a network needs a name", docker.ErrInvalid)
	}
	args := []string{"network", "create"}
	if spec.Internal {
		args = append(args, "--internal")
	}
	args = append(args, labelArgs("--label", spec.Labels)...)
	if _, err := c.run(ctx, append(args, spec.Name)...); err != nil {
		return docker.NetworkInfo{}, err
	}
	return c.InspectNetwork(ctx, spec.Name)
}

type networkJSON struct {
	ID         string                     `json:"Id"`
	Name       string                     `json:"Name"`
	Internal   bool                       `json:"Internal"`
	Labels     map[string]string          `json:"Labels"`
	Containers map[string]json.RawMessage `json:"Containers"`
}

func parseNetworks(out string) ([]docker.NetworkInfo, error) {
	var raw []networkJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse docker network inspect: %w", err)
	}
	infos := make([]docker.NetworkInfo, 0, len(raw))
	for _, n := range raw {
		info := docker.NetworkInfo{ID: n.ID, Name: n.Name, Internal: n.Internal, Labels: n.Labels}
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		for id := range n.Containers {
			info.Containers = append(info.Containers, id)
		}
		sort.Strings(info.Containers)
		infos = append(infos, info)
	}
	return infos, nil
}

func (c *Client) InspectNetwork(ctx context.Context, nameOrID string) (docker.NetworkInfo, error) {
	res, err := c.run(ctx, "network", "inspect", nameOrID)
	if err != nil {
		return docker.NetworkInfo{}, err
	}
	infos, err := parseNetworks(res.Stdout)
	if err != nil || len(infos) == 0 {
		return docker.NetworkInfo{}, fmt.Errorf("%w: network %s", docker.ErrNotFound, nameOrID)
	}
	return infos[0], nil
}

func (c *Client) ListNetworks(ctx context.Context, labels map[string]string) ([]docker.NetworkInfo, error) {
	res, err := c.run(ctx, append([]string{"network", "ls", "--no-trunc", "--format", "{{.ID}}"}, filterArgs(labels)...)...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(res.Stdout)
	if len(ids) == 0 {
		return nil, nil
	}
	res, err = c.run(ctx, append([]string{"network", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	return parseNetworks(res.Stdout)
}

func (c *Client) RemoveNetwork(ctx context.Context, nameOrID string) error {
	_, err := c.run(ctx, "network", "rm", nameOrID)
	return err
}

// Images

// buildTail is how much of a failed build's output the error carries.
const buildTail = 20

func (c *Client) BuildImage(ctx context.Context, spec docker.BuildSpec) error {
	if spec.Tag == "" || !filepath.IsAbs(spec.ContextDir) {
		return fmt.Errorf("%w: a build needs a tag and an absolute context directory", docker.ErrInvalid)
	}
	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	path := filepath.Join(spec.ContextDir, dockerfile)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return fmt.Errorf("%w: no Dockerfile at %s", docker.ErrInvalid, path)
	}
	args := []string{"build", "--tag", spec.Tag, "--file", path}
	args = append(args, labelArgs("--label", spec.Labels)...)
	for _, k := range sortedKeys(spec.Args) {
		args = append(args, "--build-arg", k+"="+spec.Args[k])
	}
	res, err := c.run(ctx, append(args, spec.ContextDir)...)
	if err != nil {
		lines := strings.Split(strings.TrimSpace(res.Output), "\n")
		if len(lines) > buildTail {
			lines = lines[len(lines)-buildTail:]
		}
		return fmt.Errorf("build %s: %w\n%s", spec.Tag, err, strings.Join(lines, "\n"))
	}
	return nil
}

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	res, err := c.runCallAllowFailure(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	res, err := c.run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(res.Stdout)
	if id == "" {
		return "", fmt.Errorf("%w: image %s", docker.ErrNotFound, ref)
	}
	return id, nil
}

// runCallAllowFailure returns the result of a failed call too, unless docker itself is
// unreachable (which is an error whatever the question was).
func (c *Client) runCallAllowFailure(ctx context.Context, args ...string) (Result, error) {
	res, err := c.runCall(ctx, Call{Args: args})
	if err != nil && mapped(err, docker.ErrUnavailable) {
		return res, err
	}
	return res, nil
}

func mapped(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

// Containers

// createArgs builds `docker create`, and the environment that carries the values of
// -e NAME flags. It is separate from CreateContainer so the flags can be tested.
func createArgs(spec docker.ContainerSpec) (args []string, env map[string]string, err error) {
	args = []string{"create", "--name", spec.Name}
	args = append(args, labelArgs("--label", spec.Labels)...)
	env = map[string]string{}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "--env", k)
		env[k] = spec.Env[k]
	}
	for _, m := range spec.Mounts {
		if !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Target) {
			return nil, nil, fmt.Errorf("%w: mount %s:%s needs absolute paths", docker.ErrInvalid, m.Source, m.Target)
		}
		if strings.ContainsAny(m.Source+m.Target, ",\n") {
			return nil, nil, fmt.Errorf("%w: mount paths cannot contain commas or newlines", docker.ErrInvalid)
		}
		if _, err := os.Stat(m.Source); err != nil {
			return nil, nil, fmt.Errorf("%w: mount source %s does not exist", docker.ErrInvalid, m.Source)
		}
		mount := "type=bind,source=" + m.Source + ",target=" + m.Target
		if m.ReadOnly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
	}
	for _, p := range spec.Ports {
		host := ""
		if p.Host != 0 {
			host = strconv.Itoa(p.Host)
		}
		args = append(args, "--publish", "127.0.0.1:"+host+":"+strconv.Itoa(p.Container))
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
		for _, a := range spec.Aliases {
			args = append(args, "--network-alias", a)
		}
	}
	if spec.WorkDir != "" {
		args = append(args, "--workdir", spec.WorkDir)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if h := spec.Healthcheck; h != nil {
		args = append(args, "--health-cmd", shellJoin(h.Command))
		if h.Interval > 0 {
			args = append(args, "--health-interval", h.Interval.String())
		}
		if h.Timeout > 0 {
			args = append(args, "--health-timeout", h.Timeout.String())
		}
		if h.Retries > 0 {
			args = append(args, "--health-retries", strconv.Itoa(h.Retries))
		}
	}
	if spec.TTY {
		args = append(args, "--tty")
	}
	if spec.Stdin {
		args = append(args, "--interactive")
	}
	args = append(args, spec.Image)
	return append(args, spec.Command...), env, nil
}

func shellJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func (c *Client) CreateContainer(ctx context.Context, spec docker.ContainerSpec) (docker.ContainerInfo, error) {
	if spec.Name == "" || spec.Image == "" {
		return docker.ContainerInfo{}, fmt.Errorf("%w: a container needs a name and an image", docker.ErrInvalid)
	}
	args, env, err := createArgs(spec)
	if err != nil {
		return docker.ContainerInfo{}, err
	}
	// `docker create` accepts a network that does not exist and only fails at start.
	if spec.Network != "" {
		if _, err := c.InspectNetwork(ctx, spec.Network); err != nil {
			return docker.ContainerInfo{}, err
		}
	}
	if _, err := c.runCall(ctx, Call{Args: args, Env: env}); err != nil {
		return docker.ContainerInfo{}, err
	}
	return c.InspectContainer(ctx, spec.Name)
}

func (c *Client) StartContainer(ctx context.Context, nameOrID string) error {
	_, err := c.run(ctx, "start", nameOrID)
	return err
}

func (c *Client) StopContainer(ctx context.Context, nameOrID string, grace time.Duration) error {
	secs := int(grace.Round(time.Second) / time.Second)
	_, err := c.run(ctx, "stop", "--time", strconv.Itoa(secs), nameOrID)
	return err
}

func (c *Client) RemoveContainer(ctx context.Context, nameOrID string, force bool) error {
	args := []string{"rm", "--volumes"}
	if force {
		// `rm --force` succeeds for a container that does not exist.
		if _, err := c.InspectContainer(ctx, nameOrID); err != nil {
			return err
		}
		args = append(args, "--force")
	}
	_, err := c.run(ctx, append(args, nameOrID)...)
	return err
}

type containerJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status   string `json:"Status"`
		ExitCode int    `json:"ExitCode"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
	HostConfig struct {
		PortBindings map[string][]portBindingJSON `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Ports    map[string][]portBindingJSON `json:"Ports"`
		Networks map[string]struct {
			Aliases []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

type portBindingJSON struct {
	HostPort string `json:"HostPort"`
}

func parseContainers(out string) ([]docker.ContainerInfo, error) {
	var raw []containerJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}
	infos := make([]docker.ContainerInfo, 0, len(raw))
	for _, r := range raw {
		info := docker.ContainerInfo{
			ID: r.ID, Name: strings.TrimPrefix(r.Name, "/"), Image: r.Config.Image,
			ExitCode: r.State.ExitCode, Labels: r.Config.Labels, Health: docker.HealthNone,
			Env: map[string]string{}, Networks: map[string]docker.ContainerNetwork{},
		}
		if info.Labels == nil {
			info.Labels = map[string]string{}
		}
		switch r.State.Status {
		case "running", "restarting", "paused":
			info.State = docker.StateRunning
		case "created":
			info.State = docker.StateCreated
		default:
			info.State = docker.StateExited
		}
		if info.State != docker.StateExited {
			info.ExitCode = 0
		}
		if r.State.Health != nil {
			switch r.State.Health.Status {
			case "starting":
				info.Health = docker.HealthStarting
			case "healthy":
				info.Health = docker.HealthHealthy
			case "unhealthy":
				info.Health = docker.HealthUnhealthy
			}
		}
		for _, kv := range r.Config.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				info.Env[k] = v
			}
		}
		for _, m := range r.Mounts {
			if m.Type == "bind" {
				info.Mounts = append(info.Mounts, docker.Mount{Source: m.Source, Target: m.Destination, ReadOnly: !m.RW})
			}
		}
		bindings := r.NetworkSettings.Ports
		if info.State != docker.StateRunning {
			bindings = r.HostConfig.PortBindings
		}
		for port, hosts := range bindings {
			containerPort, _, _ := strings.Cut(port, "/")
			cp, _ := strconv.Atoi(containerPort)
			for _, h := range hosts {
				hp, _ := strconv.Atoi(h.HostPort)
				info.Ports = append(info.Ports, docker.PortBinding{Container: cp, Host: hp})
			}
		}
		sort.Slice(info.Ports, func(i, j int) bool {
			if info.Ports[i].Container != info.Ports[j].Container {
				return info.Ports[i].Container < info.Ports[j].Container
			}
			return info.Ports[i].Host < info.Ports[j].Host
		})
		for name, n := range r.NetworkSettings.Networks {
			aliases := append([]string(nil), n.Aliases...)
			// Docker resolves a container by its own name without listing it.
			if !containsString(aliases, info.Name) {
				aliases = append([]string{info.Name}, aliases...)
			}
			info.Networks[name] = docker.ContainerNetwork{Aliases: aliases}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (c *Client) InspectContainer(ctx context.Context, nameOrID string) (docker.ContainerInfo, error) {
	res, err := c.run(ctx, "inspect", "--type", "container", nameOrID)
	if err != nil {
		return docker.ContainerInfo{}, err
	}
	infos, err := parseContainers(res.Stdout)
	if err != nil || len(infos) == 0 {
		return docker.ContainerInfo{}, fmt.Errorf("%w: container %s", docker.ErrNotFound, nameOrID)
	}
	return infos[0], nil
}

func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]docker.ContainerInfo, error) {
	res, err := c.run(ctx, append([]string{"ps", "--all", "--no-trunc", "--format", "{{.ID}}"}, filterArgs(labels)...)...)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(res.Stdout)
	if len(ids) == 0 {
		return nil, nil
	}
	res, err = c.run(ctx, append([]string{"inspect", "--type", "container"}, ids...)...)
	if err != nil {
		return nil, err
	}
	infos, err := parseContainers(res.Stdout)
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return infos, err
}

func (c *Client) Exec(ctx context.Context, nameOrID string, spec docker.ExecSpec) (docker.ExecResult, error) {
	if len(spec.Command) == 0 {
		return docker.ExecResult{}, fmt.Errorf("%w: an exec needs a command", docker.ErrInvalid)
	}
	args := []string{"exec"}
	env := map[string]string{}
	for _, k := range sortedKeys(spec.Env) {
		args = append(args, "--env", k)
		env[k] = spec.Env[k]
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.WorkDir != "" {
		args = append(args, "--workdir", spec.WorkDir)
	}
	res, err := c.opts.Runner(ctx, Call{Args: append(append(args, nameOrID), spec.Command...), Env: env})
	if err != nil {
		return docker.ExecResult{}, err
	}
	// docker prints its own failures in a recognizable form; anything else is the
	// command's own output and exit code.
	if res.ExitCode != 0 && isDockerFailure(res.Stderr) {
		return docker.ExecResult{}, mapError(res.Stderr)
	}
	return docker.ExecResult{ExitCode: res.ExitCode, Output: res.Output}, nil
}

func isDockerFailure(stderr string) bool {
	s := strings.TrimSpace(stderr)
	return strings.HasPrefix(s, "Error response from daemon:") || strings.HasPrefix(s, "Error: No such") ||
		strings.HasPrefix(s, "Cannot connect to the Docker daemon") || strings.Contains(s, "Is the docker daemon running")
}

func (c *Client) Logs(ctx context.Context, nameOrID string, opts docker.LogOptions) (io.ReadCloser, error) {
	if _, err := c.InspectContainer(ctx, nameOrID); err != nil { // so a missing container is an error, not an empty stream
		return nil, err
	}
	args := []string{"logs"}
	if opts.Tail > 0 {
		args = append(args, "--tail", strconv.Itoa(opts.Tail))
	}
	if opts.Follow {
		return c.opts.Streamer(ctx, append(append(args, "--follow"), nameOrID))
	}
	res, err := c.run(ctx, append(args, nameOrID)...)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(res.Output)), nil
}

type processReader struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (p *processReader) Close() error {
	p.ReadCloser.Close()
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	p.cmd.Wait()
	return nil
}

func (c *Client) stream(ctx context.Context, args []string) (io.ReadCloser, error) {
	binary, err := exec.LookPath(c.opts.Binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", docker.ErrUnavailable, errNoBinary)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("run docker: %w", err)
	}
	pw.Close() // the child holds the write end; the reader sees EOF when it exits
	return &processReader{ReadCloser: pr, cmd: cmd}, nil
}
