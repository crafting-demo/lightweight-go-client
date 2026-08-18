package crafting_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	crafting "github.com/crafting-demo/lightweight-go-client"
	"github.com/crafting-demo/lightweight-go-client/craftingtest"
)

func TestCreateSandboxPassesTemplateEnvAndWaitsForReadiness(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner)

	ref, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx-abc",
		Template: "agent-sandbox",
		Env:      []string{"TEMPORAL_TASK_QUEUE=queue-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.String() != "lab/sbx-abc" {
		t.Errorf("ref = %q, want lab/sbx-abc", ref.String())
	}

	create := runner.JoinedArgsFor("sandbox", "create")
	if create == "" {
		t.Fatal("no sandbox create call was made")
	}
	for _, want := range []string{
		"-E TEMPORAL_TASK_QUEUE=queue-1",
		"-t agent-sandbox",
		// Creation converges on one sandbox rather than duplicating on retry.
		"--if-exists skip",
		"--use-pool auto",
		"--folder lab",
		"--wait=false",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("create call missing %q\ngot: %s", want, create)
		}
	}

	// cs swallows wait errors so the caller can retry, so readiness is confirmed
	// separately or a caller would use a sandbox that is not up yet.
	if runner.ArgsFor("wait", "sandbox", "--expect", "ready") == nil {
		t.Error("readiness was not confirmed after creation")
	}
}

// Restoring must apply every component, or the fork comes up with a workspace
// and a dataset that never coexisted.
func TestCreateSandboxFromSnapshotAppliesEveryComponent(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner)

	snapshot := &crafting.CompositeSnapshot{
		Workspace: "dev",
		Home:      "lab/tsp-abc-home",
		Base:      "lab/tsp-abc-base",
		Deps:      map[string]string{"db": "lab/tsp-abc-dep-db", "cache": "lab/tsp-abc-dep-cache"},
	}
	if _, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx-fork",
		Template: "agent-sandbox",
		From:     snapshot,
	}); err != nil {
		t.Fatal(err)
	}

	create := runner.JoinedArgsFor("sandbox", "create")
	for _, want := range []string{
		"-D dev/home=lab/tsp-abc-home",
		"-D dev/base=lab/tsp-abc-base",
		"-D db/snapshot=lab/tsp-abc-dep-db",
		"-D cache/snapshot=lab/tsp-abc-dep-cache",
	} {
		if !strings.Contains(create, want) {
			t.Errorf("restore call missing %q\ngot: %s", want, create)
		}
	}
}

func TestCreateSandboxRejectsIncompleteRequests(t *testing.T) {
	client := testClient(t, craftingtest.NewRunner())
	ctx := context.Background()

	if _, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{Template: "t"}); err == nil {
		t.Error("a sandbox with no name should be rejected")
	}
	if _, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{Name: "sbx"}); err == nil {
		t.Error("a sandbox with no template should be rejected")
	}
	if _, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
		Name: "sbx", Template: "t", Env: []string{"NOT_A_PAIR"},
	}); err == nil {
		t.Error("an env entry that is not KEY=VALUE should be rejected")
	}
	if _, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
		Name: "sbx", Template: "t", Env: []string{"=VALUE"},
	}); err == nil {
		t.Error("an env entry with an empty key should be rejected")
	}
}

func TestCreateSandboxTreatsAlreadyExistsAsSuccess(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox create", &crafting.Result{Stderr: `Sandbox "sbx-abc" already exists.`, ExitCode: 1}, nil)
	client := testClient(t, runner)

	ref, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx-abc",
		Template: "agent-sandbox",
	})
	if err != nil {
		t.Fatalf("an existing sandbox should be treated as a successful skip, got %v", err)
	}
	if ref.Name != "sbx-abc" {
		t.Errorf("ref = %+v, want sbx-abc", ref)
	}
	if runner.ArgsFor("wait", "sandbox", "--expect", "ready") == nil {
		t.Error("readiness should still be confirmed after skip")
	}
}

// Teardown is retried, so a sandbox that is already gone is success. Each
// spelling of that condition counts as absent.
func TestDeleteSandboxTreatsMissingSandboxAsSuccess(t *testing.T) {
	for _, stderr := range []string{
		"Sandbox tsp-abc does not exist",
		"Sandbox not found: tsp-abc",
		"rpc error: code = NotFound desc = not_found: SANDBOX",
	} {
		runner := craftingtest.NewRunner().
			On("sandbox remove", &crafting.Result{Stderr: stderr, ExitCode: 1}, nil)
		client := testClient(t, runner)

		if err := client.DeleteSandbox(context.Background(), client.Ref("tsp-abc")); err != nil {
			t.Errorf("stderr %q should be treated as already removed, got %v", stderr, err)
		}
	}
}

