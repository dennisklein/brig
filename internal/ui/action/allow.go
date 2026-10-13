// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/dennisklein/brig/internal/policy"
	"github.com/dennisklein/brig/internal/ui/store"
)

func init() {
	register(
		Action{ID: "allow", Build: buildAllow, ShowOutput: true, Next: "policy-save-preview"},
		Action{ID: "reject", Build: buildReject},
		Action{ID: "policy-save-preview", Build: buildPolicySavePreview, ShowOutput: true, Next: "policy-save"},
		Action{ID: "policy-save", Build: buildPolicySave},
	)
}

// The modes of the allow dialog.
const (
	// AllowDraft approves the rule that OpenShell drafted.
	AllowDraft = "draft"
	// AllowRule adds a rule as the form describes it.
	AllowRule = "rule"
)

var (
	// ruleKeyRE matches a rule's key under network_policies.
	ruleKeyRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// fileNameRE matches the name of a policy file to create.
	fileNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// request returns the request that the form's field "req" names, if the
// store still holds it and it is the sandbox's.
func request(f Form, c Context, vm, workspace, sandbox string) (store.Request, bool) {
	seq, err := strconv.ParseUint(f.Get("req"), 10, 64)
	if err != nil {
		return store.Request{}, false
	}
	r, ok := c.Snap.TrafficOf(vm).Request(seq)
	return r, ok && r.Workspace == workspace && r.Sandbox == sandbox
}

func buildAllow(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "changing a sandbox's policy")
	if err != nil {
		return Command{}, err
	}
	if r, ok := request(f, c, g.VM, s.Workspace, s.Name); ok && r.CredentialMismatch() && f.Get("anyway") == "" {
		return Command{}, fmt.Errorf("%s may not carry the provider's credential, so the request would go out without it. "+
			"Add the host to the provider profile's endpoints instead, or tick Allow anyway", r.Host)
	}
	keep := f.Get("keep") != ""
	to := ""
	if keep {
		if to, err = policyFileArg(f, c); err != nil {
			return Command{}, err
		}
	}
	switch f.Get("mode") {
	case AllowDraft:
		id := f.Get("draft")
		d, ok := c.Snap.TrafficOf(g.VM).Draft(s.Workspace, s.Name, id)
		if !ok {
			return Command{}, fmt.Errorf("OpenShell has no pending draft %q for %s; it may have been approved or rejected meanwhile", id, s.Name)
		}
		var cmd Command
		if keep {
			if d.Rule == "" {
				return Command{}, errors.New("OpenShell's draft names no rule to keep")
			}
			argv := []string{"brig", "policy", "allow", g.VM, s.Name, "--workspace", s.Workspace, "--draft", d.ID, "--rule", d.Rule, "--save"}
			cmd = NewCommand(withTo(argv, to)...).
				Note("Approves the rule %s as OpenShell drafted it, shown above, into the policy of %s, which loads it at once. Then it writes the rule into the sandbox's policy file, so that sandboxes created from the file get it too.", d.Rule, s.Name).
				Note(keepNote)
			cmd.Done = true
		} else {
			cmd = NewCommand("brig", "policy", "allow", g.VM, s.Name, "--workspace", s.Workspace, "--draft", d.ID).
				Note("Approves the rule %s as OpenShell drafted it, shown above, into the policy of %s, which loads it at once. Other sandboxes do not get it.", d.Rule, s.Name)
			cmd.NextForm = []string{"vm", g.VM, "workspace", s.Workspace, "sandbox", s.Name, "rule", d.Rule}
		}
		if d.Security != "" {
			cmd = cmd.Note("OpenShell's security notes: %s", d.Security)
		}
		if d.Error != "" {
			cmd = cmd.Note("The gateway could not apply this draft before: %s", d.Error)
		}
		return cmd, nil
	case AllowRule:
	default:
		return Command{}, errors.New("choose between OpenShell's draft and a rule of your own")
	}

	host, port, err := endpoint(f)
	if err != nil {
		return Command{}, err
	}
	binary := strings.TrimSpace(f.Get("binary"))
	switch {
	case binary == "":
		return Command{}, fieldErr("Binary", "say which program may connect, by its full path")
	case !path.IsAbs(binary) || !plainText(binary):
		return Command{}, fieldErr("Binary", "write the program's full path, such as /usr/bin/node")
	}
	rule := strings.TrimSpace(f.Get("rule"))
	if rule == "" {
		rule = policy.RuleName(host, port)
	}
	switch {
	case !ruleKeyRE.MatchString(rule):
		return Command{}, fieldErr("Rule name", "use letters, digits, _, . and - only")
	case strings.HasPrefix(rule, "_provider_"):
		return Command{}, fieldErr("Rule name", "names starting with _provider_ belong to the rules of providers")
	}
	protocol, access, enforcement := f.Get("protocol"), f.Get("access"), f.Get("enforcement")
	switch protocol {
	case "rest":
		if access != "read-only" && access != "read-write" && access != "full" {
			return Command{}, fieldErr("Access", "choose read-only, read-write or full")
		}
		if enforcement != "enforce" && enforcement != "audit" {
			return Command{}, fieldErr("Mode", "choose enforce or audit")
		}
	case "":
	default:
		return Command{}, fieldErr("Inspect", "choose HTTP requests or the whole connection")
	}
	spec := policy.EndpointSpec(host, port, protocol, access, enforcement)
	broad := policy.Broad(protocol, access, binary)
	if len(broad) > 0 && f.Get("broad") == "" {
		return Command{}, fieldErr("Broad rule", strings.Join(broad, "; ")+". Tick Allow a broad rule to add it anyway")
	}
	argv := []string{"brig", "policy", "allow", g.VM, s.Name, host + ":" + strconv.Itoa(port), "--workspace", s.Workspace, "--binary", binary, "--rule", rule}
	if protocol == "" {
		argv = append(argv, "--l4")
	} else {
		argv = append(argv, "--access", access)
		if enforcement == "audit" {
			argv = append(argv, "--audit")
		}
	}
	if len(broad) > 0 {
		argv = append(argv, "--broad")
	}
	var cmd Command
	if keep {
		cmd = NewCommand(withTo(append(argv, "--save"), to)...).
			Note("Adds %s to the rule %s of %s, or creates the rule, and waits until the sandbox loads it. Then it writes the rule into the sandbox's policy file, so that sandboxes created from the file get it too.", spec, rule, s.Name).
			Note(keepNote)
		cmd.Done = true
	} else {
		cmd = NewCommand(argv...).
			Note("Adds %s to the rule %s of %s, or creates the rule, and waits until the sandbox loads it. Other sandboxes do not get it.", spec, rule, s.Name)
		cmd.NextForm = []string{"vm", g.VM, "workspace", s.Workspace, "sandbox", s.Name, "rule", rule}
	}
	cmd = cmd.Note("The policy revision records that brig policy allow made it, which brig audit shows.")
	if protocol == "" {
		cmd = cmd.Note("The whole connection is allowed: the proxy does not look at the HTTP requests in it.")
	}
	if enforcement == "audit" {
		cmd = cmd.Note("Audit logs the requests that the access level would deny, and lets them pass.")
	}
	return cmd, nil
}

