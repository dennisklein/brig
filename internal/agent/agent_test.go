// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// guideSpecs are the arguments the guide's examples use.
var guideSpecs = map[Kind]Spec{
	Claude:   {Kind: Claude, User: "alice"},
	Pi:       {Kind: Pi, User: "alice", Endpoint: "https://llm.example.com/v1", Model: "your-model-id"},
	OpenCode: {Kind: OpenCode, User: "alice", Endpoint: "https://llm.example.com/v1", Model: "your-model-id"},
}

func paths(p *Plan) map[string]File {
	m := map[string]File{}
	for _, f := range p.Files {
		m[f.Path] = f
	}
	return m
}

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		spec      Spec
		want      []string
		providers string
	}{
		{Spec{Kind: Claude, User: "alice"}, []string{
			"agents/claude/Containerfile", "agents/claude/claude-sandbox", "agents/claude/managed-settings.json",
			"agents/claude/etc/CLAUDE.md", "agents/claude/skills/.gitkeep", "agents/claude/agent.yaml",
			"profiles/claude-code.yaml", "providers/claude-code.yaml", "policies/default.yaml",
		}, "[claude-code]"},
		{Spec{Kind: Claude, Name: "review", User: "alice", GitHub: "alice-gh"}, []string{
			"agents/review/Containerfile", "providers/github.yaml",
		}, "[claude-code, github]"},
		{guideSpecs[Pi], []string{
			"agents/pi/Containerfile", "agents/pi/pi-sandbox", "agents/pi/etc/settings.json", "agents/pi/etc/models.json",
			"agents/pi/etc/AGENTS.md", "profiles/llm.yaml", "providers/llm.yaml",
		}, "[llm]"},
		{guideSpecs[OpenCode], []string{
			"agents/opencode/Containerfile", "agents/opencode/opencode-sandbox", "agents/opencode/etc/opencode.json",
			"agents/opencode/etc/AGENTS.md", "profiles/llm.yaml", "providers/llm.yaml",
		}, "[llm]"},
		{Spec{Kind: OpenCode, User: "alice", Anthropic: true, Model: "claude-sonnet-4-5"}, []string{
			"agents/opencode/etc/opencode.json", "profiles/anthropic.yaml", "providers/anthropic.yaml",
		}, "[anthropic]"},
	} {
		p, err := New(tc.spec)
		if err != nil {
			t.Errorf("New(%+v): %v", tc.spec, err)
			continue
		}
		files := paths(p)
		for _, want := range tc.want {
			if _, ok := files[want]; !ok {
				t.Errorf("New(%+v) lacks %s; it has %v", tc.spec, want, p.Files)
			}
		}
		def, err := ParseDefinition(files["agents/"+p.Name+"/agent.yaml"].Data)
		if err != nil {
			t.Errorf("agent.yaml of %+v: %v", tc.spec, err)
		}
		if got := "[" + strings.Join(def.Providers, ", ") + "]"; got != tc.providers {
			t.Errorf("providers of %+v = %s, want %s", tc.spec, got, tc.providers)
		}
		if def.Image != "localhost/"+p.Name+"-agent" || def.Template != p.Name {
			t.Errorf("definition %+v", def)
		}
		// Scripts run; nothing else does. Secrets files stay out of the shared directory.
		for _, f := range p.Files {
			isScript := strings.HasSuffix(f.Path, "-sandbox")
			if isScript != (f.Mode&0o111 != 0) {
				t.Errorf("%s has mode %o", f.Path, f.Mode)
			}
			if (f.Root == Secrets) != strings.HasPrefix(f.Path, "providers/") {
				t.Errorf("%s is in the wrong root", f.Path)
			}
			if len(f.Data) > 0 && f.Data[len(f.Data)-1] != '\n' {
				t.Errorf("%s does not end in a newline", f.Path)
			}
		}
	}
}

