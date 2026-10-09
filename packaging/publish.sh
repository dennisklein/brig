#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Assemble the signed dnf repository site.
#
# Usage: publish.sh <incoming-dir> <site-dir>
#
# Restores the packages already published at $PAGES_URL (listed with their
# SHA-256 in packages.sha256), signs and files the new packages from
# <incoming-dir>, prunes old versions, verifies every package signature,
# regenerates and signs the repository metadata, and writes the static site
# into <site-dir> with a listing of every directory.
#
# Env:  PAGES_URL        published repository site
#       KEEP_VERSIONS    versions to keep per package (default 2)
#       RESTORE          "false" starts from an empty repository instead of
#                        the published packages, which a dry run with
#                        another key could not verify (default true)
#       RPM_SIGNING_KEY  armored OpenPGP secret key without passphrase; it
#                        must match packaging/brig-release/RPM-GPG-KEY-brig
set -euo pipefail

incoming=${1:?usage: publish.sh <incoming-dir> <site-dir>}
site=${2:?usage: publish.sh <incoming-dir> <site-dir>}
here=$(cd "$(dirname "$0")" && pwd)
pubkey=$here/brig-release/RPM-GPG-KEY-brig
: "${PAGES_URL:?}" "${RPM_SIGNING_KEY:?RPM_SIGNING_KEY is not set}"
mapfile -t releases < <(grep -E '^[0-9]+$' "$here/fedora-releases")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GNUPGHOME=$work/gnupg
mkdir -m 0700 "$GNUPGHOME"

gpg --batch --quiet --import <<<"$RPM_SIGNING_KEY"
fpr=$(gpg --batch --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')
expected=$(gpg --batch --with-colons --show-keys "$pubkey" | awk -F: '$1 == "fpr" { print $10; exit }')
if [ -z "$fpr" ] || [ "$fpr" != "$expected" ]; then
  echo "::error::signing key '$fpr' does not match $pubkey ('$expected')"
  exit 1
fi

mkdir -p "$site"

# Restore the published packages; the site is the repository's only state.
if [ "${RESTORE:-true}" = false ]; then
  echo "not restoring the packages published at $PAGES_URL"
elif curl -fsSL "$PAGES_URL/packages.sha256" -o "$work/packages.sha256"; then
  while read -r _ path; do
    if [[ ! $path =~ ^rpm/fedora/([0-9]+/x86_64|source)/[A-Za-z0-9._+~^-]+\.rpm$ ]]; then
      echo "::error::unexpected path in packages.sha256: $path"
      exit 1
    fi
    mkdir -p "$site/${path%/*}"
    curl -fsSL --retry 3 "$PAGES_URL/$path" -o "$site/$path"
  done < "$work/packages.sha256"
  (cd "$site" && sha256sum --check --quiet --strict "$work/packages.sha256")
  # The listings show when each package was first published.
  if curl -fsSL "$PAGES_URL/packages.mtime" -o "$work/packages.mtime"; then
    while read -r mtime path; do
      if [[ ! $mtime =~ ^[0-9]+$ || ! $path =~ ^rpm/fedora/([0-9]+/x86_64|source)/[A-Za-z0-9._+~^-]+\.rpm$ ]]; then
        echo "::error::unexpected line in packages.mtime: $mtime $path"
        exit 1
      fi
      touch -c -m -d "@$mtime" "$site/$path"
    done < "$work/packages.mtime"
  fi
else
  echo "no packages.sha256 at $PAGES_URL; starting an empty repository"
fi

# Sign and file the new packages.
shopt -s nullglob
for pkg in "$incoming"/*.rpm; do
  rpmsign --define "_gpg_name $fpr" --addsign "$pkg" > /dev/null
  name=${pkg##*/}
  dests=()
  case $name in
    *.src.rpm)
      dests=(rpm/fedora/source) ;;
    brig-release-*.noarch.rpm)
      for f in "${releases[@]}"; do dests+=("rpm/fedora/$f/x86_64"); done ;;
    *.fc[0-9]*.x86_64.rpm | *.fc[0-9]*.noarch.rpm)
      f=${name##*.fc}
      dests=("rpm/fedora/${f%%.*}/x86_64") ;;
    *)
      echo "::error::cannot tell which repository $name belongs to"
      exit 1 ;;
  esac
  for d in "${dests[@]}"; do
    mkdir -p "$site/$d"
    cp "$pkg" "$site/$d/"
  done
done

# Every release repository carries the newest brig-release, also after a
# Fedora release was added without a new brig-release build.
newest=$(find "$site/rpm/fedora" -name 'brig-release-*.noarch.rpm' -printf '%f\t%p\n' | sort -V | tail -n1 | cut -f2)
if [ -z "$newest" ]; then
  echo "::error::no brig-release package to publish"
  exit 1
fi
for f in "${releases[@]}"; do
  mkdir -p "$site/rpm/fedora/$f/x86_64"
  cp -np "$newest" "$site/rpm/fedora/$f/x86_64/"
done
cp -p "$newest" "$site/brig-release.noarch.rpm"

# Prune, verify every signature, regenerate and sign the metadata.
mkdir "$work/rpmdb"
rpmkeys --dbpath "$work/rpmdb" --import "$pubkey"
for dir in "$site"/rpm/fedora/*/x86_64 "$site"/rpm/fedora/source; do
  [ -d "$dir" ] || continue
  python3 "$here/prune-repo.py" --keep "${KEEP_VERSIONS:-2}" "$dir"
  for pkg in "$dir"/*.rpm; do
    if ! rpmkeys --dbpath "$work/rpmdb" --checksig "$pkg" | grep -q 'signatures OK$'; then
      echo "::error::bad or missing signature: $pkg"
      rpmkeys --dbpath "$work/rpmdb" --checksig --verbose "$pkg"
      exit 1
    fi
  done
  createrepo_c --quiet "$dir"
  gpg --batch --yes --local-user "$fpr" --detach-sign --armor "$dir/repodata/repomd.xml"
done

cp "$pubkey" "$site/RPM-GPG-KEY-brig"
grouped=$(sed -E 's/(.{4})/\1 /g; s/ $//' <<<"$fpr")
sed -e "s/@FINGERPRINT@/$grouped/g" "$here/site/index.html" > "$site/index.html"
(cd "$site" && find rpm -name '*.rpm' | LC_ALL=C sort | xargs -r sha256sum) > "$site/packages.sha256"
(cd "$site" && find rpm -name '*.rpm' | LC_ALL=C sort | xargs -r stat -c '%Y %n') > "$site/packages.mtime"
python3 "$here/site-index.py" "$PAGES_URL" "$site"
echo "assembled $(wc -l < "$site/packages.sha256") packages signed by $fpr"
