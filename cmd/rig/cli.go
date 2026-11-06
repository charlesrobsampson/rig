package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/charlesrobsampson/rig/internal/api"
)

// exitError carries an explicit exit code; a nil err means the failure was already
// reported and nothing more should be printed.
type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

type usageErr struct{ err error }

func (e usageErr) Error() string { return e.err.Error() }
func (e usageErr) Unwrap() error { return e.err }

func usageError(err error) error { return usageErr{err} }

// usageArgs makes argument-count mistakes exit with the usage code (2).
func usageArgs(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usageError(err)
		}
		return nil
	}
}

// exitCode maps an error to the exit codes in docs/formats.md §10.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var explicit exitError
	if errors.As(err, &explicit) {
		return explicit.code
	}
	var usage usageErr
	if errors.As(err, &usage) {
		return 2
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case api.CodeInvalid:
			return 2
		case api.CodeBlocked:
			return 3
		case api.CodeUnavailable, api.CodeNotRunning:
			return 4
		case api.CodeNotFound:
			return 5
		}
	}
	return 1
}

// printError writes an error to w (stderr), listing the reasons of a blocked request.
func printError(w io.Writer, err error) {
	var explicit exitError
	if errors.As(err, &explicit) && explicit.err == nil {
		return
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		fmt.Fprintf(w, "rig: %v\n", err)
		return
	}
	if err == error(apiErr) {
		fmt.Fprintf(w, "rig: %s\n", apiErr.Message) // the code prefix is for logs, not people
	} else {
		fmt.Fprintf(w, "rig: %v\n", err)
	}
	for _, d := range apiErr.Details {
		fmt.Fprintf(w, "  - %s\n", d)
	}
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
