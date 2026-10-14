# Development

Tool versions are pinned in `mise.toml`:

```sh
mise install
go test ./...
golangci-lint run ./...
govulncheck ./...
goreleaser release --snapshot --clean   # local release dry run
```

`go run . ui --demo --dev-assets internal/ui/web` serves the web UI's
templates and static files from disk, read again on each request, with
made-up VMs that do not touch the host.

[Docs](docs.md) describes how the documentation is laid out and the tests
that keep it in step with the code.
