package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charlesrobsampson/rig/internal/api"
)

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, 0},
		{"plain error", errors.New("boom"), 1},
		{"invalid", &api.Error{Code: api.CodeInvalid}, 2},
		{"blocked", &api.Error{Code: api.CodeBlocked}, 3},
		{"unavailable", &api.Error{Code: api.CodeUnavailable}, 4},
		{"not running", &api.Error{Code: api.CodeNotRunning}, 4},
		{"not found", &api.Error{Code: api.CodeNotFound}, 5},
		{"conflict", &api.Error{Code: api.CodeConflict}, 1},
		{"internal", &api.Error{Code: api.CodeInternal}, 1},
		{"wrapped api error", fmt.Errorf("start ticket: %w", &api.Error{Code: api.CodeBlocked}), 3},
		{"usage", usageError(errors.New("bad flag")), 2},
		{"explicit code", exitError{code: 7}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestPrintError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"plain", errors.New("boom"), "rig: boom\n"},
		{"blocked with reasons", &api.Error{Code: api.CodeBlocked, Message: "cannot stop", Details: []string{"api is dirty", "web is dirty"}}, "rig: cannot stop\n  - api is dirty\n  - web is dirty\n"},
		{"silent exit code", exitError{code: 4}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printError(&buf, tt.err)
			if buf.String() != tt.want {
				t.Errorf("printed %q, want %q", buf.String(), tt.want)
			}
		})
	}
}

func TestUsageErrorsExitWithCode2AndStayOffStdout(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"bogus"}},
		{"unknown flag", []string{"--nope"}},
		{"unexpected argument", []string{"config", "show", "extra"}},
		{"missing subcommand argument", []string{"daemon", "status", "extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tt.args, &stdout, &stderr, deps{})
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
			}
			if stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "rig: ") {
				t.Errorf("stdout = %q, stderr = %q; errors belong on stderr", stdout.String(), stderr.String())
			}
		})
	}
}
