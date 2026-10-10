#!/bin/sh
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0

# Shows brig's progress display offline: runs "brig image build" with a fake
# mkosi, so that it needs neither libvirt nor network, only qemu-img. Extra
# arguments go to brig, such as "--progress plain"; DEMO_FAIL=1 makes the
# fake mkosi fail after it printed an escape sequence.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
demo=$(mktemp -d)
trap 'rm -rf "$demo"' EXIT
mkdir "$demo/bin"

cat >"$demo/bin/mkosi" <<'EOF'
#!/bin/sh
if [ "$1" = --version ]; then
	echo 'mkosi 26'
	exit 0
fi
for a in "$@"; do
	case "$a" in
	--output-directory=*) out="${a#*=}" ;;
	esac
done
i=0
for pkg in bash coreutils glibc systemd openssh-server openshell openshell-gateway \
	podman crun passt iproute nftables dnf5 kernel-core dracut grub2 shadow-utils \
	util-linux sudo vim-minimal; do
	i=$((i + 1))
	if [ $((i % 2)) = 0 ]; then
		echo "Installing $pkg..."
	else
		echo "Installing $pkg..." >&2
	fi
	sleep 0.3
done
if [ -n "${DEMO_FAIL:-}" ]; then
	printf 'dnf: \033]0;pwned\007transaction failed\n' >&2
	exit 1
fi
truncate -s 256M "$out/base.raw"
cat >"$out/base.manifest" <<'JSON'
{
  "manifest_version": 1,
  "config": {"name": "image", "distribution": "fedora", "architecture": "x86-64", "output_format": "disk", "release": "44"},
  "packages": [
    {"type": "rpm", "name": "bash", "version": "5.3.0-2.fc44", "architecture": "x86_64", "size": 8616030},
    {"type": "rpm", "name": "openshell-gateway", "version": "0.1.2-1.fc44", "architecture": "x86_64", "size": 52000000}
  ],
  "extension": {}
}
JSON
EOF
chmod +x "$demo/bin/mkosi"

PATH="$demo/bin:$PATH" \
XDG_CONFIG_HOME="$demo/config" XDG_DATA_HOME="$demo/data" XDG_CACHE_HOME="$demo/cache" \
	go run "$root" image build --fedora 44 "$@"
