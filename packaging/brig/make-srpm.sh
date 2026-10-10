#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Build the brig source RPM for a release tag.
#
# Usage: make-srpm.sh <tag> <outdir>
#
# Clones the tag from $BRIG_REPO (default github.com/dennisklein/brig),
# archives it, vendors its Go modules and runs `rpmbuild -bs` without a dist
# tag, so one SRPM serves every Fedora release. The rpm jobs turn it into the
# SRPM that they publish, one per release. Needs git, go (as new as go.mod
# asks), xz, tar and rpmbuild.
set -euo pipefail

tag=${1:?usage: make-srpm.sh <tag> <outdir>}
outdir=${2:?usage: make-srpm.sh <tag> <outdir>}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "invalid release tag: $tag" >&2; exit 1; }
version=${tag#v}

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$tag" "${BRIG_REPO:-https://github.com/dennisklein/brig.git}" "$work/src"

mkdir -p "$work/sources"
git -C "$work/src" archive --format=tar.gz --prefix="brig-$version/" \
  -o "$work/sources/brig-$version.tar.gz" HEAD

(cd "$work/src" && go mod vendor)
# The vendored modules must match the release's go.mod and go.sum exactly.
git -C "$work/src" diff --exit-code -- go.mod go.sum
# Sorted and without owners or times, so that the same tag vendors alike.
epoch=$(git -C "$work/src" log -1 --format=%ct)
tar -C "$work/src" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
  -cJf "$work/sources/brig-$version-vendor.tar.xz" vendor

cp "$here/brig.cil" "$work/sources/"
sed "s/^%global brig_version .*/%global brig_version $version/" "$here/brig.spec" > "$work/sources/brig.spec"

mkdir -p "$outdir"
rpmbuild -bs --nodeps \
  --define "_sourcedir $work/sources" \
  --define "_srcrpmdir $outdir" \
  --define "dist %{nil}" \
  "$work/sources/brig.spec"
