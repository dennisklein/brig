#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Build the openshell source RPM for an upstream release tag.
#
# Usage: make-srpm.sh <tag> <outdir>
#
# Fetches the tag from github.com/NVIDIA/OpenShell, archives it, vendors its
# crates, copies the gateway's systemd user unit from upstream's openshell.spec
# into ours and runs `rpmbuild -bs` without a dist tag, so one SRPM serves
# every Fedora release. Vendoring uses cargo-vendor-filterer to drop crates and
# prebuilt protoc binaries that an x86_64 Linux build never uses, which
# shrinks the vendor tarball by more than half. Needs git, cargo,
# cargo-vendor-filterer, xz and rpmbuild.
set -euo pipefail

tag=${1:?usage: make-srpm.sh <tag> <outdir>}
outdir=${2:?usage: make-srpm.sh <tag> <outdir>}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "invalid release tag: $tag" >&2; exit 1; }
version=${tag#v}

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git clone --quiet --depth 1 --branch "$tag" https://github.com/NVIDIA/OpenShell.git "$work/src"
commit=$(git -C "$work/src" rev-parse HEAD)

mkdir -p "$work/sources"
git -C "$work/src" archive --format=tar.gz --prefix="openshell-$version/" \
  -o "$work/sources/openshell-$version.tar.gz" HEAD

filter=(--platform x86_64-unknown-linux-gnu)
for platform in linux-aarch_64 linux-ppcle_64 linux-s390_64 linux-x86_32 macos-aarch_64 macos-x86_64 win32; do
  filter+=(--exclude-crate-path "protoc-bin-vendored-$platform#bin")
done
(cd "$work/src" && CARGO_HTTP_TIMEOUT=600 CARGO_NET_RETRY=5 cargo vendor-filterer "${filter[@]}" vendor)
# The vendored crates must match the release's lock file exactly.
git -C "$work/src" diff --exit-code -- Cargo.lock
tar -C "$work/src" -cJf "$work/sources/openshell-$version-vendor.tar.xz" vendor

# The gateway's systemd user unit, as upstream's spec writes it in a heredoc.
if ! awk '
    !unit && /^cat > .*-gateway\.service << / { end = $NF; gsub(/[\047"]/, "", end); unit = 1; next }
    unit && $0 == end { found = 1; exit }
    unit { print }
    END { exit !found }' "$work/src/openshell.spec" > "$work/gateway.service" ||
  ! grep -q '^ExecStart=' "$work/gateway.service"; then
  echo "cannot find the gateway unit in upstream's openshell.spec" >&2
  exit 1
fi

sed -e "s/^%global openshell_version .*/%global openshell_version $version/" \
    -e "s/^%global openshell_commit .*/%global openshell_commit $commit/" \
    "$here/openshell.spec" |
  awk -v unit="$work/gateway.service" '
    $0 == "@GATEWAY_UNIT@" { while ((getline line < unit) > 0) print line; n++; next }
    { print }
    END { exit n != 1 }' > "$work/sources/openshell.spec"

mkdir -p "$outdir"
rpmbuild -bs --nodeps \
  --define "_sourcedir $work/sources" \
  --define "_srcrpmdir $outdir" \
  --define "dist %{nil}" \
  "$work/sources/openshell.spec"
