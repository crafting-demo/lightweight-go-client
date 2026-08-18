// Package craftingtest provides a fake cs CLI for testing code that drives
// Crafting through the crafting client.
//
// Inject a Runner into crafting.Options and script the responses the CLI would
// give, so behaviour can be asserted without a Crafting organization:
//
//	runner := craftingtest.NewRunner().
//	    On("sandbox show", craftingtest.RunningState(), nil).
//	    OnExec("build output", "", 0)
//
//	client, err := crafting.NewClient(crafting.Options{Folder: "lab", Runner: runner})
package craftingtest

import (
	"context"
	"strings"
	"sync"

	crafting "github.com/crafting-demo/lightweight-go-client"
)

// Call records one cs invocation.
type Call struct {
	Args []string
	Env  []string
}

// Runner is a crafting.Runner that records invocations and replays scripted
// responses. It is safe for concurrent use.
type Runner struct {
	mu        sync.Mutex
	calls     []Call
	responses []response
}

type response struct {
	// match is a substring of the joined arguments. The first match wins, so
	// more specific patterns must be registered first.
	match string
	// first, when set, matches the first argument exactly. It is how OnExec
	// avoids colliding with a sandbox whose name happens to contain "exec".
	first  string
	result *crafting.Result
	err    error
	fn     func() (*crafting.Result, error)
}

// NewRunner returns a Runner with no scripted responses, which reports every
// command as having succeeded silently.
func NewRunner() *Runner { return &Runner{} }

// On scripts a response for commands whose arguments contain match. Earlier
// registrations take precedence, so register specific patterns first.
func (r *Runner) On(match string, result *crafting.Result, err error) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, response{match: match, result: copyResult(result), err: err})
	return r
}

// OnFunc scripts a response produced per call, which is how a test varies the
// outcome across retries.
func (r *Runner) OnFunc(match string, fn func() (*crafting.Result, error)) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, response{match: match, fn: fn})
	return r
}

// OnCommand scripts a response for invocations whose first argument is cmd.
func (r *Runner) OnCommand(cmd string, result *crafting.Result, err error) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, response{first: cmd, result: copyResult(result), err: err})
	return r
}

// OnCommandFunc scripts a per-call response for invocations whose first
// argument is cmd.
func (r *Runner) OnCommandFunc(cmd string, fn func() (*crafting.Result, error)) *Runner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responses = append(r.responses, response{first: cmd, fn: fn})
	return r
}

// OnExec scripts a command execution whose stdout carries the exit sentinel the
// real remote wrapper emits, so the client parses it as a genuine result.
func (r *Runner) OnExec(stdout, stderr string, exitCode int) *Runner {
	return r.OnCommand("exec", &crafting.Result{
		Stdout: stdout + crafting.ExitSentinel(exitCode),
		Stderr: stderr,
	}, nil)
}

// Run implements crafting.Runner.
func (r *Runner) Run(_ context.Context, env []string, args ...string) (*crafting.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Args: args, Env: env})
	responses := r.responses
	r.mu.Unlock()

	joined := strings.Join(args, " ")
	for _, resp := range responses {
		if resp.first != "" {
			if len(args) == 0 || args[0] != resp.first {
				continue
			}
		} else if !strings.Contains(joined, resp.match) {
			continue
		}
		if resp.fn != nil {
			res, err := resp.fn()
			return copyResult(res), err
		}
		return copyResult(resp.result), resp.err
	}
	return &crafting.Result{}, nil
}

// Calls returns every invocation recorded so far, in order.
func (r *Runner) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// ArgsFor returns the arguments of the first call containing every fragment, or
// nil when no call matched.
func (r *Runner) ArgsFor(fragments ...string) []string {
	for _, c := range r.Calls() {
		joined := strings.Join(c.Args, " ")
		matched := true
		for _, frag := range fragments {
			if !strings.Contains(joined, frag) {
				matched = false
				break
			}
		}
		if matched {
			return c.Args
		}
	}
	return nil
}

// JoinedArgsFor is ArgsFor rendered as one string, which is what assertions
// usually want to match against.
func (r *Runner) JoinedArgsFor(fragments ...string) string {
	return strings.Join(r.ArgsFor(fragments...), " ")
}

// CountMatching reports how many calls contain the fragment.
func (r *Runner) CountMatching(fragment string) int {
	n := 0
	for _, c := range r.Calls() {
		if strings.Contains(strings.Join(c.Args, " "), fragment) {
			n++
		}
	}
	return n
}

func copyResult(r *crafting.Result) *crafting.Result {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

// RunningState is the JSON the CLI reports for a running sandbox.
func RunningState() *crafting.Result {
	return &crafting.Result{Stdout: `{"status":{"sandbox":{"run_stage":"READY","lifecycle_stage":"RUNNING"}}}`}
}

// SuspendedState is the JSON the CLI reports for a suspended sandbox.
func SuspendedState() *crafting.Result {
	return &crafting.Result{Stdout: `{"status":{"sandbox":{"lifecycle_stage":"SUSPENDED"}}}`}
}
