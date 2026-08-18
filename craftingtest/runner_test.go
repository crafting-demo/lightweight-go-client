package craftingtest

import (
	"context"
	"testing"

	crafting "github.com/crafting-demo/lightweight-go-client"
)

func TestOnExecDoesNotMatchANameThatContainsExec(t *testing.T) {
	runner := NewRunner().OnExec("from-exec", "", 0)

	res, err := runner.Run(context.Background(), nil, "snapshot", "create", "sbx-exec-home")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "" {
		t.Errorf("snapshot create was treated as exec: %q", res.Stdout)
	}

	res, err = runner.Run(context.Background(), nil, "exec", "-T", "-W", "sbx/dev")
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "from-exec"+crafting.ExitSentinel(0) {
		t.Errorf("exec call was not scripted, got %q", res.Stdout)
	}
}

func TestReturnedResultsAreIsolatedCopies(t *testing.T) {
	scripted := &crafting.Result{Stdout: "original"}
	runner := NewRunner().On("sandbox show", scripted, nil)

	first, _ := runner.Run(context.Background(), nil, "sandbox", "show", "sbx")
	first.Stdout = "mutated"

	second, _ := runner.Run(context.Background(), nil, "sandbox", "show", "sbx")
	if second.Stdout != "original" {
		t.Errorf("mutating a returned result leaked into later calls: %q", second.Stdout)
	}
	if scripted.Stdout != "original" {
		t.Errorf("mutating a returned result mutated the scripted value: %q", scripted.Stdout)
	}
}
