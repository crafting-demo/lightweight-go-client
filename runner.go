package crafting

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Result is the outcome of a single cs invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Runner executes the cs CLI. It exists so callers can be unit tested without a
// Crafting organization, and so the transport can be replaced without changing
// the client surface.
type Runner interface {
	Run(ctx context.Context, env []string, args ...string) (*Result, error)
}

// NewExecRunner returns a Runner that shells out to the named cs binary. An
// empty binary name selects DefaultBinary.
func NewExecRunner(binary string) Runner {
	if binary == "" {
		binary = DefaultBinary
	}
	return &execRunner{binary: binary}
}

type execRunner struct {
	binary string
}

// Run invokes cs and captures stdout and stderr separately. A non-zero exit
// status is reported in Result.ExitCode rather than as an error; an error is
// returned only when the process could not be run or was interrupted, which
// callers treat as retryable.
func (r *execRunner) Run(ctx context.Context, env []string, args ...string) (*Result, error) {
	cmd := exec.CommandContext(ctx, r.binary, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}

	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	// Context cancellation, binary missing, fork failure: not a CLI verdict.
	return res, fmt.Errorf("running %s %s: %w", r.binary, strings.Join(args, " "), err)
}
