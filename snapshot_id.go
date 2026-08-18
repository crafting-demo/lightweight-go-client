package crafting

import (
	"encoding/json"
	"fmt"
	"slices"
)

// CompositeSnapshot is a checkpoint of a whole sandbox, packed into a single
// opaque string.
//
// A faithful copy of a Crafting sandbox is not one artifact. The working files
// live in the workspace home, the data lives in dependency workloads such as
// databases, and image mutations live in the workspace root filesystem.
// Restoring only one of those would produce a sandbox with a workspace or a
// dataset it never had, so all of them travel together.
//
// Callers that need to persist a snapshot in a system that only understands
// strings, such as a workflow history, should use Encode and
// DecodeCompositeSnapshot rather than storing the fields individually.
type CompositeSnapshot struct {
	Version int `json:"v"`
	// Workspace records which workload the home and base components came from,
	// so a restore can rebuild the correct override rules.
	Workspace string `json:"ws"`
	// Home, Base and Deps hold folder-qualified snapshot names.
	Home string            `json:"home,omitempty"`
	Base string            `json:"base,omitempty"`
	Deps map[string]string `json:"deps,omitempty"`
}

// CompositeSnapshotVersion is the encoding version this package writes. Decoding
// rejects anything else rather than guessing at an unfamiliar layout.
const CompositeSnapshotVersion = 1

// Encode renders the snapshot as a single opaque string. The receiver is not
// modified.
func (c *CompositeSnapshot) Encode() (string, error) {
	encoded := *c
	encoded.Version = CompositeSnapshotVersion
	b, err := json.Marshal(&encoded)
	if err != nil {
		return "", fmt.Errorf("crafting: encoding composite snapshot: %w", err)
	}
	return string(b), nil
}

// DecodeCompositeSnapshot parses a string produced by Encode.
func DecodeCompositeSnapshot(id string) (*CompositeSnapshot, error) {
	if id == "" {
		return nil, fmt.Errorf("crafting: empty snapshot id")
	}
	var c CompositeSnapshot
	if err := json.Unmarshal([]byte(id), &c); err != nil {
		return nil, fmt.Errorf("crafting: snapshot id %q is not a Crafting composite snapshot: %w", id, err)
	}
	if c.Version != CompositeSnapshotVersion {
		return nil, fmt.Errorf("crafting: unsupported composite snapshot version %d", c.Version)
	}
	if c.Home == "" && c.Base == "" && len(c.Deps) == 0 {
		return nil, fmt.Errorf("crafting: composite snapshot %q has no components", id)
	}
	return &c, nil
}

// Components lists every component snapshot as a folder-qualified name.
// Dependency order is sorted so output is deterministic.
func (c *CompositeSnapshot) Components() []string {
	out := make([]string, 0, len(c.Deps)+2)
	if c.Home != "" {
		out = append(out, c.Home)
	}
	if c.Base != "" {
		out = append(out, c.Base)
	}
	for _, workload := range c.sortedDeps() {
		out = append(out, c.Deps[workload])
	}
	return out
}

// Overrides renders the composite as cs sandbox create override rules.
// Workspaces take home= and base=; dependencies take snapshot=.
func (c *CompositeSnapshot) Overrides() []string {
	var out []string
	if c.Home != "" {
		out = append(out, fmt.Sprintf("%s/home=%s", c.Workspace, c.Home))
	}
	if c.Base != "" {
		out = append(out, fmt.Sprintf("%s/base=%s", c.Workspace, c.Base))
	}
	for _, workload := range c.sortedDeps() {
		out = append(out, fmt.Sprintf("%s/snapshot=%s", workload, c.Deps[workload]))
	}
	return out
}

func (c *CompositeSnapshot) sortedDeps() []string {
	names := make([]string, 0, len(c.Deps))
	for k := range c.Deps {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