func TestNewRejects(t *testing.T) {
	ep, model := "https://llm.example.com/v1", "m"
	for name, tc := range map[string]struct {
		spec Spec
		want string
	}{
		"pi without endpoint":    {Spec{Kind: Pi, Model: model}, "--endpoint"},
		"pi without model":       {Spec{Kind: Pi, Endpoint: ep}, "--model"},
		"pi anthropic":           {Spec{Kind: Pi, Endpoint: ep, Model: model, Anthropic: true}, "opencode"},
		"claude with endpoint":   {Spec{Kind: Claude, Endpoint: ep}, "subscription token"},
		"opencode with nothing":  {Spec{Kind: OpenCode}, "--anthropic"},
		"opencode with both":     {Spec{Kind: OpenCode, Endpoint: ep, Model: model, Anthropic: true}, "not both"},
		"opencode without model": {Spec{Kind: OpenCode, Endpoint: ep}, "--model"},
		"bad name":               {Spec{Kind: Claude, Name: "../x"}, "invalid agent name"},
		"ftp endpoint":           {Spec{Kind: Pi, Endpoint: "ftp://x/v1", Model: model}, "invalid endpoint"},
		"endpoint with secret":   {Spec{Kind: Pi, Endpoint: "https://u:p@x/v1", Model: model}, "invalid endpoint"}, //nolint:gosec // G101: a URL that must be refused
		"endpoint with quote":    {Spec{Kind: Pi, Endpoint: `https://x/v1"`, Model: model}, "invalid endpoint"},
		"endpoint bad port":      {Spec{Kind: Pi, Endpoint: "https://x:99999/v1", Model: model}, "port"},
		"model with quote":       {Spec{Kind: Pi, Endpoint: ep, Model: `a"b`}, "invalid model"},
		"bad login":              {Spec{Kind: Claude, GitHub: "a b"}, "GitHub login"},
	} {
		if _, err := New(tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New() error = %v, want %q", name, err, tc.want)
		}
	}
}

func TestEndpoint(t *testing.T) {
	p, err := New(Spec{Kind: OpenCode, User: "u", Endpoint: "http://ollama.lan:11434/v1", Model: "qwen3:8b"})
	if err != nil {
		t.Fatal(err)
	}
	files := paths(p)
	if prof := string(files["profiles/llm.yaml"].Data); !strings.Contains(prof, "host: ollama.lan ") || !strings.Contains(prof, "port: 11434\n") {
		t.Errorf("profile:\n%s", prof)
	}
	if prov := string(files["providers/llm.yaml"].Data); !strings.Contains(prov, "lookup: [service, ollama.lan]") {
		t.Errorf("provider:\n%s", prov)
	}
	if cfg := string(files["agents/opencode/etc/opencode.json"].Data); !strings.Contains(cfg, `"baseURL": "http://ollama.lan:11434/v1"`) || !strings.Contains(cfg, `"llm/qwen3:8b"`) {
		t.Errorf("opencode.json:\n%s", cfg)
	}
}

func TestProviderFileQuotesOddAttributes(t *testing.T) {
	p, err := New(Spec{Kind: Claude, User: `al ice: "x"`})
	if err != nil {
		t.Fatal(err)
	}
	var prov struct {
		Credentials map[string]struct {
			SecretTool struct {
				Lookup []string `yaml:"lookup"`
			} `yaml:"secret_tool"`
		} `yaml:"credentials"`
	}
	if err := yaml.Unmarshal(paths(p)["providers/claude-code.yaml"].Data, &prov); err != nil {
		t.Fatal(err)
	}
	got := prov.Credentials["CLAUDE_CODE_OAUTH_TOKEN"].SecretTool.Lookup
	if len(got) != 4 || got[1] != "claude.ai" || got[3] != `al ice: "x"` {
		t.Errorf("lookup = %q", got)
	}
}

func TestParseDefinition(t *testing.T) {
	d, err := ParseDefinition([]byte("image: localhost/x-agent\ntemplate: x # c\nproviders: [a, b]\ncommand: x-sandbox\n"))
	if err != nil || d.Template != "x" || len(d.Providers) != 2 {
		t.Errorf("ParseDefinition() = %+v, %v", d, err)
	}
	for _, bad := range []string{"template: x\n", "image: localhost/x:1\n", "image: x\nunknown: 1\n"} {
		if _, err := ParseDefinition([]byte(bad)); err == nil {
			t.Errorf("ParseDefinition(%q) accepted it", bad)
		}
	}
	if _, err := ParseDefinition([]byte("image: registry:5000/x\n")); err != nil {
		t.Errorf("a registry port is no tag: %v", err)
	}
}

