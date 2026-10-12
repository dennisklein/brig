// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/dennisklein/brig/internal/ui/store"
)

func init() {
	register(
		Action{ID: "sandbox-create", Build: buildSandboxCreate},
		Action{ID: "sandbox-start", Build: buildSandboxStart},
		Action{ID: "sandbox-stop", Build: buildSandboxStop},
		Action{ID: "sandbox-delete", Build: buildSandboxDelete, Typed: func(f Form) string { return f.Get("sandbox") }},
		Action{ID: "sandbox-attach", Build: buildSandboxAttach},
		Action{ID: "sandbox-detach", Build: buildSandboxDetach},
	)
}

// sandboxNameRE matches OpenShell sandbox names, as brig's openshell package
// does.
var sandboxNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxSandboxName is the longest sandbox name: OpenShell names containers
// after it.
const maxSandboxName = 63

// ValidSandboxName reports whether name can name an OpenShell sandbox.
func ValidSandboxName(name string) bool {
	return len(name) <= maxSandboxName && sandboxNameRE.MatchString(name)
}

// maxMounts is the most host directories a dialog offers to one sandbox.
const maxMounts = 8

// openshellCommand makes the openshell command for the gateway of the VM
// named vm. It always names the gateway: the user's shells may point at
// another one.
func openshellCommand(vm string, args ...string) Command {
	return NewCommand(append([]string{"openshell", "-g", "brig-" + vm}, args...)...)
}

// findGateway returns the gateway of the VM that the form's field "vm"
// names, if the gateway answers, which every change to a sandbox needs.
func findGateway(f Form, c Context, what string) (store.Gateway, error) {
	v, err := findVM(f, c)
	if err != nil {
		return store.Gateway{}, err
	}
	if err := needRunning(v, what); err != nil {
		return store.Gateway{}, err
	}
	g, ok := c.Snap.Gateway(v.Name)
	switch {
	case !ok || g.State == store.GatewayConnecting:
		return g, fmt.Errorf("brig ui has not reached %s's gateway yet; try again in a moment", v.Name)
	case g.State != store.GatewayReady:
		return g, fmt.Errorf("brig ui cannot reach %s: %s", g.Name(), g.Err)
	}
	return g, nil
}

// findSandbox returns the gateway and the sandbox that the form's fields
// "vm" and "sandbox" name.
func findSandbox(f Form, c Context, what string) (store.Gateway, store.Sandbox, error) {
	g, err := findGateway(f, c, what)
	if err != nil {
		return g, store.Sandbox{}, err
	}
	name := f.Get("sandbox")
	if !ValidSandboxName(name) {
		return g, store.Sandbox{}, fmt.Errorf("%q is no sandbox name", name)
	}
	s, ok := g.Sandbox(store.DefaultWorkspace, name)
	if !ok {
		return g, s, fmt.Errorf("%s has no sandbox %s", g.Name(), name)
	}
	return g, s, nil
}

func buildSandboxCreate(f Form, c Context) (Command, error) {
	g, err := findGateway(f, c, "creating a sandbox")
	if err != nil {
		return Command{}, err
	}
	name := strings.TrimSpace(f.Get("name"))
	switch {
	case name == "":
		return Command{}, errors.New("name: say what to call the sandbox")
	case !ValidSandboxName(name):
		return Command{}, fmt.Errorf("name: letters, digits, dots, dashes and underscores, starting with a letter or digit, at most %d characters", maxSandboxName)
	}
	if _, ok := g.Sandbox(store.DefaultWorkspace, name); ok {
		return Command{}, fmt.Errorf("%s has a sandbox %s already", g.Name(), name)
	}
	mounts, err := sandboxMounts(f, c)
	if err != nil {
		return Command{}, err
	}
	argv := []string{"brig", "sandbox", "create", g.VM}
	for _, m := range mounts {
		argv = append(argv, "--mount", m)
	}
	argv = append(argv, "--", "--name", name)
	template, image := f.Get("template"), strings.TrimSpace(f.Get("image"))
	switch {
	case template != "" && image != "":
		return Command{}, errors.New("a template brings its own image: pick one of them")
	case template != "":
		if !slices.ContainsFunc(g.Templates, func(t store.Template) bool { return t.Name == template }) {
			return Command{}, fmt.Errorf("%s has no sandbox template %q", g.Name(), template)
		}
		argv = append(argv, "--template", template)
	case image != "":
		if len(image) > 512 || strings.HasPrefix(image, "-") || !plainText(image) || strings.ContainsAny(image, " \t\\") {
			return Command{}, fmt.Errorf("image: %q is no image name", image)
		}
		argv = append(argv, "--from", image)
	}
	var notes []string
	for _, p := range f["provider"] {
		prov, ok := g.Provider(store.DefaultWorkspace, p)
		if !ok {
			return Command{}, fmt.Errorf("%s has no provider %q", g.Name(), p)
		}
		if slices.Contains(providersOf(argv), p) {
			continue
		}
		argv = append(argv, "--provider", p)
		notes = append(notes, reach(g, prov))
	}
	if policy := f.Get("policy"); policy != "" {
		if !slices.Contains(g.Config.Policies, policy) {
			return Command{}, fmt.Errorf("%q is no policy file of %s's config directories", policy, g.VM)
		}
		argv = append(argv, "--policy", policy)
	}
	argv = append(argv, "--detach", "--no-auto-providers")
	cmd := NewCommand(argv...)
	if g.Config.DefaultPolicy != "" && f.Get("policy") == "" {
		cmd = cmd.Note("The sandbox gets the default policy of %s's config directories, %s.", g.VM, g.Config.DefaultPolicy)
	}
	for _, n := range notes {
		cmd = cmd.Note("%s", n)
	}
	if len(mounts) > 0 {
		cmd = cmd.Note("brig adds a read-only sandbox mount of each host directory to %s if it has none, and the VM keeps it for later sandboxes.", g.VM)
	}
	return cmd.Note("The sandbox starts without a terminal. Connect to it from your own terminal once it runs."), nil
}