// keepNote says what keeping a rule does not do.
const keepNote = "brig neither applies the file to other sandboxes nor commits it. A sandbox created from the default policy needs a File of its own."

// withTo adds --to to the arguments of a brig policy command, if to is set.
func withTo(argv []string, to string) []string {
	if to == "" {
		return argv
	}
	return append(argv, "--to", to)
}

// policyFileArg returns the policy file that the form's field "to" names, or
// "" for the sandbox's own: a name in the config directories, or a path.
func policyFileArg(f Form, c Context) (string, error) {
	to := strings.TrimSpace(f.Get("to"))
	switch {
	case to == "":
	case strings.HasPrefix(to, "/") || strings.HasPrefix(to, "~"):
		return dirPath(f, "to", "File", c)
	case !fileNameRE.MatchString(to):
		return "", fieldErr("File", "write a name such as webapp, or a full path")
	}
	return to, nil
}

// buildReject makes the command that rejects the draft that the form's field
// "draft" names, with the reason that the form gives, if it does.
func buildReject(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "rejecting a draft")
	if err != nil {
		return Command{}, err
	}
	id := f.Get("draft")
	d, ok := c.Snap.TrafficOf(g.VM).Draft(s.Workspace, s.Name, id)
	if !ok {
		return Command{}, fmt.Errorf("OpenShell has no pending draft %q for %s; it may have been approved or rejected meanwhile", id, s.Name)
	}
	args := []string{"rule", "reject", s.Name, "--chunk-id", d.ID}
	if reason := strings.TrimSpace(f.Get("reason")); reason != "" {
		if len(reason) > maxReason || !plainText(reason) {
			return Command{}, fieldErr("Reason", fmt.Sprintf("write one line of at most %d characters", maxReason))
		}
		args = append(args, "--reason", reason)
	}
	return openshellCommand(g.VM, s.Workspace, args...).
		Note("Rejects the rule %s that OpenShell drafted for %s, so that it leaves the pending drafts. The sandbox's policy stays as it is, and its requests are denied as before; OpenShell may draft a rule again if the denials go on.", d.Rule, s.Name), nil
}

