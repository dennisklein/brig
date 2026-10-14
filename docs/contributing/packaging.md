# Packaging

brig's VM images, and users' hosts, get OpenShell from
<https://dennisklein.github.io/brig/>, a signed dnf repository that the
[packages workflow](../../.github/workflows/packages.yaml) fills: it builds
each new OpenShell release and each new brig release from source for the
Fedora releases listed in
[`packaging/fedora-releases`](../../packaging/fedora-releases). It checks daily
and after each brig release. See [`packaging/`](../../packaging/).

## Package updates

Two workflows keep the package repository current:

- [`packages`](../../.github/workflows/packages.yaml) runs daily, on pushes to
  `main` that change `packaging/`, and on demand. If upstream's latest
  OpenShell release is not published yet, it builds `openshell`,
  `openshell-gateway`, `openshell-prover` and `python3-openshell` from the
  release's source, signs them, smoke-tests the repository and only then
  publishes it, keeping the previous version and the newest one of the previous
  minor line, so `--openshell` can still pin it. It skips a release that the
  repository would not keep, such as an older backport, unless rebuild is
  requested. A push republishes the site even when nothing needs building. The
  version, the gateway's systemd unit and the SDK's protobuf modules follow
  upstream by themselves; the rest of
  [`openshell.spec`](../../packaging/openshell/openshell.spec), such as file lists
  and dependencies, does not. To republish a version after changing the spec,
  bump its `baserelease`.

  The same run builds `brig` and `brig-selinux` from the latest brig release
  if it is not published for every Fedora release yet; a brig release starts
  the workflow itself, so that it reaches the repository within minutes. The
  build is offline, against the Go modules that
  [`make-srpm.sh`](../../packaging/brig/make-srpm.sh) vendors from the release tag.
  [`brig.spec`](../../packaging/brig/brig.spec) lists the dependencies of `brig
  print-fedora-deps` by hand, and a test fails when they differ. A run that
  builds neither keeps the published packages. To republish a version after
  changing the spec, bump its `baserelease`, too.
- [`openshell-releases`](../../.github/workflows/openshell-releases.yaml) runs
  weekly and opens an `openshell-release` issue for each new release, so that
  a maintainer checks the spec and brig against it. Running
  `/openshell-release <issue>` in Claude Code works through [the
  checklist](../../.claude/skills/openshell-release/SKILL.md); it never starts on
  its own.

GitHub disables scheduled workflows in a public repository after 60 days
without repository activity. Each scheduled run of these two workflows
therefore enables both again, so that a quiet period does not stop them.

## Package repository setup

One-time setup for maintainers:

1. Create the signing key on a trusted machine, from a checkout of this
   repository (needs `git`, `gpg` and an authenticated `gh`): `mise run
   signing-key` or `scripts/signing-key.sh`. It restricts the `rpm-signing`
   environment to the `main` branch (removing any other deployment branch or
   tag policy it finds there), stores the secret key as its secret
   `RPM_SIGNING_KEY` and writes the public key to
   `packaging/brig-release/RPM-GPG-KEY-brig`; commit it. It also leaves an
   unencrypted backup of the secret key and its revocation certificate in
   `~/.local/share/brig/signing-key/`; move them to offline storage.
2. In the repository settings, set *Pages → Source* to *GitHub Actions*.

`RPM-GPG-KEY-brig` lists every key that clients trust; the packages workflow
signs with one of them and re-signs the published packages that another
listed key signed. To replace the key, run `scripts/signing-key.sh` with
each of these options in turn, and bump brig-release's `Version` with the
`--next` and `--retire` commits, so that clients get the new keys from a
package that a key they trust signed:

1. `--next` adds a new key beside the current one and backs it up.
2. Once clients have updated brig-release, `--switch FILE` with that
   backup makes the next publish sign everything with the new key.
3. After that publish, `--retire FINGERPRINT` drops the old key.

After a key compromise, instead run `--next`, `--switch` and `--retire` at
once, then the packages workflow with *discard-published*, which publishes
only what it rebuilds. Clients then need the new key by hand: `sudo rpm
--import https://dennisklein.github.io/brig/RPM-GPG-KEY-brig`, and `sudo
rpmkeys --delete` with the old key's ID.
