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
		Action{ID: "allow", Build: buildAllow, Next: "policy-save-preview"},
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
	switch f.Get("mode") {
	case AllowDraft:
		id := f.Get("draft")
		d, ok := c.Snap.TrafficOf(g.VM).Draft(s.Workspace, s.Name, id)
		if !ok {
			return Command{}, fmt.Errorf("OpenShell has no pending draft %q for %s; it may have been approved or rejected meanwhile", id, s.Name)
		}
		cmd := openshellCommand(g.VM, s.Workspace, "rule", "approve", s.Name, "--chunk-id", d.ID).
			Note("Approves the rule %s as OpenShell drafted it, shown above, into the policy of %s, which loads it at once. Other sandboxes do not get it.", d.Rule, s.Name)
		if d.Security != "" {
			cmd = cmd.Note("OpenShell's security notes: %s", d.Security)
		}
		if d.Error != "" {
			cmd = cmd.Note("The gateway could not apply this draft before: %s", d.Error)
		}
		cmd.NextForm = []string{"vm", g.VM, "workspace", s.Workspace, "sandbox", s.Name, "rule", d.Rule}
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
	if why := policy.Broad(protocol, access, binary); len(why) > 0 && f.Get("broad") == "" {
		return Command{}, fieldErr("Broad rule", strings.Join(why, "; ")+". Tick Allow a broad rule to add it anyway")
	}
	cmd := openshellCommand(g.VM, s.Workspace, "policy", "update", s.Name,
		"--rule-name", rule, "--binary", binary, "--add-endpoint", spec, "--wait").
		Note("Adds %s to the rule %s of %s, or creates the rule, and waits until the sandbox loads it. Other sandboxes do not get it.", spec, rule, s.Name)
	if protocol == "" {
		cmd = cmd.Note("The whole connection is allowed: the proxy does not look at the HTTP requests in it.")
	}
	if enforcement == "audit" {
		cmd = cmd.Note("Audit logs the requests that the access level would deny, and lets them pass.")
	}
	cmd.NextForm = []string{"vm", g.VM, "workspace", s.Workspace, "sandbox", s.Name, "rule", rule}
	return cmd, nil
}

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
	if to := strings.TrimSpace(f.Get("to")); to != "" {
		if strings.HasPrefix(to, "/") || strings.HasPrefix(to, "~") {
			if to, err = dirPath(f, "to", "File", c); err != nil {
				return Command{}, err
			}
		} else if !fileNameRE.MatchString(to) {
			return Command{}, fieldErr("File", "write a name such as webapp, or a full path")
		}
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
