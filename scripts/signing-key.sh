#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Create the OpenPGP key that signs brig's RPM packages and repository
# metadata. Run it once, on a trusted machine, from a checkout of the repo:
#
# - generates an RSA-4096 signing key without passphrase or expiry in a
#   throwaway GnuPG home
# - creates the GitHub environment rpm-signing if needed and restricts it to
#   the main branch, so that only workflow runs on main can read its secrets;
#   it removes any other deployment branch or tag policy of that environment
# - stores the secret key as secret RPM_SIGNING_KEY of that environment via
#   the gh CLI
# - writes the public key to packaging/brig-release/RPM-GPG-KEY-brig, to be
#   committed
# - keeps a backup of the secret key and its revocation certificate in
#   ${XDG_DATA_HOME:-~/.local/share}/brig/signing-key/; move them to offline
#   storage
#
# Env: BRIG_SIGNING_UID  key user ID (default: brig package signing <d.klein@gsi.de>)
#      BRIG_REPO         GitHub repository (default: dennisklein/brig)
set -euo pipefail

uid=${BRIG_SIGNING_UID:-brig package signing <d.klein@gsi.de>}
repo=${BRIG_REPO:-dennisklein/brig}
environment=rpm-signing
secret=RPM_SIGNING_KEY

for cmd in gpg gh git; do
  command -v "$cmd" > /dev/null || { echo "$cmd is required" >&2; exit 1; }
done
root=$(git rev-parse --show-toplevel)
pubkey=$root/packaging/brig-release/RPM-GPG-KEY-brig
backup=${XDG_DATA_HOME:-$HOME/.local/share}/brig/signing-key
if [ -e "$pubkey" ]; then
  echo "$pubkey already exists; to rotate the key, delete it and the $secret secret and bump brig-release's Version" >&2
  exit 1
fi
gh auth status > /dev/null
# The public key may be missing from this checkout although the secret exists,
# e.g. in a clone from before the key was committed. Only a 404 means that
# neither the environment nor the secret exists yet.
if out=$(gh api "repos/$repo/environments/$environment/secrets/$secret" 2>&1); then
  echo "$secret already exists in the $environment environment of $repo; update this checkout to get ${pubkey#"$root"/}" >&2
  echo "or, to rotate the key, delete the secret and bump brig-release's Version" >&2
  exit 1
elif ! grep -q 'HTTP 404' <<< "$out"; then
  echo "cannot check for an existing $secret: $out" >&2
  exit 1
fi

# Restrict the environment before any key exists. This replaces the
# environment's protection rules with a deployment branch policy and removes
# every other branch or tag policy an existing environment may have, so that
# main is the only ref that can read the key.
gh api --silent --method PUT "repos/$repo/environments/$environment" \
  -F 'deployment_branch_policy[protected_branches]=false' \
  -F 'deployment_branch_policy[custom_branch_policies]=true'
policies="repos/$repo/environments/$environment/deployment-branch-policies"
# Read the listing into a variable first: a failed listing then aborts the
# script, where one read through process substitution would pass as "no
# policies".
list=$(gh api --paginate "$policies" --jq '.branch_policies[] | [.id, .type, .name] | @tsv')
have_main=
while IFS=$'\t' read -r id type name; do
  [ -n "$id" ] || continue
  if [ "$type" = branch ] && [ "$name" = main ]; then
    have_main=1
  else
    echo "removing $type policy $name from the $environment environment" >&2
    gh api --silent --method DELETE "$policies/$id"
  fi
done <<< "$list"
if [ -z "$have_main" ]; then
  gh api --silent --method POST "$policies" -f name=main -f type=branch
fi

GNUPGHOME=$(mktemp -d)
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT

gpg --batch --quiet --pinentry-mode loopback --passphrase '' \
  --quick-gen-key "$uid" rsa4096 sign never
fpr=$(gpg --batch --with-colons --list-secret-keys | awk -F: '$1 == "fpr" { print $10; exit }')

umask 077
mkdir -p "$backup"
gpg --batch --pinentry-mode loopback --passphrase '' --armor \
  --export-secret-keys "$fpr" > "$backup/$fpr.secret.asc"
cp "$GNUPGHOME/openpgp-revocs.d/$fpr.rev" "$backup/$fpr.rev"

gh secret set "$secret" --repo "$repo" --env "$environment" < "$backup/$fpr.secret.asc"

umask 022
gpg --batch --armor --export "$fpr" > "$pubkey"

cat <<MSG

Created signing key $fpr ($uid).

- Secret key stored as $secret in the $environment environment of $repo,
  which only workflow runs on main can use.
- Public key written to ${pubkey#"$root"/}; commit and push it.
- Backups of the secret key and revocation certificate are in $backup.
  Move them to offline storage and delete them from this machine.
MSG
