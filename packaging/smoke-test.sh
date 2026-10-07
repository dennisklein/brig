#!/usr/bin/env bash
# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Install the OpenShell packages from a repository site the way users do and
# check that they run. Meant for a throwaway Fedora container.
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
  openshell openshell-gateway openshell-prover python3-openshell

openshell --version
openshell-gateway --version
openshell-prover --version
rpm -q --qf '%{NAME}-%{VERSION}-%{RELEASE}: %{SIGPGP:pgpsig}\n' \
  brig-release openshell openshell-gateway openshell-prover python3-openshell