func TestDeleteSandboxSurfacesRealFailures(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox remove", &crafting.Result{Stderr: "permission denied", ExitCode: 1}, nil)
	client := testClient(t, runner)

	if err := client.DeleteSandbox(context.Background(), client.Ref("tsp-abc")); err == nil {
		t.Error("a genuine removal failure should be reported")
	}
}

func TestSuspendAndResumeWaitForTheExpectedState(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnExec("", "", 0)
	client := testClient(t, runner)
	ref := client.Ref("tsp-abc")

	if err := client.SuspendSandbox(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if runner.ArgsFor("wait", "sandbox", "--expect", "suspended") == nil {
		t.Error("suspend did not wait for the suspended state")
	}

	if err := client.ResumeSandbox(context.Background(), ref, crafting.ResumeOptions{Workload: "dev"}); err != nil {
		t.Fatal(err)
	}
	if runner.ArgsFor("wait", "sandbox", "--expect", "ready") == nil {
		t.Error("resume did not wait for readiness")
	}
}

func TestSuspendLeavesAnAlreadySuspendedSandboxAlone(t *testing.T) {
	runner := craftingtest.NewRunner().On("sandbox show", craftingtest.SuspendedState(), nil)
	client := testClient(t, runner)

	if err := client.SuspendSandbox(context.Background(), client.Ref("tsp-abc")); err != nil {
		t.Fatal(err)
	}
	if runner.CountMatching("sandbox suspend") != 0 {
		t.Error("an already-suspended sandbox should not be suspended again")
	}
}

// Resume must return a sandbox that can actually run commands. The control plane
// reports readiness slightly before the workload accepts connections, so callers
// that trusted readiness alone saw their first command fail to dial.
func TestResumeWaitsUntilTheWorkloadAcceptsCommands(t *testing.T) {
	attempts := 0
	// The probe fails twice, as an unreachable workload does, then succeeds.
	runner := craftingtest.NewRunner().OnCommandFunc("exec", func() (*crafting.Result, error) {
		attempts++
		if attempts < 3 {
			return &crafting.Result{Stderr: "Failed to connect to workload: dev", ExitCode: 1}, nil
		}
		return &crafting.Result{Stdout: crafting.ExitSentinel(0)}, nil
	})
	client := testClient(t, runner)

	if err := client.ResumeSandbox(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{
		Workload: "dev",
	}); err != nil {
		t.Fatalf("resume should wait for reachability, got %v", err)
	}
	if attempts != 3 {
		t.Errorf("probe ran %d times, want 3", attempts)
	}
}

func TestResumeFailsWhenWorkloadNeverBecomesReachable(t *testing.T) {
	runner := craftingtest.NewRunner().
		OnCommand("exec", &crafting.Result{Stderr: "Failed to connect to workload: dev", ExitCode: 1}, nil)
	client := testClient(t, runner)

	if err := client.ResumeSandbox(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{
		Workload: "dev",
	}); err == nil {
		t.Error("resume should fail when the workload stays unreachable")
	}
}

func TestResumeDoesNotRetryWhenTheSandboxIsGone(t *testing.T) {
	attempts := 0
	runner := craftingtest.NewRunner().OnCommandFunc("exec", func() (*crafting.Result, error) {
		attempts++
		return &crafting.Result{Stderr: "Sandbox tsp-abc does not exist", ExitCode: 1}, nil
	})
	client := testClient(t, runner)

	err := client.ResumeSandbox(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{Workload: "dev"})
	if err == nil {
		t.Fatal("resume should fail when the sandbox is gone")
	}
	if !errors.Is(err, crafting.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if attempts != 1 {
		t.Errorf("probe ran %d times, want 1", attempts)
	}
}

// Without a workload there is nothing to probe, so resume settles for readiness
// rather than failing.
func TestResumeWithoutWorkloadSkipsTheReachabilityProbe(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner)

	if err := client.ResumeSandbox(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{}); err != nil {
		t.Fatal(err)
	}
	if runner.CountMatching("exec") != 0 {
		t.Error("no probe should run when no workload was named")
	}
}

func TestEnsureResumedOnlyResumesSuspendedSandboxes(t *testing.T) {
	running := craftingtest.NewRunner().On("sandbox show", craftingtest.RunningState(), nil)
	client := testClient(t, running)
	if err := client.EnsureResumed(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{}); err != nil {
		t.Fatal(err)
	}
	if running.CountMatching("sandbox resume") != 0 {
		t.Error("a running sandbox should not be resumed")
	}

	suspended := craftingtest.NewRunner().On("sandbox show", craftingtest.SuspendedState(), nil)
	client = testClient(t, suspended)
	if err := client.EnsureResumed(context.Background(), client.Ref("tsp-abc"), crafting.ResumeOptions{}); err != nil {
		t.Fatal(err)
	}
	if suspended.CountMatching("sandbox resume") != 1 {
		t.Error("a suspended sandbox should be resumed")
	}
}

// A command that runs and fails is a result the caller can act on, not an
// infrastructure error.
func TestExecReportsExitCodeAsResult(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.RunningState(), nil).
		OnExec("build output", "warning: deprecated", 2)
	client := testClient(t, runner)

	res, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev"}, "make build")
	if err != nil {
		t.Fatalf("a failing command should not be an error: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("exit code = %d, want 2", res.ExitCode)
	}
	if res.Stdout != "build output" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "build output")
	}
	if res.Stderr != "warning: deprecated" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "warning: deprecated")
	}
}

