package crafting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sandbox lifecycle stages reported by the control plane.
const (
	StageRunning   = "RUNNING"
	StageSuspended = "SUSPENDED"
)

// CreateSandboxOptions describes the sandbox to provision.
type CreateSandboxOptions struct {
	// Name is the sandbox name. Crafting names are short and must start with a
	// letter; DeriveSandboxName turns an arbitrary identifier into one.
	Name string
	// Template is the Crafting Template to create from.
	Template string
	// Env are KEY=VALUE pairs injected into every workload.
	Env []string
	// From restores a composite snapshot, so the new sandbox comes up with the
	// same home directory and the same dependency data the origin had. Each
	// call yields an independent sandbox, which is what makes forking possible.
	From *CompositeSnapshot
	// UsePool selects pooled capacity. Empty uses DefaultUsePool.
	UsePool string
	// Region pins the sandbox to a region. Empty lets Crafting choose.
	Region string
	// WaitTimeout bounds provisioning. Zero uses DefaultCreateTimeout.
	WaitTimeout time.Duration
}

// CreateSandbox provisions a sandbox and returns once it is ready.
//
// Creation is idempotent for a given name: an existing sandbox is left alone
// rather than duplicated, so a retried call after a partial failure converges on
// one sandbox.
func (c *Client) CreateSandbox(ctx context.Context, opts CreateSandboxOptions) (SandboxRef, error) {
	ref := c.Ref(opts.Name)
	if opts.Name == "" {
		return ref, fmt.Errorf("crafting: sandbox name is required")
	}
	if opts.Template == "" {
		return ref, fmt.Errorf("crafting: template is required")
	}
	for _, e := range opts.Env {
		if i := strings.IndexByte(e, '='); i <= 0 {
			return ref, fmt.Errorf("crafting: env entry %q is not KEY=VALUE", e)
		}
	}
	if err := c.ensureAuth(ctx); err != nil {
		return ref, err
	}

	waitTimeout := opts.WaitTimeout
	if waitTimeout == 0 {
		waitTimeout = DefaultCreateTimeout
	}
	usePool := opts.UsePool
	if usePool == "" {
		usePool = DefaultUsePool
	}

	args := []string{"sandbox", "create", ref.Name, "-t", opts.Template, "--if-exists", "skip"}
	for _, e := range opts.Env {
		args = append(args, "-E", e)
	}
	if opts.From != nil {
		for _, rule := range opts.From.Overrides() {
			args = append(args, "-D", rule)
		}
	}
	args = append(args, "--use-pool", usePool)
	if opts.Region != "" {
		args = append(args, "--region", opts.Region)
	}
	// --wait=false avoids a CLI bug: --if-exists skip plus --wait then waits on
	// an empty sandbox id and fails after the timeout with "already exists".
	args = append(args, "--wait=false")

	res, err := c.run(ctx, ref.Folder, "creating sandbox", args...)
	if err != nil && !isAlreadyExists(res) {
		return ref, err
	}

	if err := c.waitUntil(ctx, ref, "ready", waitTimeout); err != nil {
		return ref, err
	}
	return ref, nil
}

