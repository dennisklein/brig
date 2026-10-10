#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Decide what the packages workflow builds and publishes.
#
# Env:  PAGES_URL      published repository site
#       INPUT_TAG      OpenShell release tag; empty means the latest release
#       INPUT_BRIG_TAG brig release tag; empty means the latest release
#       REBUILD        "true" rebuilds even if the packages are published
#                      or the repository would not keep the release
#       GITHUB_REF     only refs/heads/main may publish
#       GITHUB_EVENT_NAME  "push" republishes the site even if nothing needs
#                      building, so that changes to it go live
#       KEEP_VERSIONS  versions kept per package (default 2); a release that
#                      publish.sh would prune is not built
# Writes tag, version, fedora (JSON list), build (true|false), brig_tag,
# brig_version, brig_build (true|false) and publish (real|dry-run|none) to
# $GITHUB_OUTPUT (stdout when unset). OpenShell and brig are decided apart:
# most runs build neither, and a release of one builds only its packages.
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

brig_tag=${INPUT_BRIG_TAG:-}
if [ -z "$brig_tag" ]; then
  # As for OpenShell: only a published release counts, not a bare tag, and
  # GitHub's latest release is never a pre-release.
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' https://github.com/dennisklein/brig/releases/latest)
  brig_tag=${url##*/}
fi
if [[ ! $brig_tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::invalid brig release tag: $brig_tag"
  exit 1
fi
brig_version=${brig_tag#v}

mapfile -t releases < <(grep -E '^[0-9]+$' "$here/fedora-releases")
baserelease=$(sed -n 's/^%global baserelease //p' "$here/openshell/openshell.spec")
brig_baserelease=$(sed -n 's/^%global baserelease //p' "$here/brig/brig.spec")
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

# superseded reports whether publish.sh would prune version $3 of package $2
# at once for Fedora release $1, as it keeps only the KEEP_VERSIONS newest
# versions and the newest of the previous major.minor line (see
# prune-repo.py); the next run would then build it again.
listing=
superseded() {
  local out status v kept=() line top_line version=$3
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
    sed -n "s|^.* rpm/fedora/$1/x86_64/$2-\\([0-9][^-/]*\\)-[^/]*\\.fc$1\\.x86_64\\.rpm\$|\\1|p" <<<"$listing"
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
brig_build=false
release_missing=false
for f in "${releases[@]}"; do
  dir=rpm/fedora/$f/x86_64
  if ! published "$dir/openshell-gateway-$version-$baserelease.fc$f.x86_64.rpm"; then
    if superseded "$f" openshell-gateway "$version"; then
      echo "::notice::the repository would not keep $tag next to the published versions for Fedora $f; not building it" >&2
    else
      build=true
    fi
  fi
  if ! published "$dir/brig-$brig_version-$brig_baserelease.fc$f.x86_64.rpm"; then
    if superseded "$f" brig "$brig_version"; then
      echo "::notice::the repository would not keep brig $brig_tag next to the published versions for Fedora $f; not building it" >&2
    else
      brig_build=true
    fi
  fi
  published "$dir/brig-release-$rel_version-$rel_release.noarch.rpm" || release_missing=true
done
if [ "${REBUILD:-false}" = true ]; then
  build=true
  brig_build=true
fi

publish=none
if [ "$build" = true ] || [ "$brig_build" = true ] || [ "$release_missing" = true ] || [ "${GITHUB_EVENT_NAME:-}" = push ]; then
  if [ "${GITHUB_REF:-}" = refs/heads/main ] && [ -s "$here/brig-release/RPM-GPG-KEY-brig" ]; then
    publish=real
  else
    if [ ! -s "$here/brig-release/RPM-GPG-KEY-brig" ]; then
      echo "::warning::packaging/brig-release/RPM-GPG-KEY-brig is missing; run scripts/signing-key.sh. Publishing as a dry run with a throwaway key."
    fi
    publish=dry-run
    # A dry run restores nothing, and the smoke test installs brig.
    brig_build=true
  fi
fi

{
  echo "tag=$tag"
  echo "version=$version"
  echo "fedora=$(printf '%s\n' "${releases[@]}" | jq -R . | jq -cs .)"
  echo "build=$build"
  echo "brig_tag=$brig_tag"
  echo "brig_version=$brig_version"
  echo "brig_build=$brig_build"
  echo "publish=$publish"
} >> "$out"
