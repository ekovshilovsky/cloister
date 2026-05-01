# Contributing to cloister

Thanks for your interest in contributing to cloister.

## Development

```bash
git clone https://github.com/ekovshilovsky/cloister.git
cd cloister
go mod tidy
make hooks
make build
make test
```

Run `make hooks` once after cloning to configure git to use the project's pre-commit hooks from `.githooks/`. The hooks enforce formatting (`gofmt -s`), static analysis (`go vet`), and the test suite before every commit.

## Architecture

cloister supports two VM backends:

- **Colima** (`internal/vm/colima/`) — Linux VMs for Claude Code isolation and Docker workloads
- **Lume** (`internal/vm/lume/`) — macOS VMs for OpenClaw and agents needing native macOS features

Both implement the `vm.Backend` interface (`internal/vm/backend.go`). The CLI layer in `cmd/` resolves the correct backend from each profile's `backend` field in the config.

Key packages:

| Package | Purpose |
|---------|---------|
| `cmd/` | Cobra CLI commands |
| `internal/config/` | YAML config types, Load/Save with `.prev` rotation |
| `internal/setup/` | OpenClaw setup wizard (orchestrator, sections, state, credentials) |
| `internal/vm/` | Backend interface, Colima and Lume implementations |
| `internal/provision/` | VM provisioning engines (Linux and macOS) |
| `internal/tunnel/` | SSH port forwarding management |
| `internal/agent/` | Legacy agent runtime (Colima/Docker only) |

## Pull Requests

1. Fork the repo and create a feature branch
2. Write tests for new functionality
3. Run `go test ./...` and `gofmt -s -w .` before submitting
4. Keep PRs focused — one feature or fix per PR

## Testing

`go test ./...` runs the default unit suite. A few suites are gated behind build tags because they require external state and only run on demand.

### GPG forwarding integration tests

Requires:
- a host with `git config --global user.signingkey` set
- `cloister setup gpg-forward` previously run
- a Colima profile named `cloister-test-gpg-forward` provisioned with `GPGSigning=true`

Run:

```sh
go test -tags integration_gpg ./internal/provision/linux/ -v -run TestGPGForward
```

## Reporting Issues

Open an issue at https://github.com/ekovshilovsky/cloister/issues with:
- macOS version
- `cloister --version` output
- Steps to reproduce
- Expected vs actual behavior

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and testable
- Backend-specific logic belongs in `internal/vm/colima/` or `internal/vm/lume/`, not in `cmd/`
- The `vm.Backend` interface is the abstraction boundary — `cmd/` code should never import a backend directly except for `resolveBackend()` in `root.go`