func TestWrite(t *testing.T) {
	roots := Roots{Shared: t.TempDir(), Secrets: filepath.Join(t.TempDir(), "personal")}
	p, err := New(guideSpecs[Claude])
	if err != nil {
		t.Fatal(err)
	}
	none := func(File) string { return "" }
	res, err := Write(p.Files, roots, false, none)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.State != "created" {
			t.Errorf("%s: %s", r.Path, r.State)
		}
	}
	for path, mode := range map[string]os.FileMode{
		filepath.Join(roots.Shared, "agents/claude/claude-sandbox"):        0o755,
		filepath.Join(roots.Shared, "agents/claude/Containerfile"):         0o644,
		filepath.Join(roots.Secrets, "providers/claude-code.yaml"):         0o644,
		filepath.Join(roots.Shared, "agents/claude/skills/.gitkeep"):       0o644,
		filepath.Join(roots.Shared, "agents/claude/etc/CLAUDE.md"):         0o644,
		filepath.Join(roots.Shared, "agents/claude/managed-settings.json"): 0o644,
	} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v", path, fi, err)
		}
	}
	if fi, err := os.Stat(roots.Secrets); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("personal directory: %v, %v", fi, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(roots.Shared, "*", ".brig-agent-*")); len(entries) != 0 {
		t.Errorf("temporary files left: %v", entries)
	}

	// Again: nothing to do.
	res, err = Write(p.Files, roots, false, none)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.State != "unchanged" {
			t.Errorf("second run: %s: %s", r.Path, r.State)
		}
	}

	// A changed file stops everything, and nothing else is written.
	edited := filepath.Join(roots.Shared, "agents/claude/Containerfile")
	other := filepath.Join(roots.Shared, "agents/claude/managed-settings.json")
	if err := os.WriteFile(edited, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	_, err = Write(p.Files, roots, false, none)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Paths) != 1 || ce.Paths[0] != edited {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := os.Stat(other); err == nil {
		t.Error("a conflict still wrote the other files")
	}
	if got, _ := os.ReadFile(edited); string(got) != "mine\n" {
		t.Errorf("the edited file changed: %q", got)
	}

	// A skipped file is neither written nor compared.
	res, err = Write(p.Files, roots, false, func(f File) string {
		if f.Path == "agents/claude/Containerfile" {
			return "defined elsewhere"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if (r.State == "skipped") != strings.HasSuffix(r.Path, "/Containerfile") {
			t.Errorf("%s: %s", r.Path, r.State)
		}
	}

	// --force overwrites.
	res, err = Write(p.Files, roots, true, none)
	if err != nil {
		t.Fatal(err)
	}
	var overwritten int
	for _, r := range res {
		if r.State == "overwritten" {
			overwritten++
		}
	}
	if got, _ := os.ReadFile(edited); string(got) == "mine\n" || overwritten != 1 {
		t.Errorf("force: %d overwritten, file %q", overwritten, got)
	}
}

var guideMarkerRE = regexp.MustCompile(`^<!-- agent-file: (\w+) (\S+) -->$`)

// guideFiles returns the code blocks of the guide that follow an
// "agent-file: KIND PATH" marker, by KIND and PATH.
func guideFiles(t *testing.T) map[[2]string]string {
	t.Helper()
	f, err := os.Open("../../docs/how-to/agents.md")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	blocks := map[[2]string]string{}
	var key *[2]string
	var body strings.Builder
	inBlock := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case inBlock && line == "```":
			blocks[*key] = body.String()
			inBlock, key = false, nil
		case inBlock:
			body.WriteString(line + "\n")
		case key != nil && strings.HasPrefix(line, "```"):
			inBlock = true
			body.Reset()
		default:
			if m := guideMarkerRE.FindStringSubmatch(line); m != nil {
				k := [2]string{m[1], m[2]}
				if _, dup := blocks[k]; dup {
					t.Errorf("the guide marks %v twice", k)
				}
				key = &k
			} else if key != nil && line != "" {
				t.Errorf("the marker for %v is not followed by a code block: %q", *key, line)
				key = nil
			}
		}
	}
	return blocks
}

// TestGuideShowsTheFiles keeps the files in the guide and the ones that brig
// agent init writes the same, as rendered for the guide's examples.
func TestGuideShowsTheFiles(t *testing.T) {
	blocks := guideFiles(t)
	for key, block := range blocks {
		kind, err := ParseKind(key[0])
		if err != nil {
			t.Errorf("guide: %v", err)
			continue
		}
		p, err := New(guideSpecs[kind])
		if err != nil {
			t.Fatal(err)
		}
		f, ok := paths(p)[key[1]]
		if !ok {
			t.Errorf("guide shows %s for %s, which brig agent init does not write", key[1], kind)
			continue
		}
		if string(f.Data) != block {
			t.Errorf("guide's %s for %s differs from brig agent init's:\n--- guide\n%s--- brig\n%s", key[1], kind, block, f.Data)
		}
	}
	// What the guide must show, for each agent.
	for _, key := range [][2]string{
		{"claude", "agents/claude/Containerfile"},
		{"claude", "agents/claude/claude-sandbox"},
		{"claude", "agents/claude/managed-settings.json"},
		{"claude", "profiles/claude-code.yaml"},
		{"pi", "agents/pi/Containerfile"},
		{"pi", "agents/pi/pi-sandbox"},
		{"pi", "agents/pi/etc/settings.json"},
		{"pi", "agents/pi/etc/models.json"},
		{"pi", "profiles/llm.yaml"},
		{"opencode", "agents/opencode/Containerfile"},
		{"opencode", "agents/opencode/opencode-sandbox"},
		{"opencode", "agents/opencode/etc/opencode.json"},
	} {
		if _, ok := blocks[key]; !ok {
			t.Errorf("the guide does not show %s for %s", key[1], key[0])
		}
	}
}
