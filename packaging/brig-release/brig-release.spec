# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0

Name:           brig-release
# Bump Version when brig.repo or the signing key changes.
Version:        1
Release:        1
Summary:        dnf repository configuration for brig's Fedora packages

License:        Apache-2.0
URL:            https://github.com/dennisklein/brig
Source0:        brig.repo
Source1:        RPM-GPG-KEY-brig

BuildArch:      noarch

%description
Configures the dnf repository at https://dennisklein.github.io/brig/, which
serves OpenShell packages built from source and signed by the brig project,
and installs the OpenPGP key that verifies its packages and metadata.

%prep

%build

%install
install -Dpm 0644 %{SOURCE0} %{buildroot}%{_sysconfdir}/yum.repos.d/brig.repo
install -Dpm 0644 %{SOURCE1} %{buildroot}%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-brig

%files
%config(noreplace) %{_sysconfdir}/yum.repos.d/brig.repo
%{_sysconfdir}/pki/rpm-gpg/RPM-GPG-KEY-brig

%changelog
* Wed Oct 07 2026 Dennis Klein <d.klein@gsi.de> - 1-1
- Initial package
