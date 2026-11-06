package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// acquireInstance takes an exclusive lock on the pid file and writes our pid into it.
// The lock, not the pid, decides whether another daemon is alive: a crashed daemon
// leaves a pid file nobody holds, which is simply taken over.
func acquireInstance(pidPath string) (*os.File, error) {
	f, err := os.OpenFile(pidPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pid file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("daemon already running (pid %s, pid file %s)", readPID(f), pidPath)
		}
		return nil, fmt.Errorf("lock pid file: %w", err)
	}
	if err := f.Truncate(0); err == nil {
		_, err = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("write pid file: %w", err)
	}
	return f, nil
}

func releaseInstance(f *os.File, pidPath string) {
	os.Remove(pidPath)
	f.Close()
}

func readPID(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if pid := strings.TrimSpace(string(buf[:n])); pid != "" {
		return pid
	}
	return "unknown"
}

// clearStaleSocket removes a socket file left by a crashed daemon. It must only be
// called while holding the instance lock, and never deletes anything but a socket.
func clearStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
	}
	return os.Remove(path)
}
