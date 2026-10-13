// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package action_test

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/ui/demo"
	"github.com/dennisklein/brig/internal/ui/store"
)

// demoContext is the demo host: dev and review run, airgap is shut off and
// on an older image.
func demoContext() action.Context {
	now := time.Now()
	imgs := demo.Images(now)
	return action.Context{Home: demo.Home, Snap: store.Snapshot{
		VMs: demo.VMs(now), Images: imgs, NewestImage: &imgs[0],
		Profiles: demo.Profiles(), Config: demo.Config(), Gateways: demo.Gateways(now),
	}}
}

func form(kv ...string) action.Form {
	f := url.Values{}
	for c := range slices.Chunk(kv, 2) {
		f.Add(c[0], c[1])
	}
	return f
}

func TestBuild(t *testing.T) {
	for _, c := range []struct {
		name string
		id   string
		f    action.Form
		want string // the command, or "" if Build must refuse
		err  string // part of the refusal
	}{
		{"start", "start", form("vm", "airgap"), "brig start airgap", ""},
		{"start running", "start", form("vm", "dev"), "", "running already"},
		{"start unknown", "start", form("vm", "nope"), "", "no VM nope"},
		{"start bad name", "start", form("vm", "../x"), "", "invalid VM name"},
		{"start flag as name", "start", form("vm", "--help"), "", "invalid VM name"},
		{"stop", "stop", form("vm", "dev"), "brig stop dev", ""},
		{"stop force", "stop", form("vm", "dev", "force", "on"), "brig stop dev --force", ""},
		{"stop shut off", "stop", form("vm", "airgap"), "", "shut off already"},
		{"delete running", "delete", form("vm", "dev"), "brig delete dev --force", ""},
		{"delete shut off", "delete", form("vm", "airgap"), "brig delete airgap", ""},
		{"upgrade", "upgrade", form("vm", "review"), "", "newest base image already"},
		{"profile", "profile", form("vm", "dev", "profile", "isolated"), "brig update dev --profile isolated", ""},
		{"profile same", "profile", form("vm", "dev", "profile", "ollama"), "", "nothing to change"},
		{"profile unknown", "profile", form("vm", "dev", "profile", "-x"), "", "no network profile"},
		{
			"resources", "resources", form("vm", "dev", "cpus", "8", "memory", "16GiB", "data-disk", "80GiB"),
			"brig update dev --cpus 8 --memory 16GiB --data-disk 80GiB", "",
		},
		{"resources unchanged", "resources", form("vm", "dev", "cpus", "4", "memory", "8GiB", "data-disk", "40GiB"), "", "nothing to change"},
		{"resources shrink", "resources", form("vm", "dev", "data-disk", "10GiB"), "", "only grow"},
		{"resources bad cpus", "resources", form("vm", "dev", "cpus", "many"), "", "CPUs"},
		{"resources bad memory", "resources", form("vm", "dev", "memory", "lots"), "", "Memory"},
		{
			"mount", "mount-add", form("vm", "dev", "src", "~/src/notes"),
			"brig update dev --add-mount /home/dklein/src/notes", "",
		},
		{
			"mount rw target", "mount-add", form("vm", "dev", "src", "/data/x/", "target", "/work/x", "access", "rw"),
			"brig update dev --add-mount /data/x:/work/x:rw", "",
		},
		{
			"mount sandbox wins over rw", "mount-add", form("vm", "dev", "src", "/data/x", "access", "rw", "sandbox", "on"),
			"brig update dev --add-mount /data/x::sandbox", "",
		},
		{
			"mount spaces", "mount-add", form("vm", "dev", "src", "/data/my notes", "target", "/mnt/notes"),
			"brig update dev --add-mount '/data/my notes:/mnt/notes'", "",
		},
		{"mount spaces in the default target", "mount-add", form("vm", "dev", "src", "/data/my notes"), "", "whitespace"},
		{"mount relative", "mount-add", form("vm", "dev", "src", "notes"), "", "in full"},
		{"mount colon", "mount-add", form("vm", "dev", "src", "/data/a:b"), "", "colon"},
		{"mount bad target", "mount-add", form("vm", "dev", "src", "/data/x", "target", "work"), "", "absolute path"},
		{"mount taken", "mount-add", form("vm", "dev", "src", "/data/x", "target", "/mnt/pi-kit"), "", "already shares"},
		{"mount empty", "mount-add", form("vm", "dev"), "", "say which directory"},
		{"unmount", "mount-rm", form("vm", "dev", "target", "/mnt/pi-kit"), "brig update dev --remove-mount /mnt/pi-kit", ""},
		{"unmount unknown", "mount-rm", form("vm", "dev", "target", "/mnt/other"), "", "shares nothing"},
		{
			"config add", "config-add", form("vm", "dev", "dir", "~/src/work-openshell/"),
			"brig update dev --add-openshell-config /home/dklein/src/work-openshell", "",
		},
		{"config add again", "config-add", form("vm", "dev", "dir", "~/src/agent-openshell"), "", "already"},
		{
			"config remove", "config-rm", form("vm", "dev", "dir", "/home/dklein/src/agent-openshell"),
			"brig update dev --remove-openshell-config /home/dklein/src/agent-openshell", "",
		},
		{"config remove unknown", "config-rm", form("vm", "dev", "dir", "/elsewhere"), "", "does not apply"},
		{
			"create defaults", "create", form("name", "scratch", "profile", "default", "cpus", "4", "memory", "8GiB", "configs", "on", "start", "on"),
			"brig create scratch", "",
		},
		{
			"create changes", "create", form("name", "scratch", "profile", "isolated", "cpus", "2", "memory", "4GiB"),
			"brig create scratch --cpus 2 --memory 4GiB --profile isolated --no-openshell-config --no-start", "",
		},
		{"create exists", "create", form("name", "dev"), "", "there is a VM dev"},
		{"create bad name", "create", form("name", "Dev"), "", "invalid VM name"},
		{"create bad profile", "create", form("name", "x", "profile", "nope"), "", "no network profile"},
		{"sync preview", "sync-preview", form("vm", "dev"), "brig sync dev --dry-run", ""},
		{"sync preview without dirs", "sync-preview", form("vm", "review", "vm", "review"), "brig sync review --dry-run", ""},
		{"sync stopped", "sync", form("vm", "airgap"), "", "start it before syncing"},
		{"sync", "sync", form("vm", "dev"), "brig sync dev", ""},
		{"sync workspace", "sync", form("vm", "dev", "workspace", "team"), "brig sync dev --workspace team", ""},
		{"sync preview workspace", "sync-preview", form("vm", "dev", "workspace", "team"), "brig sync dev --workspace team --dry-run", ""},
		{"sync unknown workspace", "sync", form("vm", "dev", "workspace", "ops"), "", "define nothing for the workspace"},
		{"sync prune refresh", "sync", form("vm", "dev", "prune", "on", "refresh", "on"), "brig sync dev --prune --refresh-secrets", ""},
		{"image build", "image-build", form(), "brig image build", ""},
		{"image build options", "image-build", form("fedora", "43", "openshell", "0.1.1"), "brig image build --fedora 43 --openshell 0.1.1", ""},
		{"image build default release", "image-build", form("fedora", "44"), "brig image build", ""},
		{"image build bad openshell", "image-build", form("openshell", "--help"), "", "OpenShell"},
		{"image build bad fedora", "image-build", form("fedora", "x"), "", "fedora"},
		{"image prune", "image-prune", form(), "brig image prune", ""},
		{"image push", "image-push", form("vm", "dev", "image", "localhost/pi-agent:latest"), "brig image push dev localhost/pi-agent:latest", ""},
		{"image push option", "image-push", form("vm", "dev", "image", "--all"), "", "no image name"},
		{"image push space", "image-push", form("vm", "dev", "image", "a b"), "", "no image name"},
		{"image push stopped", "image-push", form("vm", "airgap", "image", "x"), "", "before pushing"},
		{"image push empty", "image-push", form("vm", "dev"), "", "name an image"},
		{
			"sandbox create", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "scratch"),
			"brig sandbox create dev --workspace default -- --name scratch --detach --no-auto-providers", "",
		},
		{
			"sandbox create everything", "sandbox-create", form(
				"vm", "dev", "workspace", "default", "name", "kit2", "template", "pi", "provider", "llm", "provider", "github", "provider", "llm",
				"policy", demo.Home+"/src/agent-openshell/policies/tools.yaml",
				"mounts", "~/src/pi-kit:/usr/local/share/pi-kit\n\n/data/my notes:/mnt/notes\n",
			),
			"brig sandbox create dev --workspace default --mount /home/dklein/src/pi-kit:/usr/local/share/pi-kit --mount '/data/my notes:/mnt/notes' -- " +
				"--name kit2 --template pi --provider llm --provider github --policy /home/dklein/src/agent-openshell/policies/tools.yaml --detach --no-auto-providers", "",
		},
		{"sandbox create image", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "image", "localhost/pi-agent:2026-10-08"), "brig sandbox create dev --workspace default -- --name x --from localhost/pi-agent:2026-10-08 --detach --no-auto-providers", ""},
		{"sandbox create image option", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "image", "--privileged"), "", "no image name"},
		{"sandbox create image and template", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "image", "a", "template", "pi"), "", "pick one"},
		{"sandbox create unknown template", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "template", "nope"), "", "no sandbox template"},
		{"sandbox create unknown provider", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "provider", "nope"), "", "no provider"},
		{"sandbox create provider as option", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "provider", "--help"), "", "no provider"},
		{"sandbox create policy elsewhere", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "policy", "/etc/shadow"), "", "no policy file"},
		{"sandbox create no name", "sandbox-create", form("vm", "dev"), "", "say what to call"},
		{"sandbox create option as name", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "--help"), "", "name:"},
		{"sandbox create long name", "sandbox-create", form("vm", "dev", "workspace", "default", "name", strings.Repeat("a", 64)), "", "at most 63"},
		{"sandbox create taken", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "webapp"), "", "has a sandbox webapp in workspace default already"},
		{"sandbox create stopped VM", "sandbox-create", form("vm", "airgap", "workspace", "default", "name", "x"), "", "before creating a sandbox"},
		{"sandbox create mount without target", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "mounts", "/data/x"), "", "DIR:TARGET"},
		{"sandbox create mount relative target", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "mounts", "/data/x:work"), "", "clean absolute"},
		{"sandbox create mount root target", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "mounts", "/data/x:/"), "", "clean absolute"},
		{"sandbox create mount relative source", "sandbox-create", form("vm", "dev", "workspace", "default", "name", "x", "mounts", "data:/x"), "", "in full"},
		{
			"sandbox create in the other workspace", "sandbox-create", form("vm", "dev", "workspace", "team", "name", "scratch", "provider", "github"),
			"brig sandbox create dev --workspace team -- --name scratch --provider github --detach --no-auto-providers", "",
		},
		{
			"sandbox create name taken in another workspace only", "sandbox-create", form("vm", "dev", "workspace", "team", "name", "kit-dev"),
			"brig sandbox create dev --workspace team -- --name kit-dev --detach --no-auto-providers", "",
		},
		{"sandbox create template of another workspace", "sandbox-create", form("vm", "dev", "workspace", "team", "name", "x", "template", "claude"), "", "no sandbox template"},
		{"sandbox create provider of another workspace", "sandbox-create", form("vm", "dev", "workspace", "team", "name", "x", "provider", "claude-code"), "", "no provider"},
		{"sandbox create without a workspace", "sandbox-create", form("vm", "dev", "name", "scratch"), "", "has no workspace"},
		{"sandbox create in an unknown workspace", "sandbox-create", form("vm", "dev", "workspace", "nope", "name", "scratch"), "", "has no workspace"},
		{"sandbox create option as workspace", "sandbox-create", form("vm", "dev", "workspace", "--help", "name", "scratch"), "", "has no workspace"},
		{"sandbox create with one workspace", "sandbox-create", form("vm", "review", "name", "scratch"), "brig sandbox create review --workspace default -- --name scratch --detach --no-auto-providers", ""},
		{"sandbox start", "sandbox-start", form("vm", "dev", "workspace", "default", "sandbox", "tools"), "openshell -g brig-dev --workspace default sandbox start tools", ""},
		{"sandbox start running", "sandbox-start", form("vm", "dev", "workspace", "default", "sandbox", "webapp"), "", "only a stopped sandbox"},
		{"sandbox start unknown", "sandbox-start", form("vm", "dev", "workspace", "default", "sandbox", "nope"), "", "no sandbox nope"},
		{"sandbox start stopped VM", "sandbox-start", form("vm", "airgap", "workspace", "default", "sandbox", "corpus-scan"), "", "start it before"},
		{"sandbox start option", "sandbox-start", form("vm", "dev", "workspace", "default", "sandbox", "--all"), "", "no sandbox name"},
		{"sandbox start without a workspace", "sandbox-start", form("vm", "dev", "sandbox", "tools"), "", "no workspace name"},
		{"sandbox start in an unknown workspace", "sandbox-start", form("vm", "dev", "workspace", "nope", "sandbox", "tools"), "", "no sandbox tools in workspace nope"},
		{"sandbox start option as workspace", "sandbox-start", form("vm", "dev", "workspace", "--all", "sandbox", "tools"), "", "no workspace name"},
		{"sandbox stop in the other workspace", "sandbox-stop", form("vm", "dev", "workspace", "team", "sandbox", "webapp"), "openshell -g brig-dev --workspace team sandbox stop webapp", ""},
		{"sandbox stop a sandbox of the other workspace", "sandbox-stop", form("vm", "dev", "workspace", "team", "sandbox", "kit-dev"), "", "no sandbox kit-dev in workspace team"},
		{"sandbox attach in the other workspace", "sandbox-attach", form("vm", "dev", "workspace", "team", "sandbox", "webapp", "provider", "github"), "openshell -g brig-dev --workspace team sandbox provider attach webapp github --wait", ""},
		{"sandbox attach a provider of another workspace", "sandbox-attach", form("vm", "dev", "workspace", "team", "sandbox", "webapp", "provider", "claude-code"), "", "no provider"},
		{"sandbox stop", "sandbox-stop", form("vm", "dev", "workspace", "default", "sandbox", "webapp"), "openshell -g brig-dev --workspace default sandbox stop webapp", ""},
		{"sandbox stop stopped", "sandbox-stop", form("vm", "dev", "workspace", "default", "sandbox", "tools"), "", "stopped already"},
		{"sandbox delete", "sandbox-delete", form("vm", "dev", "workspace", "default", "sandbox", "kit-dev"), "openshell -g brig-dev --workspace default sandbox delete kit-dev", ""},
		{"sandbox delete on another VM", "sandbox-delete", form("vm", "review", "workspace", "default", "sandbox", "kit-dev"), "", "no sandbox kit-dev"},
		{"sandbox attach", "sandbox-attach", form("vm", "dev", "workspace", "default", "sandbox", "kit-dev", "provider", "github"), "openshell -g brig-dev --workspace default sandbox provider attach kit-dev github --wait", ""},
		{"sandbox attach attached", "sandbox-attach", form("vm", "dev", "workspace", "default", "sandbox", "kit-dev", "provider", "llm"), "", "attached already"},
		{"sandbox attach unknown", "sandbox-attach", form("vm", "dev", "workspace", "default", "sandbox", "kit-dev", "provider", "nope"), "", "no provider"},
		{"sandbox attach stopped", "sandbox-attach", form("vm", "dev", "workspace", "default", "sandbox", "tools", "provider", "github"), "", "start it before attaching"},
		{"sandbox detach", "sandbox-detach", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "provider", "github"), "openshell -g brig-dev --workspace default sandbox provider detach webapp github --wait", ""},
		{"sandbox detach not attached", "sandbox-detach", form("vm", "dev", "workspace", "default", "sandbox", "webapp", "provider", "claude-code"), "", "attached"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, ok := action.Lookup(c.id)
			if !ok {
				t.Fatalf("no action %s", c.id)
			}
			cmd, err := a.Build(c.f, demoContext())
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("got %q and error %v, want an error with %q", cmd.Display, err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Display != c.want {
				t.Errorf("display %q, want %q", cmd.Display, c.want)
			}
			if cmd.Argv[0] != "brig" && cmd.Argv[0] != "openshell" {
				t.Errorf("argv %q does not start with brig or openshell", cmd.Argv)
			}
		})
	}
}