// maxReason is the longest reason for rejecting a draft, in bytes.
const maxReason = 200

// fieldErr says what is wrong with the dialog's field label.
func fieldErr(label, msg string) error { return fmt.Errorf("%s: %s", label, msg) }

// endpoint returns the host and port of the form's fields "host" and
// "port".
func endpoint(f Form) (string, int, error) {
	host := strings.ToLower(strings.TrimSpace(f.Get("host")))
	if err := policy.CheckHost(host); err != nil {
		return "", 0, fieldErr("Host", err.Error())
	}
	port, err := strconv.Atoi(strings.TrimSpace(f.Get("port")))
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fieldErr("Port", "write a number from 1 to 65535")
	}
	return host, port, nil
}

// policySave returns the brig policy save command of the form.
func policySave(f Form, c Context, dryRun bool) (Command, error) {
	g, s, err := findSandbox(f, c, "saving a sandbox's rules")
	if err != nil {
		return Command{}, err
	}
	argv := []string{"brig", "policy", "save", g.VM, s.Name, "--workspace", s.Workspace}
	next := []string{"vm", g.VM, "workspace", s.Workspace, "sandbox", s.Name}
	if rule := strings.TrimSpace(f.Get("rule")); rule != "" {
		if !ruleKeyRE.MatchString(rule) {
			return Command{}, fieldErr("Rule", "use letters, digits, _, . and - only")
		}
		argv = append(argv, "--rule", rule)
		next = append(next, "rule", rule)
	}
	to, err := policyFileArg(f, c)
	if err != nil {
		return Command{}, err
	}
	if to != "" {
		argv = append(argv, "--to", to)
		next = append(next, "to", to)
	}
	if dryRun {
		argv = append(argv, "--dry-run")
	}
	cmd := NewCommand(argv...)
	cmd.NextForm = next
	return cmd, nil
}

func buildPolicySavePreview(f Form, c Context) (Command, error) {
	cmd, err := policySave(f, c, true)
	if err != nil {
		return cmd, err
	}
	return cmd.Note("Changes nothing. The output is the change to the policy file, which saving makes."), nil
}

func buildPolicySave(f Form, c Context) (Command, error) {
	cmd, err := policySave(f, c, false)
	if err != nil {
		return cmd, err
	}
	cmd.NextForm = nil
	return cmd.Note("Edits the policy file's text; comments and other rules stay. brig neither applies the file to other sandboxes nor commits it."), nil
}
