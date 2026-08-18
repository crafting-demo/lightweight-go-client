// Package crafting is a lightweight Go client for driving Crafting sandboxes.
//
// A Crafting sandbox is a production-like environment: a workspace plus the
// dependency workloads it talks to, such as databases and caches. This package
// exposes that lifecycle to Go programs — create, execute, suspend, resume,
// snapshot, fork and delete — by driving the public cs CLI, which must be on
// the host running the program.
//
// The zero-configuration path uses whatever credentials cs already has, which
// is what a program running inside a Crafting sandbox inherits:
//
//	client, err := crafting.NewClient(crafting.Options{Folder: "lab"})
//	if err != nil {
//	    return err
//	}
//
//	ref, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
//	    Name:     "my-sandbox",
//	    Template: "agent-sandbox",
//	})
//	if err != nil {
//	    return err
//	}
//	defer client.DeleteSandbox(ctx, ref)
//
//	res, err := client.Exec(ctx, ref, crafting.ExecOptions{Workload: "dev"}, "go test ./...")
//
// Snapshots are composite: a single opaque CompositeSnapshot carries the
// workspace home directory, the data of each dependency, and optionally the
// workspace root filesystem, so a sandbox restored from one has both the files
// and the database the original had.
package crafting

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Defaults applied when the corresponding option is left unset.
const (
	DefaultBinary           = "cs"
	DefaultExecUID          = 1000
	DefaultUsePool          = "auto"
	DefaultCreateTimeout    = 10 * time.Minute
	DefaultLifecycleTimeout = 5 * time.Minute
	DefaultProbeAttempts    = 6
	DefaultProbeInterval    = 3 * time.Second

	// authTokenEnv is how cs reads a service-account token without putting it
	// on the command line, where ps would expose it.
	authTokenEnv = "CRAFTING_SANDBOX_AUTH_TOKEN"
)

// ErrNotFound reports that the sandbox or workload the client targeted is
// absent. Unlike a transport failure, retrying will not help.
var ErrNotFound = errors.New("crafting: not found")

// Options configures a Client. Every field is optional: the zero value produces
// a client that shells out to cs in the caller's default organization and
// folder, using whatever session cs already holds.
type Options struct {
	// Org is the Crafting organization. Empty uses the CLI's current org.
	Org string
	// Folder is the folder objects are created in and resolved against. Empty
	// uses the CLI's default folder.
	Folder string

	// Token is a service-account token used to authenticate cs on first use.
	// When empty the client relies on ambient credentials, which is the case
	// for a program running inside a Crafting sandbox.
	Token string
	// ConfigDir isolates cs session state, so a program does not disturb a
	// developer's own session on the same host.
	ConfigDir string

	// Binary is the cs executable to invoke. Ignored when Runner is set.
	Binary string
	// Runner replaces the subprocess transport, which is how callers test
	// against this client without a Crafting organization.
	Runner Runner

	// LifecycleTimeout bounds each wait for a sandbox to reach a given state.
	LifecycleTimeout time.Duration

	// ProbeAttempts and ProbeInterval tune the post-resume reachability check.
	ProbeAttempts int
	ProbeInterval time.Duration
}

// Client drives Crafting through the cs CLI. It is safe for concurrent use.
type Client struct {
	org       string
	folder    string
	token     string
	configDir string
	runner    Runner

	lifecycleTimeout time.Duration
	probeAttempts    int
	probeInterval    time.Duration

	authMu        sync.Mutex
	authenticated bool
}

// NewClient builds a Client from opts, filling in defaults for unset fields.
func NewClient(opts Options) (*Client, error) {
	if opts.LifecycleTimeout < 0 {
		return nil, fmt.Errorf("crafting: negative lifecycle timeout %s", opts.LifecycleTimeout)
	}
	if opts.ProbeAttempts < 0 {
		return nil, fmt.Errorf("crafting: negative probe attempts %d", opts.ProbeAttempts)
	}
	if opts.ProbeInterval < 0 {
		return nil, fmt.Errorf("crafting: negative probe interval %s", opts.ProbeInterval)
	}

	runner := opts.Runner
	if runner == nil {
		runner = NewExecRunner(opts.Binary)
	}
	c := &Client{
		org:              opts.Org,
		folder:           opts.Folder,
		token:            opts.Token,
		configDir:        opts.ConfigDir,
		runner:           runner,
		lifecycleTimeout: opts.LifecycleTimeout,
		probeAttempts:    opts.ProbeAttempts,
		probeInterval:    opts.ProbeInterval,
	}
	if c.lifecycleTimeout == 0 {
		c.lifecycleTimeout = DefaultLifecycleTimeout
	}
	if c.probeAttempts == 0 {
		c.probeAttempts = DefaultProbeAttempts
	}
	if c.probeInterval == 0 {
		c.probeInterval = DefaultProbeInterval
	}
	return c, nil
}