// providersOf returns the providers that argv attaches.
func providersOf(argv []string) []string {
	var out []string
	for i, a := range argv {
		if a == "--provider" && i+1 < len(argv) {
			out = append(out, argv[i+1])
		}
	}
	return out
}

// reach says what attaching the provider lets the sandbox reach.
func reach(g store.Gateway, p store.Provider) string {
	prof, ok := g.Profile(p.Workspace, p.Type)
	if !ok || len(prof.Endpoints) == 0 {
		return fmt.Sprintf("Provider %s opens no endpoint.", p.Name)
	}
	bins := "any program"
	if len(prof.Binaries) > 0 {
		bins = strings.Join(prof.Binaries, ", ")
	}
	return fmt.Sprintf("Provider %s opens %s for %s, and the sandbox sees placeholders for %s.",
		p.Name, strings.Join(prof.Endpoints, ", "), bins, strings.Join(p.Credentials, ", "))
}

// sandboxMounts reads the dialog's host directories, one DIR:TARGET per
// line, into the values of brig sandbox create's --mount.
func sandboxMounts(f Form, c Context) ([]string, error) {
	var out []string
	for _, line := range strings.Split(f.Get("mounts"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		src, target, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(target, ":") {
			return nil, fmt.Errorf("host directories: %q is not DIR:TARGET", line)
		}
		src, err := dirPath(Form{"d": {src}}, "d", "host directories", c)
		if err != nil {
			return nil, err
		}
		target = strings.TrimSpace(target)
		if !path.IsAbs(target) || path.Clean(target) != target || target == "/" {
			return nil, fmt.Errorf("host directories: the target %q must be a clean absolute path other than /", target)
		}
		out = append(out, src+":"+target)
	}
	if len(out) > maxMounts {
		return nil, fmt.Errorf("host directories: at most %d", maxMounts)
	}
	return out, nil
}

func buildSandboxStart(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "starting a sandbox")
	if err != nil {
		return Command{}, err
	}
	if s.Phase != "Stopped" && s.Phase != "Error" {
		return Command{}, fmt.Errorf("%s is %s; only a stopped sandbox, or one in error, starts", s.Name, strings.ToLower(s.Phase))
	}
	return openshellCommand(g.VM, "sandbox", "start", s.Name).
		Note("The workspace /sandbox is as you left it. Programs that ran before are gone; start your agent again from a terminal."), nil
}

func buildSandboxStop(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "stopping a sandbox")
	if err != nil {
		return Command{}, err
	}
	if s.Phase == "Stopped" || s.Phase == "Stopping" || s.Phase == "Deleting" {
		return Command{}, fmt.Errorf("%s is %s already", s.Name, strings.ToLower(s.Phase))
	}
	return openshellCommand(g.VM, "sandbox", "stop", s.Name).
		Note("Ends every program in the sandbox, the agent included. The workspace /sandbox stays."), nil
}

func buildSandboxDelete(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "deleting a sandbox")
	if err != nil {
		return Command{}, err
	}
	if s.Phase == "Deleting" {
		return Command{}, fmt.Errorf("%s is being deleted already", s.Name)
	}
	return openshellCommand(g.VM, "sandbox", "delete", s.Name), nil
}

func buildSandboxAttach(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "attaching a provider")
	if err != nil {
		return Command{}, err
	}
	p, ok := g.Provider(store.DefaultWorkspace, f.Get("provider"))
	switch {
	case !ok:
		return Command{}, fmt.Errorf("%s has no provider %q", g.Name(), f.Get("provider"))
	case slices.Contains(s.Providers, p.Name):
		return Command{}, fmt.Errorf("%s has %s attached already", s.Name, p.Name)
	case !s.Ready():
		return Command{}, fmt.Errorf("%s is %s; start it before attaching a provider", s.Name, strings.ToLower(s.Phase))
	}
	return openshellCommand(g.VM, "sandbox", "provider", "attach", s.Name, p.Name, "--wait").
		Note("%s", reach(g, p)).
		Note("Only programs that the sandbox starts afterwards see the placeholders: open a new shell, or stop and start the sandbox."), nil
}

func buildSandboxDetach(f Form, c Context) (Command, error) {
	g, s, err := findSandbox(f, c, "detaching a provider")
	if err != nil {
		return Command{}, err
	}
	p := f.Get("provider")
	switch {
	case !slices.Contains(s.Providers, p):
		return Command{}, fmt.Errorf("%s has no provider %q attached", s.Name, p)
	case !s.Ready():
		return Command{}, fmt.Errorf("%s is %s; start it before detaching a provider", s.Name, strings.ToLower(s.Phase))
	}
	return openshellCommand(g.VM, "sandbox", "provider", "detach", s.Name, p, "--wait").
		Note("The rules that %s added to the sandbox's policy go with it. Programs that run already keep the placeholders until they restart.", p), nil
}
