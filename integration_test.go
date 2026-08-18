//go:build integration

// These tests drive the real cs CLI against a real Crafting organization. They
// create and delete sandboxes and snapshots, so they are gated behind a build
// tag and an explicit template.
//
//	CRAFTING_TEST_ORG=eng \
//	CRAFTING_TEST_FOLDER=lab \
//	CRAFTING_TEST_TEMPLATE=agent-sandbox \
//	CRAFTING_TEST_WORKSPACE=dev \
//	CRAFTING_TEST_DEPENDENCY=db \
//	go test -tags integration -timeout 40m -v ./...
//
// Required:
//
//	CRAFTING_TEST_TEMPLATE  a template with a workspace and a dependency
//	CRAFTING_TEST_WORKSPACE the workspace workload name
//
// Optional:
//
//	CRAFTING_TEST_DEPENDENCY dependency workload to include in snapshots
//	CRAFTING_TEST_FOLDER     folder to create objects in
//	CRAFTING_TEST_ORG        organization to create objects in
package crafting_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	crafting "github.com/crafting-demo/lightweight-go-client"
)

// runNonce is mixed into every derived name so a leaked sandbox from a previous
// crashed run cannot be reused by --if-exists skip.
var runNonce = func() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}()

// Tracked objects are deleted again after m.Run returns, so a failed Cleanup
// cannot leave sandboxes or snapshots behind in the org.
var (
	trackedMu        sync.Mutex
	trackedClient    *crafting.Client
	trackedSandboxes []crafting.SandboxRef
	trackedSnapshots []*crafting.CompositeSnapshot
)

func TestMain(m *testing.M) {
	code := m.Run()
	sweepTracked()
	os.Exit(code)
}

func sweepTracked() {
	trackedMu.Lock()
	client := trackedClient
	sandboxes := append([]crafting.SandboxRef(nil), trackedSandboxes...)
	snapshots := append([]*crafting.CompositeSnapshot(nil), trackedSnapshots...)
	trackedMu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, snap := range snapshots {
		if err := client.DeleteSnapshot(ctx, snap); err != nil {
			fmt.Fprintf(os.Stderr, "sweep: deleting snapshot %v: %v\n", snap.Components(), err)
		}
	}
	for _, ref := range sandboxes {
		if err := client.DeleteSandbox(ctx, ref); err != nil {
			fmt.Fprintf(os.Stderr, "sweep: deleting sandbox %s: %v\n", ref, err)
		}
	}
}

type liveEnv struct {
	client    *crafting.Client
	template  string
	workspace string
	deps      []string
}

func liveClient(t *testing.T) liveEnv {
	t.Helper()
	template := os.Getenv("CRAFTING_TEST_TEMPLATE")
	workspace := os.Getenv("CRAFTING_TEST_WORKSPACE")
	if template == "" || workspace == "" {
		t.Skip("set CRAFTING_TEST_TEMPLATE and CRAFTING_TEST_WORKSPACE to run integration tests")
	}
	client, err := crafting.NewClient(crafting.Options{
		Org:    os.Getenv("CRAFTING_TEST_ORG"),
		Folder: os.Getenv("CRAFTING_TEST_FOLDER"),
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	trackedMu.Lock()
	trackedClient = client
	trackedMu.Unlock()

	env := liveEnv{client: client, template: template, workspace: workspace}
	if dep := os.Getenv("CRAFTING_TEST_DEPENDENCY"); dep != "" {
		env.deps = []string{dep}
	}
	return env
}

func (e liveEnv) execOptions() crafting.ExecOptions {
	return crafting.ExecOptions{Workload: e.workspace, AutoResume: true}
}

func (e liveEnv) create(t *testing.T, ctx context.Context, seed string, from *crafting.CompositeSnapshot) crafting.SandboxRef {
	t.Helper()
	name, err := crafting.DeriveSandboxName("lgcit", seed+"|"+runNonce)
	if err != nil {
		t.Fatalf("deriving name: %v", err)
	}
	ref := e.client.Ref(name)
	trackSandbox(ref)
	// Register cleanup before create so a sandbox that comes up and then fails
	// the wait is still removed.
	t.Cleanup(func() {
		if err := e.client.DeleteSandbox(context.Background(), ref); err != nil {
			t.Errorf("deleting sandbox %s: %v", ref, err)
		}
	})
	ref, err = e.client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
		Name:     name,
		Template: e.template,
		Env:      []string{"INTEGRATION_SEED=" + seed},
		From:     from,
	})
	if err != nil {
		t.Fatalf("creating sandbox %s: %v", name, err)
	}
	t.Logf("sandbox: %s", ref)
	return ref
}

