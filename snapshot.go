package crafting

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// DefaultHomeIncludes captures the whole home directory. Work lands in
// arbitrary places, so a narrow list risks a snapshot that has lost it. Callers
// who know their layout can narrow this.
var DefaultHomeIncludes = []string{"."}

// DefaultHomeExcludes mirrors the platform's own home-snapshot excludes plus
// caches, which are large and never worth carrying into a fork.
var DefaultHomeExcludes = []string{
	".config/crafting/sandbox",
	".ssh",
	".bash_history",
	".sudo_as_admin_successful",
	".cache",
}

// SnapshotOptions selects what a snapshot captures.
type SnapshotOptions struct {
	// Workspace is the workspace workload whose home directory is captured.
	Workspace string
	// Dependencies are dependency workload names whose data is captured. This
	// is what lets a fork come up against the same database contents.
	Dependencies []string
	// IncludeBase additionally captures the workspace root filesystem. It is
	// off by default because it is the slowest and largest component, and most
	// work does not mutate the image.
	IncludeBase bool

	// HomeIncludes and HomeExcludes select what the home component captures.
	// Empty selects DefaultHomeIncludes and DefaultHomeExcludes.
	HomeIncludes []string
	HomeExcludes []string

	// Folder is where the component snapshots are created. Empty uses the
	// client's folder.
	Folder string
	// UID is the user the home-snapshot preparation command runs as. Nil
	// selects DefaultExecUID.
	UID *int
	// Timeout bounds the whole capture. Zero means the caller's context
	// governs.
	Timeout time.Duration
}

// SnapshotSandbox captures the sandbox as a set of components and returns them
// as one composite snapshot.
//
// The sandbox keeps running, so the caller gets a checkpoint without losing the
// environment it just built. Restoring the result through CreateSandbox with
// CreateSandboxOptions.From produces an independent sandbox, which is what lets
// a caller explore several candidate changes from the same starting state.
//
// A partially built composite is worse than none, since it consumes storage and
// can never be restored, so any components already created are removed if a
// later one fails.
func (c *Client) SnapshotSandbox(ctx context.Context, ref SandboxRef, opts SnapshotOptions) (*CompositeSnapshot, error) {
	if err := c.validate(ref); err != nil {
		return nil, err
	}
	if opts.Workspace == "" {
		return nil, fmt.Errorf("crafting: workspace is required to snapshot a sandbox")
	}
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	// A suspended sandbox has no running workloads to snapshot.
	if err := c.EnsureResumed(ctx, ref, ResumeOptions{Workload: opts.Workspace, UID: opts.UID}); err != nil {
		return nil, err
	}

	snapFolder := opts.Folder
	if snapFolder == "" {
		snapFolder = c.folder
	}
	unique := newUnique()
	composite := &CompositeSnapshot{Workspace: opts.Workspace}

	// Components are tracked as they are created so a partial failure can be
	// rolled back instead of leaking storage.
	var created []string
	fail := func(err error) (*CompositeSnapshot, error) {
		// The original context is often already cancelled or timed out, which
		// is why we are here. Cleanup must not inherit that deadline or the
		// components we meant to roll back leak.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.lifecycleTimeout)
		defer cancel()
		if cleanupErrs := c.deleteComponents(cleanupCtx, snapFolder, created); len(cleanupErrs) > 0 {
			err = errors.Join(append([]error{err}, cleanupErrs...)...)
		}
		return nil, err
	}

	if err := c.prepareHomeSnapshot(ctx, ref, opts); err != nil {
		return fail(err)
	}

	homeName, err := snapshotComponentName(ref.Name, unique, "home")
	if err != nil {
		return fail(err)
	}
	if err := c.createSnapshot(ctx, snapFolder, ref, opts.Workspace, homeName, true); err != nil {
		return fail(err)
	}
	composite.Home = Qualify(snapFolder, homeName)
	created = append(created, composite.Home)

	if opts.IncludeBase {
		baseName, err := snapshotComponentName(ref.Name, unique, "base")
		if err != nil {
			return fail(err)
		}
		if err := c.createSnapshot(ctx, snapFolder, ref, opts.Workspace, baseName, false); err != nil {
			return fail(err)
		}
		composite.Base = Qualify(snapFolder, baseName)
		created = append(created, composite.Base)
	}

	for _, dep := range opts.Dependencies {
		depName, err := snapshotComponentName(ref.Name, unique, "dep-"+dep)
		if err != nil {
			return fail(err)
		}
		if err := c.createSnapshot(ctx, snapFolder, ref, dep, depName, false); err != nil {
			return fail(err)
		}
		if composite.Deps == nil {
			composite.Deps = map[string]string{}
		}
		composite.Deps[dep] = Qualify(snapFolder, depName)
		created = append(created, composite.Deps[dep])
	}
	return composite, nil
}

