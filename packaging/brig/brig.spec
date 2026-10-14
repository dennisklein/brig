# SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
# SPDX-License-Identifier: Apache-2.0
#
# Builds brig from a release tag: an offline Go build against vendored
# modules, with the flags of .goreleaser.yaml. make-srpm.sh makes the sources.

# Replaced by packaging/brig/make-srpm.sh with the packaged release.
%global brig_version 0.0.0
# Bump when the packaging changes without a new brig version.
%global baserelease 1

# No debuginfo packages: the binary is stripped by the linker, like the
# release archive's.
%global debug_package %{nil}

# Where to install completions. Fedora's bash-completion, zsh and fish packages
# define these macros; fall back to their directories so that building does not
# need those packages.
%{!?bash_completions_dir:%global bash_completions_dir %{_datadir}/bash-completion/completions}
%{!?zsh_completions_dir:%global zsh_completions_dir %{_datadir}/zsh/site-functions}
%{!?fish_completions_dir:%global fish_completions_dir %{_datadir}/fish/vendor_completions.d}
%{!?fish_vendor_conf_dir:%global fish_vendor_conf_dir %{_datadir}/fish/vendor_conf.d}

%global selinuxtype targeted

Name:           brig
Version:        %{brig_version}
Release:        %{baserelease}%{?dist}
Summary:        Manage agent-sandbox VMs on a Fedora laptop

# brig is Apache-2.0. The binary also contains Go's standard library and the
# vendored modules (LICENSE.dependencies): cobra, mousetrap, go-libvirt, grpc,
# genproto and OpenShell's Go SDK are Apache-2.0, yaml also MIT, libvirtxml
# MIT, crypto, net, sys, text, pflag, protobuf and Go itself BSD-3-Clause,
# go-libvirt's XDR code ISC. The web UI embeds htmx (0BSD) and fonts
# (OFL-1.1), listed in NOTICE.
License:        Apache-2.0 AND BSD-3-Clause AND MIT AND ISC AND 0BSD AND OFL-1.1
URL:            https://github.com/dennisklein/brig
# git archive of the release tag
Source0:        brig-%{version}.tar.gz
# the same tree's Go modules, vendored by make-srpm.sh
Source1:        brig-%{version}-vendor.tar.xz
Source2:        brig.cil

ExclusiveArch:  x86_64

# go.mod's minimum
BuildRequires:  golang >= 1.26.0
# the macros for the brig-selinux scriptlets
BuildRequires:  selinux-policy-devel

# What brig needs on the host: the "required" group of internal/deps, as
# `brig print-fedora-deps` lists it. packaging/spec_test.go keeps them equal.
Requires:       container-selinux
Requires:       cpio
Requires:       distribution-gpg-keys
Requires:       dosfstools
Requires:       e2fsprogs
Requires:       edk2-ovmf
Requires:       iproute
Requires:       libvirt-client
Requires:       libvirt-daemon-driver-qemu
Requires:       mkosi
Requires:       mtools
Requires:       nftables
Requires:       openssh-clients
Requires:       passt
Requires:       policycoreutils
Requires:       qemu-img
Requires:       qemu-kvm-core
Requires:       selinux-policy-targeted
Requires:       systemd-udev
Requires:       systemd-ukify
Requires:       zstd
# The host's CLI must match the OpenShell in the VMs, which comes from the
# same repository. It does not need brig, so there is no install-order problem.
Requires:       openshell
# The optional groups of internal/deps: --mount, brig image push, providers
# from a keyring. Recommends, so that hosts that do not use them can skip them.
Recommends:     libsecret
Recommends:     podman
Recommends:     virtiofsd
# passt under SELinux needs the module of brig-selinux (see below)
Requires:       (%{name}-selinux = %{version}-%{release} if selinux-policy-%{selinuxtype})

%description
brig manages the lifecycle of agent-sandbox VMs on a Fedora laptop. Each VM
runs headless Fedora with an NVIDIA OpenShell gateway, so autonomous agents are
contained by a VM boundary in addition to OpenShell's own sandboxing.

%package selinux
Summary:        SELinux policy module for brig
License:        Apache-2.0
BuildArch:      noarch
Requires:       selinux-policy-%{selinuxtype}
Requires:       passt-selinux
%{?selinux_requires}

%description selinux
Allows passt, which brig runs as root inside pasta's user namespace, to use the
setfcap capability there. Without it, the SELinux policy of passt 0^20261002
stops passt from sandboxing itself and no VM can start.

%prep
%autosetup -n brig-%{version} -a1

