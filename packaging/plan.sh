#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Decide what the packages workflow builds and publishes.
#
# Env:  PAGES_URL      published repository site
#       INPUT_TAG      OpenShell release tag; empty means the latest release
#       REBUILD        "true" rebuilds even if the packages are published
#                      or the repository would not keep the release
#       GITHUB_REF     only refs/heads/main may publish
#       GITHUB_EVENT_NAME  "push" republishes the site even if nothing needs
#                      building, so that changes to it go live
#       KEEP_VERSIONS  versions kept per package (default 2); a release that
#                      publish.sh would prune is not built
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

# superseded reports whether publish.sh would prune this version at once for
# Fedora release $1, as it keeps only the KEEP_VERSIONS newest versions and
# the newest of the previous major.minor line (see prune-repo.py); the next
# run would then build it again.
listing=
superseded() {
  local out status v kept=() line top_line
  if [ -z "$listing" ]; then
    out=$(curl -sSL --retry 3 -w '\n%{http_code}' "$PAGES_URL/packages.sha256") || out=$'\nfailed'
    status=${out##*$'\n'}
    case $status in
      200) listing=${out%$'\n'*} ;;
      404) listing=none ;;
      *)
        echo "::error::fetching $PAGES_URL/packages.sha256 failed ($status)" >&2
        exit 1 ;;
    esac
  fi
  local versions
  versions=$( {
    echo "$version"
    sed -n "s|^.* rpm/fedora/$1/x86_64/openshell-gateway-\\([0-9][^-/]*\\)-[^/]*\\.fc$1\\.x86_64\\.rpm\$|\\1|p" <<<"$listing"
  } | sort -Vru)
  top_line=
  while read -r v; do
    line=$(cut -d. -f1-2 <<<"$v")
    [ -n "$top_line" ] || top_line=$line
    if [ "${#kept[@]}" -lt "${KEEP_VERSIONS:-2}" ]; then
      kept+=("$v")
    elif [ "$line" != "$top_line" ]; then
      kept+=("$v")
      break
    fi
    if [ "$line" != "$top_line" ]; then
      break
    fi
  done <<<"$versions"
  for v in "${kept[@]}"; do
    [ "$v" != "$version" ] || return 1
  done
  return 0
}

build=false
release_missing=false
for f in "${releases[@]}"; do
  dir=rpm/fedora/$f/x86_64
  if ! published "$dir/openshell-gateway-$version-$baserelease.fc$f.x86_64.rpm"; then
    if superseded "$f"; then
      echo "::notice::the repository would not keep $tag next to the published versions for Fedora $f; not building it" >&2
    else
      build=true
    fi
  fi
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
