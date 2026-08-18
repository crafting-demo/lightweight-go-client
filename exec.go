package crafting

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ExecResult is the outcome of a command run inside a sandbox.
//
// A command that runs and fails is a result, not an error: it is reported here
// with a non-zero ExitCode. Exec returns an error only when the command could
// not be run at all. A missing sandbox or workload is ErrNotFound; other
// errors are typically worth retrying.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ExecOptions selects where and as whom a command runs.
type ExecOptions struct {
	// Workload is the workload to run in, usually the sandbox's workspace.
	Workload string
	// UID is the user the command runs as. Nil selects DefaultExecUID, the
	// workspace user, whose home directory is what home snapshots capture.
	// Running as root instead would write into /root and be lost on a fork.
	UID *int
	// Dir is the working directory. Empty uses the workload's default.
	Dir string
	// Timeout bounds the command. Zero means the caller's context governs.
	Timeout time.Duration
	// AutoResume resumes the sandbox first if it is suspended. Without it a
	// command against a suspended sandbox is resumed by cs itself, which
	// reports that progress on stderr and pollutes the command's own output.
	AutoResume bool
}

// UID is a convenience for setting ExecOptions.UID, since a Go literal cannot
// take the address of an integer constant.
func UID(uid int) *int { return &uid }

// Exec runs cmd inside a sandbox workload and reports its output and exit
// status. cmd is a shell script and may span multiple lines.
func (c *Client) Exec(ctx context.Context, ref SandboxRef, opts ExecOptions, cmd string) (*ExecResult, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}
	if opts.AutoResume {
		if err := c.EnsureResumed(ctx, ref, ResumeOptions{Workload: opts.Workload, UID: opts.UID}); err != nil {
			return nil, err
		}
	}
	return c.exec(ctx, ref, opts, cmd)
}

// exec runs a command and parses the exit-code sentinel, without the auth and
// resume handling Exec performs. Internal callers use it to avoid re-entering
// the resume path they are already inside.
func (c *Client) exec(ctx context.Context, ref SandboxRef, opts ExecOptions, cmd string) (*ExecResult, error) {
	if opts.Workload == "" {
		return nil, fmt.Errorf("crafting: no workload to run in")
	}
	if cmd == "" {
		return nil, fmt.Errorf("crafting: empty command")
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	uid := DefaultExecUID
	if opts.UID != nil {
		uid = *opts.UID
	}

	args := []string{
		"exec",
		// Without -T, cs allocates a TTY and stdout and stderr are merged.
		"-T",
		"-u", strconv.Itoa(uid),
		"-W", ref.Name + "/" + opts.Workload,
	}
	if opts.Dir != "" {
		args = append(args, "-w", opts.Dir)
	}
	// The global flags must precede the -- separator, so they are placed here
	// rather than appended by the shared run helper.
	args = append(args, c.globalArgs(ref.Folder)...)
	args = append(args, "--", "bash", "-c", remoteWrapper, "_", encodeCommand(cmd))

	res, err := c.runner.Run(ctx, c.env(), args...)
	if err != nil {
		return nil, fmt.Errorf("crafting: executing command in %s/%s: %w", ref.Name, opts.Workload, err)
	}

	stdout, exitCode, found := parseSentinel(res.Stdout)
	if !found {
		// The wrapper never reported, so the command did not run to completion:
		// the sandbox was unreachable, cs rejected the call, or the connection
		// dropped. Surfacing this as an error lets the caller retry, except when
		// the target is simply gone — retrying that cannot succeed.
		err := fmt.Errorf("crafting: command did not complete in %s/%s (cs exited %d): %s",
			ref.Name, opts.Workload, res.ExitCode, firstLine(res.Stderr))
		if IsNotFound(res) {
			return nil, fmt.Errorf("%w: %w", err, ErrNotFound)
		}
		return nil, err
	}

	return &ExecResult{Stdout: stdout, Stderr: res.Stderr, ExitCode: exitCode}, nil
}

// The remote wrapper script serves two purposes.
//
// It runs the caller's command in a subshell so that an `exit` inside the
// command cannot terminate the wrapper before the exit code is reported, and it
// prints that exit code as a sentinel on stdout. Reporting the code in-band is
// what lets the client distinguish a command that ran and failed, which is a
// legitimate ExecResult, from a command that never reached the sandbox, which is
// a retryable error.
//
// The command itself arrives base64-encoded in $1 rather than interpolated into
// the script, which keeps multi-line input intact and removes every quoting
// hazard.
const remoteWrapper = `( eval "$(printf %s "$1" | base64 -d)" ); printf "\n` + sentinelPrefixLiteral + `%d__\n" "$?"`

const (
	sentinelPrefixLiteral = "__CS_EXIT__"
	sentinelMarker        = "\n" + sentinelPrefixLiteral
	sentinelSuffix        = "__"
)

// ExitSentinel renders the in-band exit-code marker the remote wrapper appends
// to stdout, including its leading newline. It is exported so that test doubles
// standing in for the cs CLI can produce output this package will parse.
func ExitSentinel(exitCode int) string {
	return sentinelMarker + strconv.Itoa(exitCode) + sentinelSuffix + "\n"
}

func encodeCommand(cmd string) string {
	return base64.StdEncoding.EncodeToString([]byte(cmd))
}

// parseSentinel splits captured stdout into the command's own output and its
// exit code. found is false when the sentinel is absent, which means the
// wrapper never completed and the invocation should be treated as a transport
// failure rather than a command result.
func parseSentinel(stdout string) (output string, exitCode int, found bool) {
	idx := strings.LastIndex(stdout, sentinelMarker)
	if idx < 0 {
		return stdout, 0, false
	}
	rest := stdout[idx+len(sentinelMarker):]
	end := strings.Index(rest, sentinelSuffix)
	if end < 0 {
		return stdout, 0, false
	}
	code, err := strconv.Atoi(rest[:end])
	if err != nil {
		return stdout, 0, false
	}
	// Everything before the marker is the command's output, byte for byte:
	// the wrapper contributed the leading newline of the marker itself, so no
	// trailing newline of the real output is consumed.
	return stdout[:idx], code, true
}

// writeFileScript builds a script that writes content to path, creating parent
// directories. path is a shell expression the caller controls, so it is
// double-quoted to allow $HOME to expand while still tolerating spaces. The
// heredoc delimiter is quoted so the body is written literally, with no
// expansion or command substitution.
func writeFileScript(path, content string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mkdir -p \"$(dirname \"%s\")\"\n", path)
	fmt.Fprintf(&b, "cat > \"%s\" <<'CRAFTING_EOF'\n", path)
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("CRAFTING_EOF\n")
	return b.String()
}
