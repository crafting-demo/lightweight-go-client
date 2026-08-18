package crafting

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// DefaultNamePrefix is used by DeriveSandboxName when no prefix is given.
const DefaultNamePrefix = "sbx"

// Crafting name rules, mirrored here so callers fail fast with a message that
// names the offending value.
//
// Sandbox names are limited to 20 characters and must start with a letter, which
// rules out using most external identifiers directly.
const (
	maxSandboxNameLen  = 20
	maxSnapshotNameLen = 128
	minHashLen         = 8
)

var (
	sandboxNameRegexp = regexp.MustCompile(`^[a-z]+[a-z0-9-]*[a-z0-9]+$`)
	scopedNameRegexp  = regexp.MustCompile(`^([a-z0-9_]|([a-z0-9_]+[a-z0-9-_]*[a-z0-9_]+))$`)
	prefixRegexp      = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
)

// DeriveSandboxName turns an arbitrary identifier into a valid Crafting sandbox
// name by hashing it under a caller-chosen prefix.
//
// The mapping is deterministic, which is what makes provisioning idempotent: the
// same seed always yields the same name, so a retried create resolves to the
// existing sandbox instead of a duplicate.
func DeriveSandboxName(prefix, seed string) (string, error) {
	if prefix == "" {
		prefix = DefaultNamePrefix
	}
	if !prefixRegexp.MatchString(prefix) {
		return "", fmt.Errorf("crafting: name prefix %q must start with a letter and contain only lowercase letters and digits", prefix)
	}
	budget := maxSandboxNameLen - len(prefix) - 1
	if budget < minHashLen {
		return "", fmt.Errorf("crafting: name prefix %q leaves only %d characters for uniqueness; use at most %d characters",
			prefix, budget, maxSandboxNameLen-minHashLen-1)
	}

	sum := sha256.Sum256([]byte(seed))
	name := prefix + "-" + hex.EncodeToString(sum[:])[:budget]
	if !sandboxNameRegexp.MatchString(name) {
		return "", fmt.Errorf("crafting: derived sandbox name %q is invalid", name)
	}
	return name, nil
}

// snapshotComponentName names one component of a composite snapshot. Snapshot
// names allow 128 characters, so these stay readable: the sandbox they came
// from, a token unique to this snapshot call, and the component kind.
//
// The unique token matters because a sandbox is often snapshotted repeatedly.
// Reusing a name would overwrite a snapshot that other callers may still be
// forking from.
func snapshotComponentName(sandbox, unique, kind string) (string, error) {
	name := fmt.Sprintf("%s-%s-%s", sandbox, unique, sanitizeComponent(kind))
	if len(name) > maxSnapshotNameLen {
		return "", fmt.Errorf("crafting: derived snapshot name %q exceeds %d characters", name, maxSnapshotNameLen)
	}
	if !scopedNameRegexp.MatchString(name) {
		return "", fmt.Errorf("crafting: derived snapshot name %q is invalid", name)
	}
	return name, nil
}

// sanitizeComponent reduces a caller-supplied identifier, such as a dependency
// workload name, to characters valid in a snapshot name.
func sanitizeComponent(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(collapseDashes(b.String()), "-")
	if out == "" {
		return "x"
	}
	return out
}

func collapseDashes(s string) string {
	var b strings.Builder
	var prevDash bool
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}

// newUnique returns a short random token for snapshot names.
func newUnique() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failed CSPRNG read must not silently produce colliding names.
		panic("crafting: reading random bytes: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Qualify returns a folder-qualified object reference. Snapshot references in
// sandbox creation overrides are resolved against the org root rather than the
// folder flag, so they must always carry their folder.
func Qualify(folder, name string) string {
	if folder == "" || strings.Contains(name, "/") {
		return name
	}
	return folder + "/" + name
}

// SplitQualified splits a possibly folder-qualified name, falling back to the
// supplied default folder.
func SplitQualified(defaultFolder, qualified string) (folder, name string) {
	if i := strings.LastIndex(qualified, "/"); i >= 0 {
		return qualified[:i], qualified[i+1:]
	}
	return defaultFolder, qualified
}
