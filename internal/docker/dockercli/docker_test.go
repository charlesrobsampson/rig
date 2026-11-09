//go:build docker

package dockercli_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"

	"github.com/charlesrobsampson/rig/internal/docker"
	"github.com/charlesrobsampson/rig/internal/docker/dockercli"
	"github.com/charlesrobsampson/rig/internal/docker/dockertest"
)

func TestTheClientMeetsTheContractAgainstRealDocker(t *testing.T) {
	image := os.Getenv("RIG_DOCKER_TEST_IMAGE")
	if image == "" {
		image = "alpine:3.22"
	}
	c := dockercli.New(dockercli.Options{})
	if err := c.Ping(context.Background()); err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	if ok, _ := c.ImageExists(context.Background(), image); !ok {
		t.Skipf("image %s is not present locally (set RIG_DOCKER_TEST_IMAGE or docker pull it)", image)
	}

	dockertest.Run(t, func(t *testing.T) dockertest.Subject {
		ctx := context.Background()
		return dockertest.Subject{
			Docker:        c,
			Image:         image,
			RunsProcesses: true,
			FreePort: func(t *testing.T) int {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer l.Close()
				return l.Addr().(*net.TCPAddr).Port
			},
			Exiting: func(t *testing.T, spec docker.ContainerSpec, code int) string {
				spec.Command = []string{"sh", "-c", fmt.Sprintf("echo bye; exit %d", code)}
				info, err := c.CreateContainer(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.StartContainer(ctx, info.ID); err != nil {
					t.Fatal(err)
				}
				return info.ID
			},
			Print: func(t *testing.T, id, line string) {
				_, err := c.Exec(ctx, id, docker.ExecSpec{Command: []string{"sh", "-c", `echo "$1" > /proc/1/fd/1`, "sh", line}})
				if err != nil {
					t.Fatal(err)
				}
			},
			MakeHealthy: func(*testing.T, string) {}, // the healthcheck is `true`, so it gets there by itself
			Down: func(t *testing.T) {
				t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
			},
		}
	})
}
