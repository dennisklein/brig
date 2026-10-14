// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The scripts below snapshot and restore the /sandbox of OpenShell
// sandboxes. They run as User over ssh. A sandbox's /sandbox is a Podman
// volume, whose files belong to the IDs that User's user namespace maps,
// so User cannot even read them directly: every step on them runs under
// podman unshare, where they have the owners the sandbox sees.
//
// A snapshot is a directory SnapshotDir/WORKSPACE/SANDBOX/TIME with the
// copy, data, and manifest.json, which brig writes. data is a btrfs
// subvolume. A restore makes the volume's _data a subvolume too, so that
// the next snapshot of it is an atomic btrfs snapshot rather than a copy
// that walks the files.

// SnapshotDir is where the snapshots of sandboxes live, relative to User's
// home, on the data disk next to Podman's volumes.
const SnapshotDir = ".local/share/brig/snapshots"

// NotBtrfs is what the snapshot scripts write to standard error on a data
// disk that is not btrfs, which cannot hold snapshots.
const NotBtrfs = "brig: the data disk is not btrfs"

// snapshotPart matches a workspace or sandbox name, and a snapshot's time.
var snapshotPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// CheckSnapshotPath accepts the path of a snapshot relative to
// SnapshotDir: WORKSPACE/SANDBOX/TIME.
func CheckSnapshotPath(rel string) error {
	parts := strings.Split(rel, "/")
	if len(parts) != 3 {
		return fmt.Errorf("invalid snapshot path %q: want WORKSPACE/SANDBOX/TIME", rel)
	}
	for _, p := range parts {
		if !snapshotPart.MatchString(p) || strings.HasSuffix(p, ".partial") {
			return fmt.Errorf("invalid snapshot path %q", rel)
		}
	}
	return nil
}

// CheckResetPath accepts a path to leave out of a restore: a clean path
// relative to /sandbox, inside it.
func CheckResetPath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("invalid reset path %q: want a clean path relative to /sandbox, inside it", p)
	}
	return nil
}

// unshare returns the lines of User's script that set mp to the mount
// point of volume and run inner under podman unshare with args, after
// root, the snapshot directory, and mp.
func unshare(volume, inner string, args ...string) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "root=\"$HOME\"/%s\n", SnapshotDir)
	if volume != "" {
		fmt.Fprintf(&b, "mp=$(podman volume inspect --format '{{.Mountpoint}}' %s)\n", shQuote(volume))
	} else {
		b.WriteString("mp=\n")
	}
	fmt.Fprintf(&b, "exec podman unshare sh -c %s sh \"$root\" \"$mp\"", shQuote(inner))
	for _, a := range args {
		b.WriteString(" " + shQuote(a))
	}
	b.WriteString("\n")
	return b.String()
}

// btrfsCheck fails unless the directory in $1 is on btrfs.
const btrfsCheck = `[ "$(stat -f -c %T "$1")" = btrfs ] || { echo '` + NotBtrfs + `' >&2; exit 3; }
`

// copyTree makes $2 a subvolume holding what directory $1 holds, with
// $1's owner and mode: a snapshot of $1 if it is a subvolume, whose root
// directory has inode 256, else a reflink copy, which fails rather than
// copy the data where the filesystem cannot share it.
const copyTree = `copy_tree() {
	if [ "$(stat -c %i "$1")" = 256 ]; then
		btrfs subvolume snapshot "$1" "$2" >/dev/null
	else
		btrfs subvolume create "$2" >/dev/null
		cp -a --reflink=always "$1/." "$2/"
	fi
}
`

// credentialFiles are files in /sandbox that hold a real credential, as
// /login inside a sandbox leaves them, rather than a provider's
// placeholder.
var credentialFiles = []string{".claude/.credentials.json", ".local/share/opencode/auth.json"}

// SnapshotCreateScript returns User's script that copies the /sandbox of
// the stopped sandbox whose workspace is volume into a new snapshot rel,
// with manifest as its manifest.json. A partial snapshot is removed. It
// prints "credential PATH" for each file in the copy that seems to hold a
// real credential.
func SnapshotCreateScript(volume, rel, manifest string) (string, error) {
	if err := CheckSnapshotPath(rel); err != nil {
		return "", err
	}
	var check strings.Builder
	for _, f := range credentialFiles {
		fmt.Fprintf(&check, "[ -f \"$dst/data/%s\" ] && echo 'credential %[1]s'\n", f)
	}
	check.WriteString("grep -qs oauth_token \"$dst/data/.config/gh/hosts.yml\" && echo 'credential .config/gh/hosts.yml'\n")
	inner := `set -eu
root=$1 mp=$2 rel=$3
dst=$root/$rel tmp=$root/$rel.partial
mkdir -p "$root"
` + strings.ReplaceAll(btrfsCheck, "$1", "$root") + `[ ! -e "$dst" ] || { echo "brig: snapshot $rel exists" >&2; exit 1; }
mkdir -p "${tmp%/*}"
rm -rf "$tmp"
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp"
` + copyTree + `copy_tree "$mp" "$tmp/data"
printf '%s\n' "$4" >"$tmp/manifest.json"
mv "$tmp" "$dst"
trap - EXIT
` + check.String() + "exit 0\n"
	return unshare(volume, inner, rel, manifest), nil
}

