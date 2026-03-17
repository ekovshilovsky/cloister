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

## Pull Requests

1. Fork the repo and create a feature branch
2. Write tests for new functionality
3. Run `go test ./...` and `gofmt -s -w .` before submitting
4. Keep PRs focused — one feature or fix per PR

## Reporting Issues

Open an issue at https://github.com/ekovshilovsky/cloister/issues with:
- macOS version
- `cloister version` output
- Steps to reproduce
- Expected vs actual behavior

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and testable
- Colima is an implementation detail — never expose it in user-facing output
