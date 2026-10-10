// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package doctor

import (
	"regexp"
	"strings"
)

// missingProgramRE matches the errors of vmnet and of the net-helper about
// a program they cannot run. The helper also tries /usr/sbin.
var missingProgramRE = regexp.MustCompile(`\(package ([a-z0-9-]+)\)|exec: "(?:/usr/sbin/)?([a-z-]+)": `)

// programPackages are the Fedora packages of the programs that a VM network
// runs.
var programPackages = map[string]string{
	"nft": "nftables", "passt": "passt", "pasta": "passt", "ip": "iproute",
	"systemd-run": "systemd", "systemctl": "systemd",
}

// classifyNetwork says what likely makes the network probe fail, from the
// error of vmnet.Probe, which includes the log of the network's unit, and
// how to fix it. The cause is empty when nothing is recognized. The
// substrings are those of passt, pasta, nft and systemd, which brig does not
// control, so each matches little.
func classifyNetwork(err error) (cause, hint string) {
	msg := err.Error()
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	if m := missingProgramRE.FindStringSubmatch(msg); m != nil {
		pkg := m[1]
		if pkg == "" {
			pkg = programPackages[m[2]]
		}
		if pkg != "" {
			return "a program is missing", "sudo dnf install " + pkg
		}
	}
	switch {
	case has("Failed to connect to", "No medium found", "XDG_RUNTIME_DIR"):
		return "no systemd user manager", sessionHint
	case has("unrecognized option", "unrecognised option", "unknown option"):
		return "passt is too old", "sudo dnf upgrade passt\n" +
			"brig runs passt with --vhost-user; passt --help lists it when this passt supports it."
	case has("loading the firewall"):
		return classifyFirewall(has)
	case has("uid_map", "gid_map", "exited after it created"):
		// passt binds its socket and then sandboxes itself in a user
		// namespace, which SELinux may deny it.
		return "SELinux probably denies passt", "sudo dnf install brig-selinux, which allows passt to sandbox itself in pasta's user namespace\n" +
			"To confirm the denial, look for { setfcap } and tclass=cap_userns in: sudo ausearch -m AVC -c passt\n" +
			"Without that package, see Troubleshooting in brig's README for a local SELinux module."
	case has("routable interface", "No interface"):
		return "pasta finds no network", "connect the host to a network with a default route; ip route shows the routes pasta copies into the VM's network"
	case has("user namespace", "Operation not permitted", "Permission denied"):
		return "pasta cannot set up its namespace", "check that user namespaces are enabled: sysctl user.max_user_namespaces (0 disables them; see the user namespaces check above)\n" +
			"If SELinux denies pasta, sudo ausearch -m AVC -c pasta shows it."
	case has("context deadline exceeded", "did not create"):
		return "timed out", "run brig doctor again, since a loaded host can start slowly; the log below shows how far pasta and passt got\n" +
			"If it persists, check sudo ausearch -m AVC -ts recent for SELinux denials."
	case has("exited before it created"):
		return "the network exited early", "read the log below for the reason; sudo ausearch -m AVC -ts recent shows SELinux denials, which also end pasta and passt silently"
	}
	return "", "read the error below; sudo ausearch -m AVC -ts recent shows SELinux denials, and Troubleshooting in brig's README lists known causes"
}

// classifyFirewall classifies a failure of the net-helper to load the
// nftables rules, which brig's ruleset needs nf_tables, nft_ct and
// nft_connlimit for.
func classifyFirewall(has func(...string) bool) (cause, hint string) {
	switch {
	case has("syntax error", "unexpected"):
		return "nft rejects brig's rules", "sudo dnf upgrade nftables, in case this nft is older than the ruleset expects; nft --version shows the version\n" +
			"If it is current, report the error below as a brig bug."
	case has("Operation not permitted", "Permission denied"):
		return "nft is not allowed to load rules", "check for SELinux denials: sudo ausearch -m AVC -ts recent\n" +
			"nft needs CAP_NET_ADMIN in pasta's namespace, which a policy that confines brig may withhold."
	case has("Operation not supported", "Protocol not supported", "Could not process rule", "No such file or directory"):
		return "the kernel lacks nftables support", "load the kernel's nftables modules: for m in nf_tables nft_ct nft_connlimit; do sudo modprobe $m; done\n" +
			"A kernel built without them, such as some minimal or container kernels, cannot run brig's VM networks."
	}
	return "nft fails", "sudo dnf reinstall nftables, and check nft --version; the error below says what nft rejected"
}
