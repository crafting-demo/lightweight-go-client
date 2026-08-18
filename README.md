# Crafting Go client

A lightweight Go client for driving [Crafting](https://crafting.dev) sandboxes:
create, execute, suspend, resume, snapshot, fork and delete.

A Crafting sandbox is a production-like environment — a workspace plus the
dependency workloads it talks to, such as Postgres, MySQL, Kafka or Redis. This
package exposes that lifecycle to Go programs by driving the public `cs` CLI.

```bash
go get github.com/crafting-demo/lightweight-go-client
```

## Usage

```go
import crafting "github.com/crafting-demo/lightweight-go-client"

client, err := crafting.NewClient(crafting.Options{Folder: "lab"})
if err != nil {
    return err
}

ref, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
    Name:     "my-sandbox",
    Template: "agent-sandbox",
    Env:      []string{"FEATURE_FLAG=on"},
})
if err != nil {
    return err
}
defer client.DeleteSandbox(ctx, ref)

res, err := client.Exec(ctx, ref, crafting.ExecOptions{Workload: "dev"}, "go test ./...")
if err != nil {
    return err // the command could not be run
}
fmt.Println(res.ExitCode, res.Stdout, res.Stderr)
```

A command that runs and fails is a result, not an error: it comes back with a
non-zero `ExitCode`. An error means the command never ran. A missing sandbox or
workload is `ErrNotFound` and is not worth retrying; other errors typically are.

## Composite snapshots

Most sandbox tooling snapshots a filesystem. A Crafting sandbox is a workspace
*and* its dependencies, so a snapshot here is composite: the workspace home
directory, the data inside each dependency, and optionally the workspace root
filesystem, captured together and restored together.

```go
snap, err := client.SnapshotSandbox(ctx, ref, crafting.SnapshotOptions{
    Workspace:    "dev",
    Dependencies: []string{"db"},
})

forkA, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
    Name: "fork-a", Template: "agent-sandbox", From: snap,
})
forkB, err := client.CreateSandbox(ctx, crafting.CreateSandboxOptions{
    Name: "fork-b", Template: "agent-sandbox", From: snap,
})
```

`forkA` and `forkB` each get their own copy of the files *and* their own copy of
the database, so two candidate changes can run in parallel without either seeing
the other's rows.

`CompositeSnapshot` round-trips through a single opaque string via `Encode` and
`DecodeCompositeSnapshot`, for callers that can only persist a string — a
workflow history, for instance.

## Naming

Crafting sandbox names are at most 20 characters and must start with a letter,
so external identifiers rarely fit. `DeriveSandboxName` hashes one into a valid
name deterministically, which also makes creation idempotent: the same seed
resolves to the same sandbox rather than a duplicate.

```go
name, err := crafting.DeriveSandboxName("agent", someWorkflowID)
```

## Authentication

With no token configured the client uses whatever session `cs` already holds,
which is what a program running inside a Crafting sandbox inherits. For a worker
running elsewhere, pass a service-account token and an isolated config
directory so the client never disturbs a developer's own session on the host.
The token is supplied to `cs` through the environment rather than the command
line, so it does not appear in the process table.

```go
client, err := crafting.NewClient(crafting.Options{
    Org:       "eng",
    Folder:    "lab",
    Token:     os.Getenv("CRAFTING_API_TOKEN"),
    ConfigDir: "/var/lib/worker/cs",
})
```

## Testing

`craftingtest` provides a fake CLI so callers can be tested without a Crafting
organization:

```go
runner := craftingtest.NewRunner().
    On("sandbox show", craftingtest.RunningState(), nil).
    OnExec("build output", "", 0)

client, _ := crafting.NewClient(crafting.Options{Folder: "lab", Runner: runner})
```

Integration tests drive a real organization. They skip unless a template is set,
and they delete every sandbox and snapshot they create:

```bash
CRAFTING_TEST_ORG=eng \
CRAFTING_TEST_FOLDER=lab \
CRAFTING_TEST_TEMPLATE=agent-sandbox \
CRAFTING_TEST_WORKSPACE=dev \
CRAFTING_TEST_DEPENDENCY=db \
go test -tags integration -timeout 40m -v ./...
```

## Requirements

- Go 1.24 or newer. The module has no dependencies outside the standard library.
- The `cs` CLI on the host, from the Crafting web console.

## License

MIT