func (e liveEnv) trackSnapshot(t *testing.T, snap *crafting.CompositeSnapshot) {
	t.Helper()
	if snap == nil {
		return
	}
	trackSnapshot(snap)
	// Cleanup must be registered on the test that outlives every use of the
	// snapshot. Registering it on a subtest deletes the snapshot when that
	// subtest returns, before a later fork can restore it.
	t.Cleanup(func() {
		if err := e.client.DeleteSnapshot(context.Background(), snap); err != nil {
			t.Errorf("deleting snapshot %v: %v", snap.Components(), err)
		}
	})
}

func trackSandbox(ref crafting.SandboxRef) {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	trackedSandboxes = append(trackedSandboxes, ref)
}

func trackSnapshot(snap *crafting.CompositeSnapshot) {
	trackedMu.Lock()
	defer trackedMu.Unlock()
	trackedSnapshots = append(trackedSnapshots, snap)
}

func (e liveEnv) mustExec(t *testing.T, ctx context.Context, ref crafting.SandboxRef, cmd string) *crafting.ExecResult {
	t.Helper()
	res, err := e.client.Exec(ctx, ref, e.execOptions(), cmd)
	if err != nil {
		t.Fatalf("executing %q: %v", cmd, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("command %q exited %d\nstdout: %s\nstderr: %s", cmd, res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}

func (e liveEnv) mustStage(t *testing.T, ctx context.Context, ref crafting.SandboxRef, want string) {
	t.Helper()
	got, err := e.client.LifecycleStage(ctx, ref)
	if err != nil {
		t.Fatalf("reading lifecycle stage of %s: %v", ref, err)
	}
	if got != want {
		t.Fatalf("lifecycle stage of %s = %q, want %q", ref, got, want)
	}
}

func missingRef(t *testing.T, client *crafting.Client) crafting.SandboxRef {
	t.Helper()
	name, err := crafting.DeriveSandboxName("lgcit", "missing|"+runNonce+"|"+t.Name())
	if err != nil {
		t.Fatalf("deriving missing name: %v", err)
	}
	return client.Ref(name)
}

// TestIntegrationMissingObjects covers the no-op and ErrNotFound paths against
// names that were never created, so it needs no live sandbox.
func TestIntegrationMissingObjects(t *testing.T) {
	env := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ref := missingRef(t, env.client)

	t.Run("delete is a no-op", func(t *testing.T) {
		if err := env.client.DeleteSandbox(ctx, ref); err != nil {
			t.Errorf("deleting an absent sandbox should succeed, got %v", err)
		}
	})

	t.Run("exec is ErrNotFound", func(t *testing.T) {
		_, err := env.client.Exec(ctx, ref, env.execOptions(), "true")
		if !errors.Is(err, crafting.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("lifecycle stage fails", func(t *testing.T) {
		if _, err := env.client.LifecycleStage(ctx, ref); err == nil {
			t.Error("reading a missing sandbox should fail")
		}
	})

	t.Run("delete snapshot of empty composite is nothing to do", func(t *testing.T) {
		if err := env.client.DeleteSnapshot(ctx, nil); err == nil {
			t.Error("deleting a nil snapshot should fail")
		}
	})
}

// TestIntegrationCreateExecSuspendDelete is the core lifecycle: provision,
// run commands, park and wake the sandbox, tear it down twice.
func TestIntegrationCreateExecSuspendDelete(t *testing.T) {
	env := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	ref := env.create(t, ctx, "exec-lifecycle", nil)

	t.Run("environment reaches the sandbox", func(t *testing.T) {
		res := env.mustExec(t, ctx, ref, `printf '%s' "$INTEGRATION_SEED"`)
		if res.Stdout != "exec-lifecycle" {
			t.Errorf("INTEGRATION_SEED = %q, want exec-lifecycle", res.Stdout)
		}
	})

	t.Run("commands run as the workspace user", func(t *testing.T) {
		res := env.mustExec(t, ctx, ref, `printf '%s %s' "$(id -u)" "$HOME"`)
		if res.Stdout != "1000 /home/owner" {
			t.Errorf("uid/home = %q, want 1000 /home/owner (home snapshots read /home/owner/.snapshot)", res.Stdout)
		}
	})

	t.Run("uid override runs as root", func(t *testing.T) {
		res, err := env.client.Exec(ctx, ref, crafting.ExecOptions{
			Workload: env.workspace, UID: crafting.UID(0), AutoResume: true,
		}, `printf '%s' "$(id -u)"`)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 || res.Stdout != "0" {
			t.Errorf("uid = %q (exit %d), want 0", res.Stdout, res.ExitCode)
		}
	})

	t.Run("working directory is honoured", func(t *testing.T) {
		res, err := env.client.Exec(ctx, ref, crafting.ExecOptions{
			Workload: env.workspace, Dir: "/tmp", AutoResume: true,
		}, `pwd`)
		if err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "/tmp" {
			t.Errorf("pwd = %q (exit %d), want /tmp", res.Stdout, res.ExitCode)
		}
	})

	t.Run("exit codes and streams are faithful", func(t *testing.T) {
		res, err := env.client.Exec(ctx, ref, env.execOptions(), "echo to-stdout; echo to-stderr >&2; exit 42")
		if err != nil {
			t.Fatalf("a failing command should be a result, not an error: %v", err)
		}
		if res.ExitCode != 42 {
			t.Errorf("exit code = %d, want 42", res.ExitCode)
		}
		if strings.TrimSpace(res.Stdout) != "to-stdout" {
			t.Errorf("stdout = %q, want to-stdout", res.Stdout)
		}
		if strings.TrimSpace(res.Stderr) != "to-stderr" {
			t.Errorf("stderr = %q, want to-stderr", res.Stderr)
		}
	})

	t.Run("multi-line commands work", func(t *testing.T) {
		res := env.mustExec(t, ctx, ref, "set -e\nfor i in 1 2 3; do\n  echo \"line $i\"\ndone")
		if !strings.Contains(res.Stdout, "line 3") {
			t.Errorf("stdout = %q, want three lines", res.Stdout)
		}
	})

	t.Run("create is idempotent for the same name", func(t *testing.T) {
		again, err := env.client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
			Name:     ref.Name,
			Template: env.template,
		})
		if err != nil {
			t.Fatalf("re-creating: %v", err)
		}
		if again.String() != ref.String() {
			t.Errorf("retry produced %s, want %s", again, ref)
		}
	})

	env.mustExec(t, ctx, ref, `echo PARKED > "$HOME/marker.txt"`)
	env.mustStage(t, ctx, ref, crafting.StageRunning)

	t.Run("suspend and resume preserve state", func(t *testing.T) {
		if err := env.client.SuspendSandbox(ctx, ref); err != nil {
			t.Fatalf("suspending: %v", err)
		}
		env.mustStage(t, ctx, ref, crafting.StageSuspended)

		if err := env.client.SuspendSandbox(ctx, ref); err != nil {
			t.Fatalf("suspending an already-suspended sandbox: %v", err)
		}

		if err := env.client.ResumeSandbox(ctx, ref, crafting.ResumeOptions{Workload: env.workspace}); err != nil {
			t.Fatalf("resuming: %v", err)
		}
		env.mustStage(t, ctx, ref, crafting.StageRunning)
		res := env.mustExec(t, ctx, ref, `cat "$HOME/marker.txt"`)
		if strings.TrimSpace(res.Stdout) != "PARKED" {
			t.Errorf("marker after resume = %q, want PARKED", res.Stdout)
		}
	})

	t.Run("commands work on a suspended sandbox without polluting stderr", func(t *testing.T) {
		if err := env.client.SuspendSandbox(ctx, ref); err != nil {
			t.Fatalf("suspending: %v", err)
		}
		res, err := env.client.Exec(ctx, ref, env.execOptions(), "echo awake")
		if err != nil {
			t.Fatalf("executing against a suspended sandbox: %v", err)
		}
		if strings.TrimSpace(res.Stdout) != "awake" {
			t.Errorf("stdout = %q, want awake", res.Stdout)
		}
		if res.Stderr != "" {
			t.Errorf("stderr = %q, want it empty after an implicit resume", res.Stderr)
		}
	})

	t.Run("deleting twice succeeds and exec then reports ErrNotFound", func(t *testing.T) {
		if err := env.client.DeleteSandbox(ctx, ref); err != nil {
			t.Fatalf("first delete: %v", err)
		}
		if err := env.client.DeleteSandbox(ctx, ref); err != nil {
			t.Errorf("second delete should be a no-op, got %v", err)
		}
		_, err := env.client.Exec(ctx, ref, env.execOptions(), "true")
		if !errors.Is(err, crafting.ErrNotFound) {
			t.Errorf("exec after delete = %v, want ErrNotFound", err)
		}
	})
}

// TestIntegrationSnapshotFork is the reason snapshots are composite: a fork
// inherits home files and dependency data, then diverges without touching the
// origin.
func TestIntegrationSnapshotFork(t *testing.T) {
	env := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	origin := env.create(t, ctx, "snapshot-origin", nil)

	env.mustExec(t, ctx, origin, `echo ORIGIN_V1 > "$HOME/marker.txt"`)
	if len(env.deps) > 0 {
		env.mustExec(t, ctx, origin, seedDatabaseScript)
	}

	var snapshot *crafting.CompositeSnapshot
	t.Run("snapshot leaves the origin running", func(t *testing.T) {
		snap, err := env.client.SnapshotSandbox(ctx, origin, crafting.SnapshotOptions{
			Workspace:    env.workspace,
			Dependencies: env.deps,
		})
		if err != nil {
			t.Fatalf("snapshotting: %v", err)
		}
		snapshot = snap
		t.Logf("snapshot components: %v", snap.Components())
		env.mustExec(t, ctx, origin, "true")
		env.mustStage(t, ctx, origin, crafting.StageRunning)
	})
	if snapshot == nil {
		t.Fatal("cannot continue without a snapshot")
	}
	env.trackSnapshot(t, snapshot)

	encoded, err := snapshot.Encode()
	if err != nil {
		t.Fatalf("encoding snapshot: %v", err)
	}
	restored, err := crafting.DecodeCompositeSnapshot(encoded)
	if err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}

	fork := env.create(t, ctx, "snapshot-fork", restored)

	t.Run("fork inherits home and dependency state", func(t *testing.T) {
		if fork.String() == origin.String() {
			t.Fatal("fork reused the origin sandbox")
		}
		res := env.mustExec(t, ctx, fork, `cat "$HOME/marker.txt"`)
		if strings.TrimSpace(res.Stdout) != "ORIGIN_V1" {
			t.Errorf("fork home marker = %q, want ORIGIN_V1", res.Stdout)
		}
		if len(env.deps) > 0 {
			res = env.mustExec(t, ctx, fork, readDatabaseScript)
			if !strings.Contains(res.Stdout, "ORIGIN_ROW_V1") {
				t.Errorf("fork database rows = %q, want the seeded row", res.Stdout)
			}
		}
	})

	t.Run("fork is independent of the origin", func(t *testing.T) {
		env.mustExec(t, ctx, fork, `echo FORK_V2 > "$HOME/marker.txt"`)
		if len(env.deps) > 0 {
			env.mustExec(t, ctx, fork, divergeDatabaseScript)
		}

		res := env.mustExec(t, ctx, origin, `cat "$HOME/marker.txt"`)
		if strings.TrimSpace(res.Stdout) != "ORIGIN_V1" {
			t.Errorf("origin home marker = %q; the fork leaked into the origin", res.Stdout)
		}
		if len(env.deps) > 0 {
			res = env.mustExec(t, ctx, origin, readDatabaseScript)
			if strings.Contains(res.Stdout, "FORK_ROW_V2") {
				t.Errorf("origin database = %q; the fork leaked into the origin", res.Stdout)
			}
		}
	})

	t.Run("snapshot of a suspended sandbox resumes it first", func(t *testing.T) {
		if err := env.client.SuspendSandbox(ctx, origin); err != nil {
			t.Fatalf("suspending origin: %v", err)
		}
		snap, err := env.client.SnapshotSandbox(ctx, origin, crafting.SnapshotOptions{
			Workspace:    env.workspace,
			Dependencies: env.deps,
		})
		if err != nil {
			t.Fatalf("snapshotting a suspended sandbox: %v", err)
		}
		env.trackSnapshot(t, snap)
		env.mustStage(t, ctx, origin, crafting.StageRunning)
	})
}

const seedDatabaseScript = `
set -e
for i in 1 2 3 4 5 6 7 8 9 10; do
  if psql -h "$DB_SERVICE_HOST" -U postgres -q -c "SELECT 1" >/dev/null 2>&1; then
    break
  fi
  sleep 3
done
psql -h "$DB_SERVICE_HOST" -U postgres -q -c "CREATE TABLE IF NOT EXISTS agent_state(id serial primary key, note text);"
psql -h "$DB_SERVICE_HOST" -U postgres -q -c "INSERT INTO agent_state(note) VALUES ('ORIGIN_ROW_V1');"
`

const readDatabaseScript = `psql -h "$DB_SERVICE_HOST" -U postgres -At -c "SELECT note FROM agent_state ORDER BY id;"`

const divergeDatabaseScript = `psql -h "$DB_SERVICE_HOST" -U postgres -q -c "INSERT INTO agent_state(note) VALUES ('FORK_ROW_V2');"`
