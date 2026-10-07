# brig

`brig` manages the lifecycle of agent-sandbox VMs on a Fedora laptop. Each VM
runs headless Fedora with an [NVIDIA OpenShell](https://github.com/NVIDIA/OpenShell)
gateway installed from OpenShell's official RPMs, so autonomous agents are
contained by a VM boundary in addition to OpenShell's own sandboxing.

> **Status:** early development. Expect breaking changes.

## Install

With [mise](https://mise.jdx.dev):

```sh
mise use -g github:dennisklein/brig
```

Or with Go 1.26 or newer:

```sh
go install github.com/dennisklein/brig@latest
```

Release archives for linux/amd64 and their checksums are attached to each
[GitHub release](https://github.com/dennisklein/brig/releases).

## Development

Tool versions are pinned in `mise.toml`:

```sh
mise install
go test ./...
golangci-lint run ./...
goreleaser release --snapshot --clean   # local release dry run
```

### Releasing

Releases are cut from annotated tags; the tag message becomes the release
notes:

```sh
git tag -a v0.1.0      # write the release notes in the editor
git push origin v0.1.0
```

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