// SnapshotRestoreScript returns User's script that replaces the /sandbox of
// the stopped sandbox whose workspace is volume with what snapshot rel
// holds, leaving out the paths resets. It builds the new /sandbox as a
// subvolume beside the old one and swaps them, so that a failure leaves
// the old one. When the old /sandbox's root has another owner than the
// snapshot's, as for an image with another user, the files of the
// snapshot's owner change to the old one's, and the script prints
// "others N", the number of files with yet other owners.
func SnapshotRestoreScript(volume, rel string, resets []string) (string, error) {
	if err := CheckSnapshotPath(rel); err != nil {
		return "", err
	}
	for _, r := range resets {
		if err := CheckResetPath(r); err != nil {
			return "", err
		}
	}
	inner := `set -eu
root=$1 mp=$2 rel=$3
shift 3
src=$root/$rel/data
[ -d "$src" ] || { echo "brig: no snapshot $rel" >&2; exit 1; }
` + strings.ReplaceAll(btrfsCheck, "$1", "$mp") + `dir=${mp%/*}
new=$dir/_data.brig-new old=$dir/_data.brig-old
rm -rf "$new" "$old"
trap 'rm -rf "$new"' EXIT
` + copyTree + `copy_tree "$src" "$new"
base=$(cd "$new" && pwd -P)
for r; do
	# Never follow a link in the snapshot out of it.
	parent=$(realpath -m -- "$(dirname -- "$new/$r")")
	case $parent/ in
	"$base"/*) rm -rf -- "$new/$r" ;;
	*) echo "brig: --reset $r leads out of /sandbox" >&2; exit 1 ;;
	esac
done
want=$(stat -c %u:%g "$mp") have=$(stat -c %u:%g "$new")
if [ "$want" != "$have" ]; then
	chown -hR --from="$have" "$want" "$new"
	echo "others $(find "$new" \( ! -uid "${want%:*}" -o ! -gid "${want#*:}" \) -print | wc -l)"
fi
mv "$mp" "$old"
mv "$new" "$mp" || { mv "$old" "$mp"; exit 1; }
trap - EXIT
rm -rf "$old"
`
	return unshare(volume, inner, append([]string{rel}, resets...)...), nil
}

// SnapshotDeleteScript returns User's script that removes the snapshots
// rels, and the directories of sandboxes and workspaces that this leaves
// empty. Blocks that a snapshot shares with others stay theirs.
func SnapshotDeleteScript(rels []string) (string, error) {
	for _, r := range rels {
		if err := CheckSnapshotPath(r); err != nil {
			return "", err
		}
	}
	inner := `set -eu
root=$1
shift 2
for rel; do
	rm -rf -- "$root/$rel"
	rmdir -- "$root/${rel%/*}" "$root/${rel%%/*}" 2>/dev/null || true
done
`
	return unshare("", inner, rels...), nil
}

// SnapshotListScript returns User's script that prints each snapshot as a
// line "snapshot WORKSPACE/SANDBOX/TIME" and its manifest on the next. With
// sizes, it then prints the snapshots' sizes in bytes as btrfs filesystem
// du -s --raw does: total, exclusive, set shared and path, the path
// ending in /data.
func SnapshotListScript(sizes bool) string {
	inner := `set -eu
[ -d "$1" ] || exit 0
cd "$1"
set --
for d in */*/*; do
	case $d in *.partial) continue ;; esac
	[ -f "$d/manifest.json" ] || continue
	printf 'snapshot %s\n' "$d"
	head -c 1048576 "$d/manifest.json" | tr -d '\n'
	echo
	set -- "$@" "$d/data"
done
` + fmt.Sprintf("if [ %t = true ] && [ $# -gt 0 ]; then btrfs filesystem du -s --raw \"$@\" || true; fi\n", sizes)
	return unshare("", inner)
}

// SnapshotPlanScript returns User's script that prints what brig sandbox
// recreate checks in the VM before it changes anything, a line each:
// "volume yes" or "volume no" for the workspace volume of the sandbox,
// "fs TYPE" for the filesystem of the snapshots, "image REF ID WORKDIR"
// for each of images, "-" for a value the image lacks, and "mount VOL yes"
// or "mount VOL no" for each of mounts, the volumes of sandbox mounts.
// With size, it adds "size BYTES", what the volume's files take.
func SnapshotPlanScript(volume string, images, mounts []string, size bool) string {
	var b strings.Builder
	b.WriteString("set -u\n")
	fmt.Fprintf(&b, "root=\"$HOME\"/%s\nmkdir -p \"$root\"\n", SnapshotDir)
	b.WriteString("echo \"fs $(stat -f -c %T \"$root\")\"\n")
	for _, img := range images {
		fmt.Fprintf(&b, "printf 'image %%s %%s\\n' %s \"$(podman image inspect --format '{{.Id}} {{if .Config.WorkingDir}}{{.Config.WorkingDir}}{{else}}-{{end}}' %[1]s 2>/dev/null || echo '- -')\"\n", shQuote(img))
	}
	for _, m := range mounts {
		fmt.Fprintf(&b, "if podman volume exists %s; then echo 'mount '%[1]s' yes'; else echo 'mount '%[1]s' no'; fi\n", shQuote(m))
	}
	q := shQuote(volume)
	fmt.Fprintf(&b, "if mp=$(podman volume inspect --format '{{.Mountpoint}}' %s 2>/dev/null); then\n\techo 'volume yes'\n", q)
	if size {
		b.WriteString("\techo \"size $(podman unshare du -sb \"$mp\" | cut -f1)\"\n")
	}
	b.WriteString("else\n\techo 'volume no'\nfi\n")
	return b.String()
}
