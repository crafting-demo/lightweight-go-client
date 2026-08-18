package crafting

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestExecRunnerReportsExitCodeAsResult(t *testing.T) {
	res, err := NewExecRunner("sh").Run(context.Background(), nil, "-c", "echo out; echo err >&2; exit 7")
	if err != nil {
		t.Fatalf("a non-zero exit should be a result, not an error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q, want out\\n", res.Stdout)
	}
	if res.Stderr != "err\n" {
		t.Errorf("stderr = %q, want err\\n", res.Stderr)
	}
}

func TestExecRunnerMergesSuppliedEnv(t *testing.T) {
	res, err := NewExecRunner("sh").Run(context.Background(), []string{"CRAFTING_TEST_ENV=from-client"}, "-c", `printf '%s' "$CRAFTING_TEST_ENV"`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "from-client" {
		t.Errorf("stdout = %q, want from-client", res.Stdout)
	}
}

func TestExecRunnerWrapsAMissingBinary(t *testing.T) {
	_, err := NewExecRunner("crafting-cs-does-not-exist").Run(context.Background(), nil)
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error = %v, want it to wrap exec.ErrNotFound", err)
	}
}

func TestExecRunnerSurfacesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewExecRunner("sh").Run(ctx, nil, "-c", "sleep 10")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestNewExecRunnerDefaultsTheBinary(t *testing.T) {
	r, ok := NewExecRunner("").(*execRunner)
	if !ok {
		t.Fatal("NewExecRunner should return an execRunner")
	}
	if r.binary != DefaultBinary {
		t.Errorf("binary = %q, want %q", r.binary, DefaultBinary)
	}
}
