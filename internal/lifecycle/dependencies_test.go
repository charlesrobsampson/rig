package lifecycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charlesrobsampson/rig/internal/api"
	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/domain"
	"github.com/charlesrobsampson/rig/internal/testkit"
)

const dbDeps = "dependencies:\n  - name: db\n    image: postgres:16\n    env: {POSTGRES_PASSWORD: dev-only}\n"

func addDeps(t *testing.T, path, doc string) {
	t.Helper()
	raw, _ := os.ReadFile(filepath.Join(path, "ticket.yaml"))
	os.WriteFile(filepath.Join(path, "ticket.yaml"), append(raw, []byte(doc)...), 0o600)
}

func TestDependenciesStartBeforeServicesAndAreReachableByName(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	addDeps(t, path, dbDeps+"env: {DEBUG: \"true\"}\n")

	res := f.mustStart("demo", domain.SandboxContainer)
	if len(res.Dependencies) != 1 || !res.Dependencies[0].Running || res.Dependencies[0].Name != "rig-demo-dep-db" {
		t.Fatalf("dependencies = %+v", res.Dependencies)
	}
	db, err1 := dk.InspectContainer(ctx, "rig-demo-dep-db")
	api, err2 := dk.InspectContainer(ctx, "rig-demo-svc1")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if db.ID >= api.ID { // fake ids count up in creation order
		t.Errorf("the dependency (%s) must be created before the service (%s)", db.ID, api.ID)
	}
	if db.Labels["rig.role"] != "dependency" || db.Labels["rig.ticket"] != "demo" || db.Labels["rig.service"] != "db" {
		t.Errorf("labels = %v", db.Labels)
	}
	if ip, err := dk.Resolve("rig-demo-svc1", "db"); err != nil || ip == "" {
		t.Errorf("the service cannot resolve db: %q, %v", ip, err)
	}
	if st := f.state("demo").Dependencies["db"]; st.ContainerID != db.ID || st.Status != domain.ServiceRunning {
		t.Errorf("state = %+v", st)
	}
}

func TestADependencysEnvIsNotLeakedToOtherContainers(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	addDeps(t, path, dbDeps+"env: {DEBUG: \"true\"}\n")
	f.mustStart("demo", domain.SandboxContainer)

	svc, _ := dk.InspectContainer(ctx, "rig-demo-svc1")
	if _, leaked := svc.Env["POSTGRES_PASSWORD"]; leaked {
		t.Errorf("the dependency's env reached the service: %v", svc.Env)
	}
	db, _ := dk.InspectContainer(ctx, "rig-demo-dep-db")
	if _, leaked := db.Env["DEBUG"]; leaked || db.Env["POSTGRES_PASSWORD"] != "dev-only" {
		t.Errorf("the dependency's env = %v; it gets its own and not the ticket's", db.Env)
	}
}

func TestADependencyThatCannotStartAbortsTheStartAndCleansUp(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(dk *testkit.FakeDocker)
		expect string
	}{
		{"the image is missing", func(*testkit.FakeDocker) {}, "postgres:16"},
		{"it exits at once", func(dk *testkit.FakeDocker) {
			go func() {
				for i := 0; i < 2000; i++ {
					if _, err := dk.InspectContainer(ctx, "rig-demo-dep-db"); err == nil {
						dk.Print("rig-demo-dep-db", "FATAL: data directory has wrong ownership")
						dk.Exit("rig-demo-dep-db", 1)
						return
					}
					time.Sleep(time.Millisecond)
				}
			}()
		}, "data directory has wrong ownership"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, dk, _ := serviceFixture(t, nil)
			if tt.name != "the image is missing" {
				dk.AddImage("postgres:16")
			}
			dk.AddImage("redis:7")
			repo := testkit.NewRepo(t)
			path := f.create("demo", repo)
			withService(t, path, repo, true, apiService)
			addDeps(t, path, "dependencies:\n  - name: cache\n    image: redis:7\n  - name: db\n    image: postgres:16\n")
			tt.setup(dk)

			_, err := f.start("demo", domain.SandboxContainer)
			if err == nil || !strings.Contains(err.Error(), tt.expect) || !strings.Contains(err.Error(), "dependency db") {
				t.Fatalf("error = %v, want one naming dependency db and %q", err, tt.expect)
			}
			if apiCode(t, err) == api.CodeInvalid {
				t.Errorf("error code = %s", apiCode(t, err))
			}
			if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
				t.Errorf("containers left behind (the cache dependency started before db failed): %v", list)
			}
			st := f.state("demo")
			if st.Services["svc1"].ContainerID != "" {
				t.Error("the service must not start after a dependency failed")
			}
			if st.Dependencies["cache"].ContainerID != "" || st.Dependencies["cache"].Status != domain.ServiceStopped {
				t.Errorf("state still lists the removed dependency as up: %+v", st.Dependencies["cache"])
			}
			if !exists(filepath.Join(path, "svc1")) {
				t.Error("the checkout should stay so starting again retries")
			}
		})
	}
}

func TestAnUnhealthyDependencyIsAnError(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	addDeps(t, path, dbDeps)
	go func() {
		for i := 0; i < 2000; i++ {
			if _, err := dk.InspectContainer(ctx, "rig-demo-dep-db"); err == nil {
				dk.SetHealth("rig-demo-dep-db", docker.HealthUnhealthy)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// the image's own healthcheck makes the fake container report health
	dk.SetImageHealthcheck("postgres:16")
	_, err := f.start("demo", domain.SandboxContainer)
	if err == nil || !strings.Contains(err.Error(), "unhealthy") {
		t.Errorf("error = %v", err)
	}
}

func TestStartingAgainKeepsRunningDependencies(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	addDeps(t, path, dbDeps)
	first := f.mustStart("demo", domain.SandboxContainer)
	again := f.mustStart("demo", domain.SandboxContainer)
	if again.Dependencies[0].ID != first.Dependencies[0].ID || again.Dependencies[0].Note != "already running" {
		t.Errorf("second start = %+v", again.Dependencies)
	}
	dk.Exit(first.Dependencies[0].ID, 1)
	third := f.mustStart("demo", domain.SandboxContainer)
	if !third.Dependencies[0].Running || third.Dependencies[0].ID == first.Dependencies[0].ID {
		t.Errorf("a stopped dependency should be replaced: %+v", third.Dependencies)
	}
}

func TestStopRemovesDependenciesBeforeTheNetwork(t *testing.T) {
	f, dk, _ := serviceFixture(t, nil)
	dk.AddImage("postgres:16")
	repo := testkit.NewRepo(t)
	path := f.create("demo", repo)
	withService(t, path, repo, true, apiService)
	addDeps(t, path, dbDeps)
	f.mustStart("demo", domain.SandboxContainer)

	f.mustStop("demo", false, false)
	if list, _ := dk.ListContainers(ctx, nil); len(list) != 0 {
		t.Errorf("containers left: %v", list)
	}
	if nets, _ := dk.ListNetworks(ctx, nil); len(nets) != 0 {
		t.Errorf("networks left: %v", nets)
	}
	if st := f.state("demo").Dependencies["db"]; st.ContainerID != "" || st.Status != domain.ServiceStopped {
		t.Errorf("state = %+v", st)
	}
}
