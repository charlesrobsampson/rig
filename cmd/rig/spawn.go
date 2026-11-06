package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/charlesrobsampson/rig/internal/client"
)

// spawnDetached starts `rig daemon run` as its own session, detached from this
// terminal, so the daemon outlives the command that started it.
func spawnDetached(configPath string) client.Spawner {
	return func(context.Context) error {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("find the rig binary: %w", err)
		}
		args := []string{"daemon", "run"}
		if configPath != "" {
			args = append(args, "--config", configPath)
		}
		cmd := exec.Command(exe, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
}