// Folder returns the folder the client resolves unqualified names against.
func (c *Client) Folder() string { return c.folder }

// SandboxRef identifies a sandbox. Crafting resolves names within a folder, so
// both parts travel together and a ref remains valid when a caller stores it and
// uses it later against a client configured for a different folder.
type SandboxRef struct {
	Folder string
	Name   string
}

// String renders the ref as a folder-qualified name, which is a stable form to
// persist and hand back to Ref.
func (r SandboxRef) String() string { return Qualify(r.Folder, r.Name) }

// Ref resolves a sandbox name, which may already be folder-qualified, against
// the client's folder.
func (c *Client) Ref(name string) SandboxRef {
	folder, bare := SplitQualified(c.folder, name)
	return SandboxRef{Folder: folder, Name: bare}
}

// globalArgs renders the flags every cs invocation carries.
func (c *Client) globalArgs(folder string) []string {
	var args []string
	if c.org != "" {
		args = append(args, "-O", c.org)
	}
	if folder != "" {
		args = append(args, "--folder", folder)
	}
	return args
}

// env isolates cs state so the client never disturbs another session on the
// same host.
func (c *Client) env() []string {
	if c.configDir == "" {
		return nil
	}
	return []string{"SANDBOX_CONFIG_DIR=" + c.configDir}
}

// run invokes cs and converts a non-zero exit into an error, since every caller
// of this helper treats a CLI verdict as failure. Exec does not use it, because
// there a non-zero exit is a legitimate result.
func (c *Client) run(ctx context.Context, folder, what string, args ...string) (*Result, error) {
	full := append(args, c.globalArgs(folder)...)
	res, err := c.runner.Run(ctx, c.env(), full...)
	if err != nil {
		return res, fmt.Errorf("crafting: %s: %w", what, err)
	}
	if res.ExitCode != 0 {
		err := fmt.Errorf("crafting: %s: cs exited %d: %s", what, res.ExitCode, firstLine(res.Stderr))
		if IsNotFound(res) {
			return res, fmt.Errorf("%w: %w", err, ErrNotFound)
		}
		return res, err
	}
	return res, nil
}

// ensureAuth logs cs in once per client. With no token configured the client
// assumes ambient credentials. A failed attempt is not cached, so a later call
// with a live context can recover from a transient error.
func (c *Client) ensureAuth(ctx context.Context) error {
	if c.token == "" {
		return nil
	}
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authenticated {
		return nil
	}
	env := append(c.env(), authTokenEnv+"="+c.token)
	args := append([]string{"login", "--if-needed"}, c.globalArgs(c.folder)...)
	res, err := c.runner.Run(ctx, env, args...)
	if err != nil {
		return fmt.Errorf("crafting: authenticating cs: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("crafting: authenticating cs: cs exited %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	c.authenticated = true
	return nil
}

// IsNotFound reports whether a failure means the object is already absent, which
// makes an operation on it a no-op rather than an error. Phrases are anchored on
// the spellings cs and the control plane actually emit, so an unrelated "file
// not found" does not make a delete look successful.
//
// The client already applies this to its own deletions; it is exported for
// callers that issue their own commands through a Runner.
func IsNotFound(res *Result) bool {
	if res == nil {
		return false
	}
	s := strings.ToLower(res.Stderr + res.Stdout)
	for _, phrase := range []string{
		"does not exist",
		"sandbox not found",
		"snapshot not found",
		"workload not found",
		"no matching found",
		"not_found:",
		"code = notfound",
	} {
		if strings.Contains(s, phrase) {
			return true
		}
	}
	return false
}

func isAlreadyExists(res *Result) bool {
	if res == nil {
		return false
	}
	s := strings.ToLower(res.Stderr + res.Stdout)
	return strings.Contains(s, "already exists")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if s == "" {
		return "no error output"
	}
	return s
}