// What the dialog shows must be what runs: Display is Argv, quoted for a
// shell, so that pasting it into one gives the same words.
func TestDisplayQuotesArgv(t *testing.T) {
	for argv, want := range map[string]string{
		"brig start dev":                  "brig start dev",
		"brig|update|dev|a b":             "brig update dev 'a b'",
		"brig|x|it's":                     `brig x 'it'\''s'`,
		"brig|x|$(rm -rf ~)":              "brig x '$(rm -rf ~)'",
		"brig|x|;|&|a*b":                  "brig x ';' '&' 'a*b'",
		"brig|x||é":                       "brig x '' 'é'",
		"brig|--add-mount|/a/b:/c:rw,x=1": "brig --add-mount /a/b:/c:rw,x=1",
	} {
		sep := " "
		if strings.Contains(argv, "|") {
			sep = "|"
		}
		if got := action.Quote(strings.Split(argv, sep)); got != want {
			t.Errorf("%q: %q, want %q", argv, got, want)
		}
	}
}

func TestUpgradeAcrossMinorVersions(t *testing.T) {
	c := demoContext()
	img := *c.Snap.NewestImage
	img.OpenShell = "0.2.0"
	c.Snap.NewestImage = &img
	up, _ := action.Lookup("upgrade")
	cmd, err := up.Build(form("vm", "airgap"), c)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Display != "brig upgrade airgap --force" || len(cmd.Notes) != 1 || !strings.Contains(cmd.Notes[0], "0.1.1 to 0.2.0") {
		t.Errorf("%q, notes %q", cmd.Display, cmd.Notes)
	}
	// Within a minor version there is nothing to force.
	if cmd, err := up.Build(form("vm", "airgap"), demoContext()); err != nil || cmd.Display != "brig upgrade airgap" {
		t.Errorf("%q, %v", cmd.Display, err)
	}
}

