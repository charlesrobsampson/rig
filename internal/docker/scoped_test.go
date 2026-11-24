package docker_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

var ctx = context.Background()

func names(list []docker.ContainerInfo) string {
	var out []string
	for _, c := range list {
		out = append(out, c.Name)
	}
	return strings.Join(out, ",")
}

// twoHomes is one Docker shared by two rig homes, each with a ticket's network and a
// container on it, plus a container made by a rig from before homes were labelled.
func twoHomes(t *testing.T) (raw *testkit.FakeDocker, a, b docker.Docker) {
	t.Helper()
	raw = testkit.NewFakeDocker()
	raw.AddImage("img")
	a, b = docker.Scoped(raw, "/home/a/.rig"), docker.Scoped(raw, "/home/b/.rig")
	for _, h := range []struct {
		d      docker.Docker
		ticket string
	}{{a, "ta"}, {b, "tb"}} {
		if _, err := h.d.CreateNetwork(ctx, docker.NetworkSpec{Name: "rig-" + h.ticket, Labels: docker.Labels(h.ticket, "", docker.RoleNetwork)}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.d.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-" + h.ticket + "-api", Image: "img", Network: "rig-" + h.ticket, Labels: docker.Labels(h.ticket, "api", docker.RoleService)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.CreateContainer(ctx, docker.ContainerSpec{Name: "rig-old-api", Image: "img", Labels: docker.Labels("old", "api", docker.RoleService)}); err != nil {
		t.Fatal(err)
	}
	return raw, a, b
}

func TestEachHomeListsOnlyItsOwnObjects(t *testing.T) {
	raw, a, b := twoHomes(t)
	for _, tt := range []struct {
		d    docker.Docker
		want string
	}{{a, "rig-ta-api"}, {b, "rig-tb-api"}} {
		got, err := tt.d.ListContainers(ctx, map[string]string{docker.LabelTicket: ""})
		if err != nil || names(got) != tt.want {
			t.Errorf("containers = %q, %v; want %q", names(got), err, tt.want)
		}
		byRole, _ := tt.d.ListContainers(ctx, map[string]string{docker.LabelRole: docker.RoleService})
		if names(byRole) != tt.want {
			t.Errorf("by role = %q, want %q", names(byRole), tt.want)
		}
		nets, _ := tt.d.ListNetworks(ctx, map[string]string{docker.LabelTicket: ""})
		if len(nets) != 1 || nets[0].Name != "rig-"+strings.TrimPrefix(tt.want, "rig-")[:2] {
			t.Errorf("networks = %+v", nets)
		}
	}
	all, _ := raw.ListContainers(ctx, map[string]string{docker.LabelTicket: ""})
	if len(all) != 3 {
		t.Fatalf("the shared Docker has %q", names(all))
	}
	homes := map[string]int{}
	for _, c := range all {
		homes[c.Labels[docker.LabelHome]]++
	}
	if homes["/home/a/.rig"] != 1 || homes["/home/b/.rig"] != 1 || homes[""] != 1 {
		t.Errorf("home labels on the shared Docker: %v", homes)
	}
}

func TestAnUnlabelledObjectIsFoundByItsTicketButNeverByAScan(t *testing.T) {
	_, a, _ := twoHomes(t)
	byTicket, err := a.ListContainers(ctx, map[string]string{docker.LabelTicket: "old"})
	if err != nil || names(byTicket) != "rig-old-api" {
		t.Errorf("by ticket = %q, %v", names(byTicket), err)
	}
	if got, _ := a.ListContainers(ctx, map[string]string{docker.LabelTicket: ""}); strings.Contains(names(got), "old") {
		t.Errorf("a scan returned the unlabelled container: %q", names(got))
	}
	if _, err := a.InspectContainer(ctx, "rig-old-api"); err != nil {
		t.Errorf("inspecting an unlabelled container: %v", err)
	}
}

func TestAnotherHomesObjectsCannotBeTouched(t *testing.T) {
	raw, a, _ := twoHomes(t)
	raw.StartContainer(ctx, "rig-tb-api")
	for name, call := range map[string]func() error{
		"inspect": func() error { _, err := a.InspectContainer(ctx, "rig-tb-api"); return err },
		"stop":    func() error { return a.StopContainer(ctx, "rig-tb-api", 0) },
		"start":   func() error { return a.StartContainer(ctx, "rig-tb-api") },
		"remove":  func() error { return a.RemoveContainer(ctx, "rig-tb-api", true) },
		"exec": func() error {
			_, err := a.Exec(ctx, "rig-tb-api", docker.ExecSpec{Command: []string{"true"}})
			return err
		},
		"inspectnet": func() error { _, err := a.InspectNetwork(ctx, "rig-tb"); return err },
		"removenet":  func() error { return a.RemoveNetwork(ctx, "rig-tb") },
		"connect":    func() error { return a.ConnectNetwork(ctx, "rig-ta-api", "rig-tb", nil) },
		"connectctr": func() error { return a.ConnectNetwork(ctx, "rig-tb-api", "rig-ta", nil) },
	} {
		err := call()
		if !errors.Is(err, docker.ErrOtherHome) || !strings.Contains(err.Error(), "/home/b/.rig") {
			t.Errorf("%s: error = %v, want ErrOtherHome naming the other home", name, err)
		}
	}
	if c, err := raw.InspectContainer(ctx, "rig-tb-api"); err != nil || c.State != docker.StateRunning {
		t.Errorf("home b's container after home a's attempts: %+v, %v", c, err)
	}
	if _, err := raw.InspectNetwork(ctx, "rig-tb"); err != nil {
		t.Errorf("home b's network is gone: %v", err)
	}
}

func TestTheCallersLabelsAreNotChangedAndImagesAreLabelledToo(t *testing.T) {
	raw := testkit.NewFakeDocker()
	d := docker.Scoped(raw, "/h")
	labels := docker.Labels("t", "api", docker.RoleAgent)
	raw.AddImage("img")
	d.CreateContainer(ctx, docker.ContainerSpec{Name: "c", Image: "img", Labels: labels})
	d.CreateNetwork(ctx, docker.NetworkSpec{Name: "n", Labels: labels})
	if _, ok := labels[docker.LabelHome]; ok {
		t.Error("the caller's label map was changed")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM img\n"), 0o600)
	if err := d.BuildImage(ctx, docker.BuildSpec{Tag: "x", ContextDir: dir}); err != nil {
		t.Fatal(err)
	}
	if b := raw.Builds(); len(b) != 1 || b[0].Labels[docker.LabelHome] != "/h" {
		t.Errorf("builds = %+v", b)
	}
}
