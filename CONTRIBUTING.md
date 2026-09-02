# Contributing to logrotate

Thank you for your interest in contributing!

## Prerequisites

- Go 1.27 or later
- [golangci-lint](https://golangci-lint.run/) for linting

The module depends on the standard library only.

## Quick Start

```bash
# Clone the repository
git clone https://github.com/libtnb/logrotate.git
cd logrotate

# Vet and run the tests with the race detector
go vet ./...
go test -race ./...

# Run the linter
golangci-lint run ./...

# Fuzz the backup name parser (what CI runs, 30 s each)
go test -run=^$ -fuzz=FuzzParseBackupName -fuzztime=30s .
go test -run=^$ -fuzz=FuzzStampRoundTrip -fuzztime=30s .

# Benchmarks
go test -run=^$ -bench . -benchmem ./...
```

## Development Workflow

1. Fork the repository
2. Create a feature branch: `git checkout -b feat/my-feature`
3. Make your changes
4. Add tests for new functionality
5. Run `go test -race ./...` and `golangci-lint run ./...`
6. Describe user-visible changes under `[Unreleased]` in `CHANGELOG.md`
7. Commit with a descriptive message
8. Push and open a Pull Request

CI runs the tests on Linux, macOS and Windows. Guard POSIX-only assertions such as permission bits or renaming open files with `skipOnWindows` (see `logrotate_test.go`) or a build tag.

## Code Guidelines

- Follow [Effective Go](https://go.dev/doc/effective_go)
- Add doc comments to all exported symbols
- Keep the module free of third-party dependencies
- Keep tests deterministic: use `t.TempDir()` and `WithClock` instead of sleeping or reading the wall clock
- Every behavior change needs a test; a change to rotation, naming or retention semantics also needs an update to the package documentation in `doc.go`

## Reporting Issues

Use [GitHub Issues](https://github.com/libtnb/logrotate/issues). Include:

- Go version (`go version`)
- OS and architecture
- Steps to reproduce
- Expected vs actual behavior