%build
# Offline: the vendored modules only, and the Go on the host.
export CGO_ENABLED=0 GOFLAGS=-mod=vendor GOPROXY=off GOTOOLCHAIN=local
export GOCACHE=%{_builddir}/go-cache GOPATH=%{_builddir}/go-path
# The flags of .goreleaser.yaml; -B gobuildid adds the build ID that rpm
# requires of ELF files (Fedora's %%gobuild passes a random one).
go build -trimpath \
    -ldflags "-s -w -B gobuildid -X github.com/dennisklein/brig/internal/version.version=v%{version}" \
    -o brig .

for shell in bash zsh fish; do
    ./brig completion "$shell" > "brig.$shell"
done

# The shell integration of brig use, for new bash, zsh and fish sessions.
./brig shell-init bash > shell-init.bash
sed -e '/^@SHELL_INIT@$/{' -e 'r shell-init.bash' -e 'd' -e '}' packaging/brig/profile.sh.in > brig.sh
./brig shell-init fish > brig.fish.conf

# The man pages come from the same command tree and help texts as the
# reference in docs/reference. rpmbuild sets SOURCE_DATE_EPOCH from the
# changelog, so they build the same every time.
go run ./internal/docgen man -version "v%{version}" man

# go-md2man and blackfriday, which render the man pages, are vendored but
# neither linked into brig nor shipped, so their licenses stay out.
find vendor -type f \( -iname 'LICENSE*' -o -iname 'NOTICE*' \) \
    ! -path 'vendor/github.com/cpuguy83/go-md2man/*' ! -path 'vendor/github.com/russross/blackfriday/*' |
LC_ALL=C sort |
while read -r f; do
    printf '==== %s ====\n\n' "$f"
    cat "$f"
    printf '\n'
done > LICENSE.dependencies

%install
install -Dpm 0755 brig %{buildroot}%{_bindir}/brig
install -Dpm 0644 brig.bash %{buildroot}%{bash_completions_dir}/brig
install -Dpm 0644 brig.zsh %{buildroot}%{zsh_completions_dir}/_brig
install -Dpm 0644 brig.fish %{buildroot}%{fish_completions_dir}/brig.fish
install -Dpm 0644 -t %{buildroot}%{_mandir}/man1 man/*.1
install -Dpm 0644 -t %{buildroot}%{_mandir}/man7 man/*.7
install -Dpm 0644 brig.sh %{buildroot}%{_sysconfdir}/profile.d/brig.sh
install -Dpm 0644 brig.fish.conf %{buildroot}%{fish_vendor_conf_dir}/brig.fish
install -Dpm 0644 %{SOURCE2} \
    %{buildroot}%{_datadir}/selinux/packages/%{selinuxtype}/brig.cil

%check
# Catches a build that lost the version flag. The tests run on the tag in
# brig's own CI.
test "$(./brig --version)" = "brig version v%{version}"
./brig print-fedora-deps > /dev/null
# Every command has a man page, and they name this version.
test -s man/brig.1
test -s man/brig-sandbox-create.1
grep -q 'brig v%{version}' man/brig.1
# The shell integration defines the function in interactive bash only.
test "$(bash -i -c '. ./brig.sh; type -t brig' 2>/dev/null)" = function
test -z "$(bash -c '. ./brig.sh; type -t brig' 2>/dev/null)"

# The module only allows a rule, it labels no files, so there is nothing to
# relabel.
%post selinux
%selinux_modules_install -s %{selinuxtype} %{_datadir}/selinux/packages/%{selinuxtype}/brig.cil

%postun selinux
if [ $1 -eq 0 ]; then
    %selinux_modules_uninstall -s %{selinuxtype} brig
fi

%files
%license LICENSE NOTICE LICENSE.dependencies
%doc README.md config.example.yaml
%{_bindir}/brig
%{_mandir}/man1/brig.1*
%{_mandir}/man1/brig-*.1*
%{_mandir}/man7/brig-concepts.7*
%{bash_completions_dir}/brig
%{zsh_completions_dir}/_brig
%{fish_completions_dir}/brig.fish
%config(noreplace) %{_sysconfdir}/profile.d/brig.sh
%{fish_vendor_conf_dir}/brig.fish

%files selinux
%license LICENSE NOTICE
%{_datadir}/selinux/packages/%{selinuxtype}/brig.cil

%changelog
* Sat Oct 10 2026 Dennis Klein <d.klein@gsi.de> - %{version}-%{baserelease}
- Initial package of brig %{version}