// DeleteSandbox removes a sandbox. A sandbox that is already gone is treated as
// success, so repeated teardown converges instead of failing.
func (c *Client) DeleteSandbox(ctx context.Context, ref SandboxRef) error {
	if err := c.validate(ref); err != nil {
		return err
	}
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	res, err := c.run(ctx, ref.Folder, "removing sandbox", "sandbox", "remove", ref.Name, "-f")
	if err != nil {
		if IsNotFound(res) || errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// SuspendSandbox releases a sandbox's compute while preserving its state, and
// returns once it is suspended. A sandbox that is already suspended is left
// alone, so retried teardown converges.
func (c *Client) SuspendSandbox(ctx context.Context, ref SandboxRef) error {
	if err := c.validate(ref); err != nil {
		return err
	}
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	stage, err := c.LifecycleStage(ctx, ref)
	if err != nil {
		return err
	}
	if stage == StageSuspended {
		return nil
	}
	if _, err := c.run(ctx, ref.Folder, "suspending sandbox", "sandbox", "suspend", ref.Name, "--wait"); err != nil {
		return err
	}
	return c.WaitFor(ctx, ref, "suspended")
}

// ResumeOptions tunes how far ResumeSandbox waits.
type ResumeOptions struct {
	// Workload, when set, makes resume wait until a command can actually run
	// there rather than only until the control plane reports readiness.
	Workload string
	// UID is the user the reachability probe runs as. Nil selects
	// DefaultExecUID.
	UID *int
}

// ResumeSandbox brings a suspended sandbox back online.
//
// Readiness and reachability are separate conditions: a resumed sandbox is ready
// once its workloads are scheduled, while a command additionally needs the
// workload to be accepting connections. Callers expect a sandbox they can use,
// so setting ResumeOptions.Workload waits for the second condition too.
func (c *Client) ResumeSandbox(ctx context.Context, ref SandboxRef, opts ResumeOptions) error {
	if err := c.validate(ref); err != nil {
		return err
	}
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	return c.resume(ctx, ref, opts)
}

func (c *Client) resume(ctx context.Context, ref SandboxRef, opts ResumeOptions) error {
	if _, err := c.run(ctx, ref.Folder, "resuming sandbox", "sandbox", "resume", ref.Name, "--wait"); err != nil {
		return err
	}
	if err := c.WaitFor(ctx, ref, "ready"); err != nil {
		return err
	}
	if opts.Workload == "" {
		return nil
	}
	return c.waitReachable(ctx, ref, opts)
}

// EnsureResumed resumes a sandbox only if it is currently suspended.
func (c *Client) EnsureResumed(ctx context.Context, ref SandboxRef, opts ResumeOptions) error {
	if err := c.validate(ref); err != nil {
		return err
	}
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	stage, err := c.LifecycleStage(ctx, ref)
	if err != nil {
		return err
	}
	if stage != StageSuspended {
		return nil
	}
	return c.resume(ctx, ref, opts)
}

// LifecycleStage reports the sandbox's lifecycle stage, for example
// StageRunning or StageSuspended.
func (c *Client) LifecycleStage(ctx context.Context, ref SandboxRef) (string, error) {
	if err := c.validate(ref); err != nil {
		return "", err
	}
	res, err := c.run(ctx, ref.Folder, "reading sandbox state", "sandbox", "show", ref.Name, "-o", "json")
	if err != nil {
		return "", err
	}
	var payload struct {
		Status struct {
			Sandbox struct {
				RunStage       string `json:"run_stage"`
				LifecycleStage string `json:"lifecycle_stage"`
			} `json:"sandbox"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &payload); err != nil {
		return "", fmt.Errorf("crafting: parsing state of sandbox %s: %w", ref.Name, err)
	}
	return payload.Status.Sandbox.LifecycleStage, nil
}

// WaitFor blocks until the sandbox reaches a state, for example "ready" or
// "suspended".
func (c *Client) WaitFor(ctx context.Context, ref SandboxRef, expect string) error {
	return c.waitUntil(ctx, ref, expect, c.lifecycleTimeout)
}

func (c *Client) waitUntil(ctx context.Context, ref SandboxRef, expect string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = c.lifecycleTimeout
	}
	_, err := c.run(ctx, ref.Folder, "waiting for sandbox to be "+expect,
		"wait", "sandbox", ref.Name, "--expect", expect, "--timeout", timeout.String())
	return err
}

// waitReachable blocks until a trivial command succeeds in the workload. The
// probe is a no-op command, so retrying it is safe in a way that retrying a
// caller's command would not be.
func (c *Client) waitReachable(ctx context.Context, ref SandboxRef, opts ResumeOptions) error {
	probe := ExecOptions{Workload: opts.Workload, UID: opts.UID}
	var lastErr error
	for attempt := range c.probeAttempts {
		_, err := c.exec(ctx, ref, probe, "true")
		if err == nil {
			return nil
		}
		lastErr = err
		if errors.Is(err, ErrNotFound) {
			break
		}
		if attempt == c.probeAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.probeInterval):
		}
	}
	return fmt.Errorf("crafting: workload %s/%s did not become reachable: %w", ref.Name, opts.Workload, lastErr)
}

func (c *Client) validate(ref SandboxRef) error {
	if ref.Name == "" {
		return fmt.Errorf("crafting: missing sandbox name")
	}
	return nil
}
