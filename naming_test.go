package crafting

import (
	"strings"
	"testing"
)

// Deterministic names are what make provisioning idempotent, so the same seed
// must always resolve to the same sandbox name.
func TestDeriveSandboxNameIsDeterministic(t *testing.T) {
	const seed = "sandbox-c2f9e1d0-3b4a-4c5d-8e6f-7a8b9c0d1e2f"
	first, err := DeriveSandboxName("tsp", seed)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeriveSandboxName("tsp", seed)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("same seed produced %q then %q", first, second)
	}
	other, err := DeriveSandboxName("tsp", seed+"|snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("different seeds produced the same name")
	}
}

// Crafting rejects names longer than 20 characters or not matching its pattern,
// and callers seed these with long identifiers such as UUIDs.
func TestDeriveSandboxNameObeysPlatformLimits(t *testing.T) {
	seeds := []string{
		"sandbox-c2f9e1d0-3b4a-4c5d-8e6f-7a8b9c0d1e2f",
		"sandbox-0000000000000000",
		"",
		strings.Repeat("x", 500),
		"sandbox-UPPER-CASE-ID",
	}
	for _, prefix := range []string{"tsp", "a", "craftingxyz", ""} {
		for _, seed := range seeds {
			name, err := DeriveSandboxName(prefix, seed)
			if err != nil {
				t.Fatalf("prefix %q seed %q: %v", prefix, seed, err)
			}
			if len(name) > maxSandboxNameLen {
				t.Errorf("name %q is %d chars, limit is %d", name, len(name), maxSandboxNameLen)
			}
			if !sandboxNameRegexp.MatchString(name) {
				t.Errorf("name %q does not match the platform pattern", name)
			}
		}
	}
}

func TestDeriveSandboxNameRejectsUnusablePrefix(t *testing.T) {
	for _, prefix := range []string{"9lead", "has-dash", "UPPER", "waytoolongprefixhere"} {
		if _, err := DeriveSandboxName(prefix, "seed"); err == nil {
			t.Errorf("prefix %q was accepted but should not be", prefix)
		}
	}
}

func TestSnapshotComponentNames(t *testing.T) {
	sandbox, err := DeriveSandboxName("tsp", "sandbox-abc")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"home", "base", "dep-db", "dep-My_Cache 1"} {
		name, err := snapshotComponentName(sandbox, "a1b2c3d4", kind)
		if err != nil {
			t.Fatalf("kind %q: %v", kind, err)
		}
		if !scopedNameRegexp.MatchString(name) {
			t.Errorf("snapshot name %q does not match the platform pattern", name)
		}
		if len(name) > maxSnapshotNameLen {
			t.Errorf("snapshot name %q exceeds %d chars", name, maxSnapshotNameLen)
		}
	}
}

// Repeated snapshots of one sandbox must not overwrite each other, since earlier
// snapshots may still be referenced by running forks.
func TestSnapshotComponentNamesAreUniquePerCall(t *testing.T) {
	first, err := snapshotComponentName("tsp-abcdef", newUnique(), "home")
	if err != nil {
		t.Fatal(err)
	}
	second, err := snapshotComponentName("tsp-abcdef", newUnique(), "home")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("two snapshot calls produced the same name %q", first)
	}
}

func TestQualifyAndSplit(t *testing.T) {
	if got := Qualify("lab", "snap"); got != "lab/snap" {
		t.Errorf("Qualify = %q, want lab/snap", got)
	}
	if got := Qualify("lab", "work/snap"); got != "work/snap" {
		t.Errorf("Qualify should not double-qualify, got %q", got)
	}
	if got := Qualify("", "snap"); got != "snap" {
		t.Errorf("Qualify with no folder = %q, want snap", got)
	}

	folder, name := SplitQualified("lab", "work/sbx")
	if folder != "work" || name != "sbx" {
		t.Errorf("SplitQualified = %q, %q, want work, sbx", folder, name)
	}
	folder, name = SplitQualified("lab", "sbx")
	if folder != "lab" || name != "sbx" {
		t.Errorf("SplitQualified fallback = %q, %q, want lab, sbx", folder, name)
	}
}
