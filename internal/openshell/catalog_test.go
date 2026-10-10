// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package openshell

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGatewayRejectsBadNames(t *testing.T) {
	c, _ := fakeCLI(t, "")
	for _, name := range []string{"", "../x", "-g"} {
		if _, err := c.Gateway(name); err == nil {
			t.Errorf("Gateway(%q) succeeded", name)
		}
	}
}

func TestProfiles(t *testing.T) {
	c, log := fakeCLI(t, `cat <<'EOF'
warning: something to say first
[WARN] and a log line that looks like JSON
[
  {"id": "github", "resource_version": 3, "scope": "workspace", "source": "user",
   "display_name": "GitHub", "endpoints": [{"host": "api.github.com", "port": 443}, {"host": "github.com", "ports": [443, 22]}]},
  {"id": "anthropic", "scope": "platform", "display_name": "Anthropic"}
]
EOF`)
	g, err := c.Gateway("brig-dev")
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.Profiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Profile{
		{ID: "github", ResourceVersion: 3, Scope: ScopeWorkspace, Source: "user", Endpoints: []Endpoint{
			{Host: "api.github.com", Port: 443}, {Host: "github.com", Ports: []uint32{443, 22}},
		}},
		{ID: "anthropic", Scope: ScopePlatform},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Profiles() = %+v, want %+v", got, want)
	}
	if l := readFile(t, log); l != "-g brig-dev profile list -o json\n" {
		t.Errorf("argv: %q", l)
	}
	if s := got[0].Endpoints[1].String(); s != "github.com:{443,22}" {
		t.Errorf("Endpoint.String() = %q", s)
	}
}

func TestProvidersPages(t *testing.T) {
	c, log := fakeCLI(t, `case "$*" in
*--page-token*) echo '{"providers": [{"name": "b", "type": "github", "credential_keys": ["GH_TOKEN"]}], "next_page_token": ""}' ;;
*) echo '{"providers": [{"name": "a", "type": "anthropic", "credential_keys": []}], "next_page_token": "p2"}' ;;
esac`)
	g, _ := c.Gateway("brig-dev")
	got, err := g.Providers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Provider{{Name: "a", Type: "anthropic", CredentialKeys: []string{}}, {Name: "b", Type: "github", CredentialKeys: []string{"GH_TOKEN"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Providers() = %+v, want %+v", got, want)
	}
	if l := readFile(t, log); l != "-g brig-dev provider list -o json\n-g brig-dev provider list -o json --page-token p2\n" {
		t.Errorf("argv: %q", l)
	}
}

func TestProvidersRejectsTokenCycles(t *testing.T) {
	c, _ := fakeCLI(t, `case "$*" in
*"--page-token p2"*) echo '{"providers": [], "next_page_token": "p3"}' ;;
*) echo '{"providers": [], "next_page_token": "p2"}' ;;
esac`)
	g, _ := c.Gateway("brig-dev")
	if _, err := g.Providers(context.Background()); err == nil || !strings.Contains(err.Error(), "repeats") {
		t.Fatalf("Providers() error = %v, want a repeated token", err)
	}
}

// TestProviderSecretsStayOffTheCommandLine checks that credential values
// reach the CLI through its environment only.
func TestProviderSecretsStayOffTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env")
	c, log := fakeCLI(t, `printf '%s|%s\n' "$GH_TOKEN" "$OTHER" >>'`+envFile+`'`)
	t.Setenv("GH_TOKEN", "inherited")
	g, _ := c.Gateway("brig-dev")
	ctx := context.Background()
	if err := g.CreateProvider(ctx, "github", "github", map[string]string{"GH_TOKEN": "s3cret", "OTHER": "two"}); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateProvider(ctx, "github", map[string]string{"GH_TOKEN": "n3w"}, []string{"OLD"}); err != nil {
		t.Fatal(err)
	}
	argv := readFile(t, log)
	want := "-g brig-dev provider create --name github --type github --credential GH_TOKEN --credential OTHER\n" +
		"-g brig-dev provider update github --credential GH_TOKEN --credential OLD=\n"
	if argv != want {
		t.Errorf("argv:\n%s\nwant:\n%s", argv, want)
	}
	if strings.Contains(argv, "s3cret") || strings.Contains(argv, "n3w") {
		t.Error("a secret appeared on the command line")
	}
	if env := readFile(t, envFile); env != "s3cret|two\nn3w|\n" {
		t.Errorf("environment: %q", env)
	}
}

func TestProfileCommands(t *testing.T) {
	c, log := fakeCLI(t, "")
	g, _ := c.Gateway("brig-dev")
	ctx := context.Background()
	for _, err := range []error{
		g.LintProfile(ctx, "/p/a.yaml"),
		g.ImportProfile(ctx, "/p/a.yaml"),
		g.UpdateProfile(ctx, "a", "/p/a.yaml"),
		g.DeleteProfile(ctx, "a"),
		g.DeleteProvider(ctx, "x"),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := `-g brig-dev profile lint -f /p/a.yaml
-g brig-dev profile import -f /p/a.yaml
-g brig-dev profile update a -f /p/a.yaml
-g brig-dev profile delete a
-g brig-dev provider delete x
`
	if got := readFile(t, log); got != want {
		t.Errorf("argv:\n%s\nwant:\n%s", got, want)
	}
}

func TestProviderErrorsRedactSecrets(t *testing.T) {
	c, _ := fakeCLI(t, `echo "invalid credential GH_TOKEN=$GH_TOKEN" >&2; exit 1`)
	g, _ := c.Gateway("brig-dev")
	err := g.CreateProvider(context.Background(), "github", "github", map[string]string{"GH_TOKEN": "s3cret"})
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("CreateProvider() error = %v", err)
	}
	err = g.UpdateProvider(context.Background(), "github", map[string]string{"GH_TOKEN": "s3cret"}, nil)
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("UpdateProvider() error = %v", err)
	}
}

func TestRedactLongerValueFirst(t *testing.T) {
	creds := map[string]string{"BOT_ID": "123456", "BOT_TOKEN": "123456:SECRETPART"}
	// The map order is random, so repeat to cover both orders.
	for range 50 {
		err := redact(errors.New("bad token 123456:SECRETPART"), creds)
		if err == nil || strings.Contains(err.Error(), "SECRETPART") {
			t.Fatalf("redact left part of a credential: %v", err)
		}
	}
}
