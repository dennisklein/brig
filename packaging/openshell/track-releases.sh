#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Open an issue for each new OpenShell release, so that someone checks the
# packaging and brig against it.
#
# A release is new when it is newer than every release named in the title of
# an issue labelled openshell-release, closed issues included. Without such an
# issue only the latest release is new, so the first run does not open an
# issue for every past release.
#
# Env:  GH_TOKEN           token that can read and create issues and labels
#       GITHUB_REPOSITORY  repository to open the issues in (owner/name)
set -euo pipefail

upstream=NVIDIA/OpenShell
label=openshell-release
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is not set}

# Upstream's releases as "tag<TAB>url" lines, newest first. Only vX.Y.Z tags
# count, as make-srpm.sh packages no others.
releases=$(gh api --paginate "repos/$upstream/releases?per_page=100" --jq '
  .[]
  | select(((.draft or .prerelease) | not) and (.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")))
  | [.tag_name, .html_url] | @tsv' | sort -rV)
if [ -z "$releases" ]; then
  echo "::error::$upstream has no vX.Y.Z releases"
  exit 1
fi
tags=()
urls=()
while IFS=$'\t' read -r tag url; do
  tags+=("$tag")
  urls+=("$url")
done <<<"$releases"

tracked=$(gh issue list --repo "$repo" --label "$label" --state all --limit 1000 --json title --jq '.[].title' |
  { grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' || true; } | sort -V | tail -n 1)

newer() { [ "$1" != "$2" ] && [ "$(printf '%s\n' "$1" "$2" | sort -V | tail -n 1)" = "$1" ]; }

if [ -z "$tracked" ]; then
  count=1
else
  count=0
  while [ "$count" -lt "${#tags[@]}" ] && newer "${tags[count]}" "$tracked"; do
    count=$((count + 1))
  done
fi
if [ "$count" -eq 0 ]; then
  echo "No OpenShell release newer than $tracked"
  exit 0
fi

gh api "repos/$repo/labels/$label" --silent 2>/dev/null ||
  gh label create "$label" --repo "$repo" --color 1d76db --description "New upstream OpenShell release"

# Oldest first, so that the issue numbers follow the releases.
for ((i = count - 1; i >= 0; i--)); do
  tag=${tags[i]}
  prev=${tags[i + 1]:-}
  body="NVIDIA OpenShell [$tag](${urls[i]}) is out"
  if [ -n "$prev" ]; then
    body+=" ([changes since $prev](https://github.com/$upstream/compare/$prev...$tag))"
  fi
  body+=".

The [packages workflow](https://github.com/$repo/actions/workflows/packages.yaml) builds and publishes the latest release by itself. Check that it did and whether the packaging or brig need changes for this release; [\`.claude/skills/openshell-release/SKILL.md\`](https://github.com/$repo/blob/main/.claude/skills/openshell-release/SKILL.md) lists what to check. A maintainer can have Claude Code work through it with \`/openshell-release $tag\`."
  gh issue create --repo "$repo" --label "$label" --title "OpenShell $tag released" --body "$body"
done