// DeleteSnapshot removes every component of a composite snapshot. Components
// that are already gone are ignored so repeated cleanup succeeds, and deletion
// continues past individual failures so one stuck component cannot strand the
// rest.
func (c *Client) DeleteSnapshot(ctx context.Context, snapshot *CompositeSnapshot) error {
	if snapshot == nil {
		return fmt.Errorf("crafting: nil snapshot")
	}
	if err := c.ensureAuth(ctx); err != nil {
		return err
	}
	if errs := c.deleteComponents(ctx, c.folder, snapshot.Components()); len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (c *Client) deleteComponents(ctx context.Context, defaultFolder string, components []string) []error {
	var errs []error
	for _, qualified := range components {
		folder, name := SplitQualified(defaultFolder, qualified)
		res, err := c.run(ctx, folder, "removing snapshot "+qualified, "snapshot", "remove", name, "-f")
		if err != nil && !IsNotFound(res) && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errs
}

// createSnapshot captures one workload. home selects a home snapshot of a
// workspace; otherwise the snapshot is of the workload itself, which is the
// workspace root filesystem for a workspace and the stored data for a
// dependency.
func (c *Client) createSnapshot(ctx context.Context, snapFolder string, ref SandboxRef, workload, snapName string, home bool) error {
	// The snapshot is created in the snapshot folder, while -W resolves against
	// the folder flag. When the two differ the sandbox must be fully qualified.
	target := ref.Name + "/" + workload
	if snapFolder != ref.Folder {
		target = ref.String() + "/" + workload
	}
	args := []string{
		"snapshot", "create", snapName,
		"-W", target,
		// Snapshotting must not rewrite the origin sandbox's own definition;
		// the caller asked for a checkpoint, not a change to their environment.
		"--auto-update-definition=false",
		"-f",
	}
	if home {
		args = append(args, "--home")
	}
	what := fmt.Sprintf("creating snapshot %s of %s/%s", snapName, ref.Name, workload)
	res, err := c.run(ctx, snapFolder, what, args...)
	if err != nil {
		return err
	}
	// cs snapshot create --home exits successfully without creating anything
	// when it generates include lists instead of capturing. That must not be
	// mistaken for a snapshot we can restore.
	if strings.Contains(res.Stdout+res.Stderr, "Please go to your workspace") {
		return fmt.Errorf("crafting: home snapshot %s was not created; snapshot lists were missing or empty", snapName)
	}
	_, err = c.run(ctx, snapFolder, "verifying snapshot "+snapName, "snapshot", "show", snapName)
	return err
}

// prepareHomeSnapshot writes the include and exclude lists a home snapshot
// requires. Supplying them explicitly keeps capture non-interactive and makes
// the captured set deliberate.
func (c *Client) prepareHomeSnapshot(ctx context.Context, ref SandboxRef, opts SnapshotOptions) error {
	includes := opts.HomeIncludes
	if len(includes) == 0 {
		includes = DefaultHomeIncludes
	}
	excludes := opts.HomeExcludes
	if len(excludes) == 0 {
		excludes = DefaultHomeExcludes
	}

	script := writeFileScript(path.Join(homeSnapshotDir, "includes.txt"), strings.Join(includes, "\n")) +
		writeFileScript(path.Join(homeSnapshotDir, "excludes.txt"), strings.Join(excludes, "\n"))

	res, err := c.exec(ctx, ref, ExecOptions{Workload: opts.Workspace, UID: opts.UID}, script)
	if err != nil {
		return fmt.Errorf("crafting: preparing home snapshot lists: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("crafting: preparing home snapshot lists failed with exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// homeSnapshotDir is the path the platform reads when creating a home snapshot
// (the workspace owner's home, uid 1000). $HOME of a different user would write
// lists the snapshot command never sees, and create would silently skip capture.
const homeSnapshotDir = "/home/owner/.snapshot"
