// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package action turns the forms of brig ui's dialogs into the brig
// commands they run.
//
// Each Action's Build is the only place that makes a command's argument
// vector. The dialog shows the command's Display, and the runner executes
// its Argv, so the command shown is the command run. A browser sends field
// values, never a command line, and Build checks every one of them.
package action

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"

	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
)

// Form holds the fields of a dialog as the browser posted them.
type Form = url.Values

// Command is what an action runs.
type Command struct {
	// Argv is the command line, run as it is with no shell. Argv[0] is
	// "brig".
	Argv []string
	// Display is Argv quoted for a POSIX shell: what the dialog shows and
	// Copy copies.
	Display string
	// Notes say what the user should know before running the command, such
	// as when a change takes effect.
	Notes []string
}

// NewCommand returns the command with the arguments argv.
func NewCommand(argv ...string) Command {
	return Command{Argv: argv, Display: Quote(argv)}
}

// Note adds a note to c.
func (c Command) Note(format string, args ...any) Command {
	c.Notes = append(c.Notes[:len(c.Notes):len(c.Notes)], fmt.Sprintf(format, args...))
	return c
}

// Quote writes argv as a command line for a POSIX shell.
func Quote(argv []string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = quoteWord(a)
	}
	return strings.Join(words, " ")
}

func quoteWord(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("_@%+=:,./-", r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Context is what Build knows besides the form.
type Context struct {
	// Snap is what brig ui shows, which Build checks the form against.
	Snap store.Snapshot
	// Home is the user's home directory, which "~" in a path stands for.
	Home string
}

// Action is a change that a dialog makes by running one brig command.
type Action struct {
	// ID names the action in URLs and templates.
	ID string
	// Build checks the form and makes the command. Its error is shown to
	// the user, so it says what to change.
	Build func(f Form, c Context) (Command, error)
	// Subject returns the VM the command works on, if there is one. Its
	// other actions wait while the command runs.
	Subject func(f Form) string
	// Typed returns what the user must type to confirm the command, or
	// "" if none. The server checks it, whatever the browser did.
	Typed func(f Form) string
	// ShowOutput shows the output of the command also when it succeeds.
	ShowOutput bool
	// Next names the action to offer when the command succeeded.
	Next string
}

// ConfirmField is the field that holds the typed confirmation.
const ConfirmField = "confirm"

// Want returns what the user must type to confirm the command, or "".
func (a Action) Want(f Form) string {
	if a.Typed == nil {
		return ""
	}
	return a.Typed(f)
}

// Confirmed reports whether the form holds the confirmation that a requires,
// if it requires one.
func (a Action) Confirmed(f Form) bool {
	want := a.Want(f)
	return want == "" || strings.TrimSpace(f.Get(ConfirmField)) == want
}

// VM returns the VM the action's command works on for the form, or "".
func (a Action) VM(f Form) string {
	if a.Subject == nil {
		return ""
	}
	return a.Subject(f)
}

// Lookup returns the action with the ID id.
func Lookup(id string) (Action, bool) {
	a, ok := registry[id]
	return a, ok
}

// IDs returns the IDs of all actions.
func IDs() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	return ids
}

var registry = map[string]Action{}

// register adds the actions to the registry.
func register(as ...Action) {
	for _, a := range as {
		if _, dup := registry[a.ID]; dup {
			panic("action " + a.ID + " registered twice")
		}
		registry[a.ID] = a
	}
}

// subjectVM is the Subject of actions whose field "vm" names a VM.
func subjectVM(f Form) string { return f.Get("vm") }

// findVM returns the VM that the form's field "vm" names.
func findVM(f Form, c Context) (store.VM, error) {
	name := f.Get("vm")
	if err := vm.ValidateName(name); err != nil {
		return store.VM{}, err
	}
	v, ok := c.Snap.VM(name)
	if !ok {
		return v, fmt.Errorf("there is no VM %s", name)
	}
	return v, nil
}

// needRunning refuses a VM that does not run, naming what to do first.
func needRunning(v store.VM, what string) error {
	if !v.Running() {
		return fmt.Errorf("%s is %s; start it before %s", v.Name, v.State, what)
	}
	return nil
}

// plainText reports whether s holds only printable characters, which a path
// or a name needs to reach a command line, libvirt's XML and a VM record
// unchanged.
func plainText(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) || r == unicode.ReplacementChar })
}

// dirPath returns the absolute, clean path that field names in the form,
// with "~" standing for the home directory.
func dirPath(f Form, field, label string, c Context) (string, error) {
	p := strings.TrimSpace(f.Get(field))
	if p == "" {
		return "", fmt.Errorf("%s: say which directory", label)
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if c.Home == "" {
			return "", fmt.Errorf("%s: brig ui does not know your home directory; write the path in full", label)
		}
		p = path.Join(c.Home, p[1:])
	}
	switch {
	case !path.IsAbs(p):
		return "", fmt.Errorf("%s: write the path in full, starting with / or ~", label)
	case !plainText(p):
		return "", fmt.Errorf("%s: the path holds control characters", label)
	}
	return path.Clean(p), nil
}

// errNothing is the error of a form that changes nothing.
var errNothing = errors.New("nothing to change: the form holds the current settings")
