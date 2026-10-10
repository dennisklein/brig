#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Decide what the packages workflow builds and publishes.
#
# Env:  PAGES_URL      published repository site
#       INPUT_TAG      OpenShell release tag; empty means the latest release
#       REBUILD        "true" rebuilds even if the packages are published
#       GITHUB_REF     only refs/heads/main may publish
#       GITHUB_EVENT_NAME  "push" republishes the site even if nothing needs
#                      building, so that changes to it go live
# Writes tag, version, fedora (JSON list), build (true|false) and
# publish (real|dry-run|none) to $GITHUB_OUTPUT (stdout when unset).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${GITHUB_OUTPUT:-/dev/stdout}

tag=${INPUT_TAG:-}
if [ -z "$tag" ]; then
  # The redirect target of /releases/latest names the newest release that
  # has assets; the highest git tag may not have been released yet.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/NVIDIA/OpenShell/releases/latest)
  tag=${url##*/}
fi
if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::invalid OpenShell release tag: $tag"
  exit 1
fi
version=${tag#v}

mapfile -t releases < <(grep -E '^[0-9]+$' "$here/fedora-releases")
baserelease=$(sed -n 's/^%global baserelease //p' "$here/openshell/openshell.spec")
rel_version=$(sed -n 's/^Version: *//p' "$here/brig-release/brig-release.spec")
rel_release=$(sed -n 's/^Release: *//p' "$here/brig-release/brig-release.spec")

# published reports whether a file is on the site. Only a 404 means it is
# not: on any other failure, guessing could rebuild and republish.
published() {
  local status
  status=$(curl -sSI --retry 3 -o /dev/null -w '%{http_code}' "$PAGES_URL/$1") || status=failed
  case $status in
    200) return 0 ;;
    404) return 1 ;;
  esac
  echo "::error::checking $PAGES_URL/$1 failed ($status)" >&2
  exit 1
}

build=false
release_missing=false
for f in "${releases[@]}"; do
  dir=rpm/fedora/$f/x86_64
  published "$dir/openshell-gateway-$version-$baserelease.fc$f.x86_64.rpm" || build=true
  published "$dir/brig-release-$rel_version-$rel_release.noarch.rpm" || release_missing=true
done
if [ "${REBUILD:-false}" = true ]; then
  build=true
fi

publish=none
if [ "$build" = true ] || [ "$release_missing" = true ] || [ "${GITHUB_EVENT_NAME:-}" = push ]; then
  if [ "${GITHUB_REF:-}" = refs/heads/main ] && [ -s "$here/brig-release/RPM-GPG-KEY-brig" ]; then
    publish=real
  else
    if [ ! -s "$here/brig-release/RPM-GPG-KEY-brig" ]; then
      echo "::warning::packaging/brig-release/RPM-GPG-KEY-brig is missing; run scripts/signing-key.sh. Publishing as a dry run with a throwaway key."
    fi
    publish=dry-run
  fi
fi

{
  echo "tag=$tag"
  echo "version=$version"
  echo "fedora=$(printf '%s\n' "${releases[@]}" | jq -R . | jq -cs .)"
  echo "build=$build"
  echo "publish=$publish"
} >> "$out"
