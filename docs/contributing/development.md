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

The tests of the vm runner drive a fake hypervisor. To check its VMs on a
real host, with KVM and libvirt, point a test at an image with
`qemu-guest-agent`:

```sh
BRIG_TEST_VM_IMAGE=~/vm-images/Fedora-Cloud-Base-Generic-44.x86_64.qcow2 \
  go test ./internal/vmrunner -run TestHost -v -timeout 15m
```

[Docs](docs.md) describes how the documentation is laid out and the tests
that keep it in step with the code.