func TestRegistered(t *testing.T) {
	// The list covers every registered action: a new one must get a
	// case in TestBuild too.
	want := []string{
		"allow", "config-add", "config-rm", "create", "delete", "image-build", "image-prune", "image-push", "mount-add",
		"mount-rm", "policy-save", "policy-save-preview", "profile", "reject", "resources", "sandbox-attach", "sandbox-create",
		"sandbox-delete", "sandbox-detach", "sandbox-start", "sandbox-stop", "start", "stop", "sync", "sync-preview", "upgrade",
	}
	got := action.IDs()
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("actions %v, want %v", got, want)
	}
}

func TestTyped(t *testing.T) {
	del, _ := action.Lookup("delete")
	if del.Confirmed(form("vm", "dev")) || del.Confirmed(form("vm", "dev", "confirm", "dev2")) {
		t.Error("delete runs without its VM's name typed")
	}
	if !del.Confirmed(form("vm", "dev", "confirm", " dev ")) {
		t.Error("delete refuses the VM's name typed")
	}
	if start, _ := action.Lookup("start"); !start.Confirmed(form("vm", "dev")) {
		t.Error("start wants a confirmation")
	}
	sd, _ := action.Lookup("sandbox-delete")
	if sd.Confirmed(form("vm", "dev", "sandbox", "webapp")) || sd.Confirmed(form("vm", "dev", "sandbox", "webapp", "confirm", "dev")) {
		t.Error("deleting a sandbox runs without its name typed")
	}
	if !sd.Confirmed(form("vm", "dev", "sandbox", "webapp", "confirm", "webapp")) {
		t.Error("deleting a sandbox refuses its name typed")
	}
}

// A new sandbox says what its providers open, since attaching one is what
// opens its endpoints.
func TestSandboxCreateSaysWhatItReaches(t *testing.T) {
	a, _ := action.Lookup("sandbox-create")
	cmd, err := a.Build(form("vm", "dev", "workspace", "default", "name", "x", "provider", "github", "mounts", "/data/x:/mnt/x"), demoContext())
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(cmd.Notes, "\n")
	for _, want := range []string{
		"Provider github opens api.github.com:443, github.com:443 for /usr/bin/gh, /usr/bin/git",
		"placeholders for GH_TOKEN",
		"default policy of dev's config directories",
		"read-only sandbox mount",
		"without a terminal",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q:\n%s", want, notes)
		}
	}
}