// Without the sentinel the client cannot know whether the command ran, so it
// must report an error and let the caller decide whether to retry.
func TestExecTreatsMissingSentinelAsError(t *testing.T) {
	runner := craftingtest.NewRunner().
		OnCommand("exec", &crafting.Result{Stderr: "Failed to connect to workload: dev", ExitCode: 1}, nil)
	client := testClient(t, runner)

	if _, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev"}, "ls"); err == nil {
		t.Fatal("expected an error when the command never completed")
	}
}

func TestExecReportsMissingSandboxAsErrNotFound(t *testing.T) {
	runner := craftingtest.NewRunner().
		OnCommand("exec", &crafting.Result{Stderr: "Sandbox tsp-abc does not exist", ExitCode: 1}, nil)
	client := testClient(t, runner)

	_, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev"}, "ls")
	if !errors.Is(err, crafting.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestExecDisablesTTYAndRunsAsTheWorkspaceUser(t *testing.T) {
	runner := craftingtest.NewRunner().OnExec("ok", "", 0)
	client := testClient(t, runner)

	if _, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev"}, "ls"); err != nil {
		t.Fatal(err)
	}
	exec := runner.JoinedArgsFor("exec")
	// -T keeps stdout and stderr separate; the uid keeps writes inside the home
	// directory that home snapshots capture.
	for _, want := range []string{"-T", "-u 1000", "-W tsp-abc/dev"} {
		if !strings.Contains(exec, want) {
			t.Errorf("exec call missing %q\ngot: %s", want, exec)
		}
	}
	// The global flags have to precede the separator or cs reads them as part of
	// the remote command.
	if strings.Index(exec, "--folder lab") > strings.Index(exec, " -- ") {
		t.Errorf("global flags must come before the -- separator\ngot: %s", exec)
	}
}

func TestExecOverridesTheUserWhenAsked(t *testing.T) {
	runner := craftingtest.NewRunner().OnExec("ok", "", 0)
	client := testClient(t, runner)

	if _, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev", UID: crafting.UID(0), Dir: "/srv"}, "ls"); err != nil {
		t.Fatal(err)
	}
	exec := runner.JoinedArgsFor("exec")
	for _, want := range []string{"-u 0", "-w /srv"} {
		if !strings.Contains(exec, want) {
			t.Errorf("exec call missing %q\ngot: %s", want, exec)
		}
	}
}

// Resuming before the command keeps resume progress out of the command's stderr.
func TestExecAutoResumesSuspendedSandboxFirst(t *testing.T) {
	runner := craftingtest.NewRunner().
		On("sandbox show", craftingtest.SuspendedState(), nil).
		OnExec("done", "", 0)
	client := testClient(t, runner)

	if _, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev", AutoResume: true}, "ls"); err != nil {
		t.Fatal(err)
	}
	if runner.ArgsFor("sandbox", "resume") == nil {
		t.Error("a suspended sandbox should be resumed before the command runs")
	}
}

func TestExecWithoutAutoResumeDoesNotQueryState(t *testing.T) {
	runner := craftingtest.NewRunner().OnExec("done", "", 0)
	client := testClient(t, runner)

	if _, err := client.Exec(context.Background(), client.Ref("tsp-abc"),
		crafting.ExecOptions{Workload: "dev"}, "ls"); err != nil {
		t.Fatal(err)
	}
	if runner.CountMatching("sandbox show") != 0 {
		t.Error("state should not be queried when auto-resume is off")
	}
}

func TestExecRejectsIncompleteRequests(t *testing.T) {
	client := testClient(t, craftingtest.NewRunner())
	ctx := context.Background()
	ref := client.Ref("tsp-abc")

	if _, err := client.Exec(ctx, ref, crafting.ExecOptions{}, "ls"); err == nil {
		t.Error("a command with no workload should be rejected")
	}
	if _, err := client.Exec(ctx, ref, crafting.ExecOptions{Workload: "dev"}, ""); err == nil {
		t.Error("an empty command should be rejected")
	}
}

func TestOperationsRejectAnEmptyRef(t *testing.T) {
	client := testClient(t, craftingtest.NewRunner())
	ctx := context.Background()

	if err := client.DeleteSandbox(ctx, crafting.SandboxRef{}); err == nil {
		t.Error("deleting an unnamed sandbox should fail")
	}
	if err := client.SuspendSandbox(ctx, crafting.SandboxRef{Folder: "lab"}); err == nil {
		t.Error("suspending an unnamed sandbox should fail")
	}
}
