#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Install the OpenShell and brig packages from a repository site the way users
# do and check that they run. Meant for a throwaway Fedora container.
#
# Usage: smoke-test.sh <site-url>
set -euo pipefail

base=${1:?usage: smoke-test.sh <site-url>}
base=${base%/}

curl -fsSL --retry 5 --retry-all-errors "$base/RPM-GPG-KEY-brig" -o /tmp/RPM-GPG-KEY-brig
rpm --import /tmp/RPM-GPG-KEY-brig
dnf -y install "$base/brig-release.noarch.rpm"
# brig.repo points at the public site; follow the site under test instead.
dnf -y install --setopt="brig.baseurl=$base/rpm/fedora/\$releasever/\$basearch/" \
  openshell openshell-gateway openshell-prover python3-openshell brig brig-selinux

openshell --version
openshell-gateway --version
openshell-prover --version

# brig installs openshell (same repository) and, with SELinux policy, its
# module, which semodule must list: this loads the CIL of brig-selinux.
brig_version=$(rpm -q --qf '%{VERSION}' brig)
[ "$(brig --version)" = "brig version v$brig_version" ]
brig print-fedora-deps
semodule -l | grep -Eq '^brig([[:space:]]|$)'

rpm -q --qf '%{NAME}-%{VERSION}-%{RELEASE}: %{SIGPGP:pgpsig}\n' \
  brig-release openshell openshell-gateway openshell-prover python3-openshell brig brig-selinux

# The SDK's Python dependencies are only recommended, as Fedora may ship older
# ones than it needs. Install those its metadata names from PyPI into a virtual
# environment that also sees the package, and import it.
python3 -m venv --system-site-packages /tmp/sdk
/tmp/sdk/bin/python -c 'from importlib.metadata import requires; print(*requires("openshell"), sep="\n")' \
  > /tmp/sdk/requirements.txt
/tmp/sdk/bin/pip install --quiet -r /tmp/sdk/requirements.txt
sdk=$(/tmp/sdk/bin/python -c 'import openshell; print(openshell.__version__)')
echo "openshell SDK $sdk"
[ "$sdk" = "$(rpm -q --qf '%{VERSION}' python3-openshell)" ]
