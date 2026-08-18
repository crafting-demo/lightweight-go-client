package crafting_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	crafting "github.com/crafting-demo/lightweight-go-client"
	"github.com/crafting-demo/lightweight-go-client/craftingtest"
)

// testClient builds a client backed by a fake CLI. The probe interval is
// squeezed so reachability retries do not slow the suite down.
func testClient(t *testing.T, runner crafting.Runner, configure ...func(*crafting.Options)) *crafting.Client {
	t.Helper()
	opts := crafting.Options{
		Folder:        "lab",
		Runner:        runner,
		ProbeInterval: time.Millisecond,
	}
	for _, fn := range configure {
		fn(&opts)
	}
	client, err := crafting.NewClient(opts)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return client
}

func TestRefResolvesAgainstTheClientFolder(t *testing.T) {
	client := testClient(t, craftingtest.NewRunner())

	if got := client.Ref("sbx"); got.Folder != "lab" || got.Name != "sbx" {
		t.Errorf("Ref = %+v, want {lab sbx}", got)
	}
	if got := client.Ref("work/sbx"); got.Folder != "work" || got.Name != "sbx" {
		t.Errorf("Ref should honour an explicit folder, got %+v", got)
	}
	if got := client.Ref("work/sbx").String(); got != "work/sbx" {
		t.Errorf("String = %q, want work/sbx", got)
	}
}

// The client must not disturb another cs session on the same host.
func TestConfigDirIsPassedThroughEnvironment(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner, func(o *crafting.Options) { o.ConfigDir = "/tmp/worker-cs" })

	if _, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx",
		Template: "agent-sandbox",
	}); err != nil {
		t.Fatal(err)
	}

	calls := runner.Calls()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}
	if !slices.Contains(calls[0].Env, "SANDBOX_CONFIG_DIR=/tmp/worker-cs") {
		t.Errorf("env = %v, want SANDBOX_CONFIG_DIR to be set", calls[0].Env)
	}
}

func TestOrgAndFolderAreOnEveryCall(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner, func(o *crafting.Options) { o.Org = "eng" })

	if _, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx",
		Template: "agent-sandbox",
	}); err != nil {
		t.Fatal(err)
	}
	create := runner.JoinedArgsFor("sandbox", "create")
	for _, want := range []string{"-O eng", "--folder lab"} {
		if !strings.Contains(create, want) {
			t.Errorf("create call missing %q\ngot: %s", want, create)
		}
	}
}

// A token means the worker is running outside Crafting, and one login serves
// every subsequent command.
func TestTokenAuthenticatesOnceAcrossCalls(t *testing.T) {
	runner := craftingtest.NewRunner().On("sandbox show", craftingtest.RunningState(), nil)
	client := testClient(t, runner, func(o *crafting.Options) { o.Token = "sa-token" })
	ctx := context.Background()

	ref, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{Name: "sbx", Template: "agent-sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SuspendSandbox(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if got := runner.CountMatching("login"); got != 1 {
		t.Errorf("logged in %d times, want exactly 1", got)
	}
	login := runner.ArgsFor("login")
	if login == nil {
		t.Fatal("no login call was made")
	}
	if slices.Contains(login, "sa-token") || slices.Contains(login, "-t") {
		t.Errorf("token must not appear on the command line, got %v", login)
	}
	if !slices.Contains(runner.Calls()[0].Env, "CRAFTING_SANDBOX_AUTH_TOKEN=sa-token") {
		t.Errorf("env = %v, want the token in CRAFTING_SANDBOX_AUTH_TOKEN", runner.Calls()[0].Env)
	}
}

func TestFailedAuthIsRetriedOnTheNextCall(t *testing.T) {
	attempts := 0
	runner := craftingtest.NewRunner().OnCommandFunc("login", func() (*crafting.Result, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("network blip")
		}
		return &crafting.Result{}, nil
	})
	client := testClient(t, runner, func(o *crafting.Options) { o.Token = "sa-token" })
	ctx := context.Background()
	opts := crafting.CreateSandboxOptions{Name: "sbx", Template: "agent-sandbox"}

	if _, err := client.CreateSandbox(ctx, opts); err == nil {
		t.Fatal("first call should fail when login fails")
	}
	if _, err := client.CreateSandbox(ctx, opts); err != nil {
		t.Fatalf("second call should retry login, got %v", err)
	}
	if attempts != 2 {
		t.Errorf("login ran %d times, want 2", attempts)
	}
}

func TestNoTokenMeansNoLogin(t *testing.T) {
	runner := craftingtest.NewRunner()
	client := testClient(t, runner)

	if _, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx",
		Template: "agent-sandbox",
	}); err != nil {
		t.Fatal(err)
	}
	if got := runner.CountMatching("login"); got != 0 {
		t.Errorf("logged in %d times without a token, want 0", got)
	}
}

// A CLI that could not be run at all is a different failure from a CLI that ran
// and rejected the request, and callers distinguish them.
func TestRunnerErrorsAreWrappedNotSwallowed(t *testing.T) {
	sentinel := errors.New("cs binary not found")
	runner := craftingtest.NewRunner().On("sandbox create", nil, sentinel)
	client := testClient(t, runner)

	_, err := client.CreateSandbox(context.Background(), crafting.CreateSandboxOptions{
		Name:     "sbx",
		Template: "agent-sandbox",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestNewClientRejectsNegativeTuning(t *testing.T) {
	for name, opts := range map[string]crafting.Options{
		"lifecycle timeout": {LifecycleTimeout: -time.Second},
		"probe attempts":    {ProbeAttempts: -1},
		"probe interval":    {ProbeInterval: -time.Second},
	} {
		if _, err := crafting.NewClient(opts); err == nil {
			t.Errorf("%s: negative value was accepted", name)
		}
	}
}

func TestIsNotFoundRecognisesEveryPhrasing(t *testing.T) {
	for _, stderr := range []string{
		"Sandbox tsp-abc does not exist",
		"Sandbox not found: tsp-abc",
		"rpc error: code = NotFound desc = not_found: SANDBOX",
		`No matching found for "lgcit-abc-home"`,
	} {
		if !crafting.IsNotFound(&crafting.Result{Stderr: stderr}) {
			t.Errorf("stderr %q should read as absent", stderr)
		}
	}
	if crafting.IsNotFound(&crafting.Result{Stderr: "permission denied"}) {
		t.Error("a permission failure should not read as absent")
	}
	if crafting.IsNotFound(&crafting.Result{Stderr: "config file not found"}) {
		t.Error("an unrelated 'not found' should not read as an absent sandbox")
	}
	if crafting.IsNotFound(nil) {
		t.Error("a nil result should not read as absent")
	}
}
