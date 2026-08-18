package crafting_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	crafting "github.com/crafting-demo/lightweight-go-client"
	"github.com/crafting-demo/lightweight-go-client/craftingtest"
)

func snapshotOptions() crafting.SnapshotOptions {
	return crafting.SnapshotOptions{Workspace: "dev", Dependencies: []string{"db"}}
}

// A snapshot has to capture the workspace home and the dependency data together.
// Restoring only one would resume against state the sandbox never had.
func TestSnapshotCapturesHomeAndDependencies(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnExec("", "", 0)
	client := testClient(t, runner)

	snap, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), snapshotOptions())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Home == "" {
		t.Error("composite has no home component")
	}
	if snap.Deps["db"] == "" {
		t.Error("composite has no db dependency component")
	}
	if snap.Base != "" {
		t.Error("base should not be captured unless requested")
	}
	if snap.Workspace != "dev" {
		t.Errorf("composite workspace = %q, want dev", snap.Workspace)
	}

	homeCall := runner.JoinedArgsFor("snapshot", "create", "home")
	if !strings.Contains(homeCall, "--home") {
		t.Error("home component was not created as a home snapshot")
	}
	// Snapshotting must not rewrite the origin sandbox's definition.
	for _, frag := range []string{"home", "dep-db"} {
		call := runner.JoinedArgsFor("snapshot", "create", frag)
		if !strings.Contains(call, "--auto-update-definition=false") {
			t.Errorf("%s snapshot would mutate the origin definition: %s", frag, call)
		}
	}
	depCall := runner.JoinedArgsFor("snapshot", "create", "dep-db")
	if strings.Contains(depCall, "--home") {
		t.Error("dependency snapshot should not use --home")
	}
	if !strings.Contains(depCall, "-W tsp-abc/db") {
		t.Errorf("dependency snapshot targeted the wrong workload: %s", depCall)
	}
}

// Home snapshots need an includes list, so the client writes one before
// capturing to keep the operation non-interactive.
func TestSnapshotWritesHomeIncludesBeforeCapturing(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnExec("", "", 0)
	client := testClient(t, runner)

	if _, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), snapshotOptions()); err != nil {
		t.Fatal(err)
	}

	execArgs := runner.ArgsFor("exec")
	if execArgs == nil {
		t.Fatal("no command was run to prepare the home snapshot lists")
	}
	script, err := base64.StdEncoding.DecodeString(execArgs[len(execArgs)-1])
	if err != nil {
		t.Fatalf("decoding prepared script: %v", err)
	}
	for _, want := range []string{".snapshot/includes.txt", ".snapshot/excludes.txt"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("prepare script missing %q\ngot:\n%s", want, script)
		}
	}
	// The whole home directory is captured by default, since work lands in
	// arbitrary places.
	if !strings.Contains(string(script), ".cache") {
		t.Error("prepare script did not carry the default excludes")
	}
}

func TestSnapshotIncludesBaseWhenRequested(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnExec("", "", 0)
	client := testClient(t, runner)

	opts := snapshotOptions()
	opts.IncludeBase = true
	snap, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Base == "" {
		t.Error("base component was requested but not captured")
	}
	if strings.Contains(runner.JoinedArgsFor("snapshot", "create", "base"), "--home") {
		t.Error("base snapshot should not use --home")
	}
}

// A half-built composite is worse than none: it consumes storage and can never
// be restored, so the components already created must be removed.
func TestSnapshotRollsBackAfterPartialFailure(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		On("dep-db", &crafting.Result{Stderr: "snapshot quota exceeded", ExitCode: 1}, nil).
		OnExec("", "", 0)
	client := testClient(t, runner)

	if _, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), snapshotOptions()); err == nil {
		t.Fatal("expected the snapshot to fail")
	}
	removal := runner.JoinedArgsFor("snapshot", "remove")
	if removal == "" {
		t.Fatal("the already-created home component was not cleaned up")
	}
	if !strings.Contains(removal, "home") {
		t.Errorf("cleanup removed the wrong component: %s", removal)
	}
}

func TestSnapshotRollbackSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnFunc("dep-db", func() (*crafting.Result, error) {
			cancel()
			return &crafting.Result{Stderr: "snapshot quota exceeded", ExitCode: 1}, nil
		}).
		OnExec("", "", 0)
	client := testClient(t, runner)

	if _, err := client.SnapshotSandbox(ctx, client.Ref("tsp-abc"), snapshotOptions()); err == nil {
		t.Fatal("expected the snapshot to fail")
	}
	if runner.ArgsFor("snapshot", "remove") == nil {
		t.Fatal("rollback must still run after the caller's context is cancelled")
	}
}

func TestSnapshotResumesASuspendedSandboxFirst(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.SuspendedState(), nil).
		OnExec("", "", 0)
	client := testClient(t, runner)

	if _, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), snapshotOptions()); err != nil {
		t.Fatal(err)
	}
	if runner.ArgsFor("sandbox", "resume") == nil {
		t.Error("a suspended sandbox has no running workloads to snapshot")
	}
}

func TestSnapshotRequiresAWorkspace(t *testing.T) {
	client := testClient(t, craftingtest.NewRunner())

	if _, err := client.SnapshotSandbox(context.Background(), client.Ref("tsp-abc"), crafting.SnapshotOptions{}); err == nil {
		t.Error("a snapshot with no workspace should be rejected")
	}
}

func TestDeleteSnapshotRemovesEveryComponent(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner)

	snapshot := &crafting.CompositeSnapshot{
		Workspace: "dev",
		Home:      "lab/snap-home",
		Base:      "lab/snap-base",
		Deps:      map[string]string{"db": "lab/snap-dep-db"},
	}
	if err := client.DeleteSnapshot(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if got := runner.CountMatching("snapshot remove"); got != 3 {
		t.Errorf("removed %d components, want 3", got)
	}
}

// Cleanup runs repeatedly under retries, so components that are already gone
// must not fail the call.
func TestDeleteSnapshotIgnoresMissingComponents(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("snapshot remove", &crafting.Result{Stderr: "not_found: SNAPSHOT: snap-home", ExitCode: 1}, nil)
	client := testClient(t, runner)

	snapshot := &crafting.CompositeSnapshot{Workspace: "dev", Home: "lab/snap-home"}
	if err := client.DeleteSnapshot(context.Background(), snapshot); err != nil {
		t.Errorf("deleting an absent component should succeed, got %v", err)
	}
}

func TestCompositeSnapshotRoundTrip(t *testing.T) {
	original := &crafting.CompositeSnapshot{
		Workspace: "dev",
		Home:      "lab/snap-home",
		Deps:      map[string]string{"db": "lab/snap-db"},
	}
	id, err := original.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := crafting.DecodeCompositeSnapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Home != original.Home || decoded.Deps["db"] != original.Deps["db"] || decoded.Workspace != original.Workspace {
		t.Errorf("round trip lost data: %+v", decoded)
	}
	if original.Version != 0 {
		t.Errorf("Encode mutated the receiver version to %d", original.Version)
	}
}

func TestDecodeCompositeSnapshotRejectsUnusableIDs(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"foreign provider": "e2b-snapshot-id-12345",
		"unknown version":  `{"v":99,"ws":"dev","home":"lab/x"}`,
		"no components":    `{"v":1,"ws":"dev"}`,
		"malformed json":   `{"v":1,`,
	}
	for name, id := range cases {
		if _, err := crafting.DecodeCompositeSnapshot(id); err == nil {
			t.Errorf("%s: expected an error for %q", name, id)
		}
	}
}

// Override order must be stable so identical snapshots produce identical calls.
func TestOverridesAreDeterministic(t *testing.T) {
	snapshot := &crafting.CompositeSnapshot{
		Workspace: "dev",
		Home:      "lab/h",
		Deps:      map[string]string{"zebra": "lab/z", "alpha": "lab/a", "middle": "lab/m"},
	}
	first := strings.Join(snapshot.Overrides(), ",")
	for range 20 {
		if got := strings.Join(snapshot.Overrides(), ","); got != first {
			t.Fatalf("override order varies: %q then %q", first, got)
		}
	}
}
