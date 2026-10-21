// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/paths"
)

const echoManifest = `name: echo
description: Says who calls it
kind: broker
placement: vm
openshell: false
port: 8080
protocol: rest
binaries: [/usr/bin/curl]
network_profile: isolated
resources: {cpus: 1, memory: 1GiB}
secrets: [UPSTREAM_TOKEN]
`

func TestBuiltinTypesLoad(t *testing.T) {
	c := Builtin()
	for _, typ := range c.Types() {
		if got, err := c.Get(typ.Name); err != nil || !reflect.DeepEqual(got, typ) {
			t.Errorf("Get(%s) = %+v, %v", typ.Name, got, err)
		}
	}
}

func TestParseType(t *testing.T) {
	typ, err := ParseType([]byte(echoManifest))
	if err != nil {
		t.Fatal(err)
	}
	want := Type{
		Name: "echo", Description: "Says who calls it", Kind: Broker, Placement: PlacementVM,
		Port: 8080, Protocol: ProtocolREST, Binaries: []string{"/usr/bin/curl"},
		NetworkProfile: "isolated", Resources: Resources{CPUs: 1, Memory: bytesize.GiB},
		Secrets: []string{"UPSTREAM_TOKEN"},
	}
	if !reflect.DeepEqual(typ, want) {
		t.Fatalf("ParseType() = %+v, want %+v", typ, want)
	}
	if env := typ.CredentialEnv("my-echo"); !reflect.DeepEqual(env, []string{"MY_ECHO_TOKEN"}) {
		t.Errorf("CredentialEnv() = %v", env)
	}
	typ.Credential.Env = []string{"A_KEY", "B_KEY"}
	if env := typ.CredentialEnv("my-echo"); !reflect.DeepEqual(env, []string{"A_KEY", "B_KEY"}) {
		t.Errorf("CredentialEnv() = %v", env)
	}
}

func TestParseTypeRejects(t *testing.T) {
	for name, edit := range map[string][2]string{
		"unknown key":          {"secrets:", "colour: blue\nsecrets:"},
		"bad name":             {"name: echo", "name: Echo"},
		"bad kind":             {"kind: broker", "kind: daemon"},
		"bad placement":        {"placement: vm", "placement: cloud"},
		"no port":              {"port: 8080", "port: 0"},
		"bad protocol":         {"protocol: rest", "protocol: tcp"},
		"host with OpenShell":  {"placement: vm\nopenshell: false", "placement: host\nopenshell: true"},
		"host with secrets":    {"placement: vm", "placement: host"},
		"relative binary":      {"[/usr/bin/curl]", "[curl]"},
		"unclean binary":       {"[/usr/bin/curl]", "[/usr/bin/../bin/curl]"},
		"no binaries":          {"binaries: [/usr/bin/curl]", "binaries: []"},
		"bad secret":           {"[UPSTREAM_TOKEN]", "[upstream-token]"},
		"reserved env":         {"secrets:", "credential: {env: [HOME]}\nsecrets:"},
		"bad env":              {"secrets:", "credential: {env: [A-B]}\nsecrets:"},
		"bad auth":             {"secrets:", "credential: {auth: basic}\nsecrets:"},
		"header without name":  {"secrets:", "credential: {auth: header}\nsecrets:"},
		"bad header":           {"secrets:", "credential: {auth: header, header: 'x key'}\nsecrets:"},
		"memory not in MiB":    {"memory: 1GiB", "memory: 1000KiB"},
		"negative cpus":        {"cpus: 1", "cpus: -1"},
		"two documents":        {"secrets: [UPSTREAM_TOKEN]\n", "secrets: [UPSTREAM_TOKEN]\n---\nname: x\n"},
		"not a mapping at all": {echoManifest, "- echo\n"},
	} {
		manifest := strings.Replace(echoManifest, edit[0], edit[1], 1)
		if manifest == echoManifest {
			t.Fatalf("%s: the edit does not apply", name)
		}
		if _, err := ParseType([]byte(manifest)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A workload in a sandbox has no port in the VM.
	inSandbox := strings.Replace(echoManifest, "openshell: false", "openshell: true", 1)
	inSandbox = strings.Replace(inSandbox, "port: 8080", "sandbox: {name: echo, port: 4000}", 1)
	if typ, err := ParseType([]byte(inSandbox)); err != nil || typ.Sandbox.Port != 4000 || typ.ForwardsPort() {
		t.Errorf("service in a sandbox: %+v, %v", typ, err)
	}
	for name, mod := range map[string][2]string{
		"sandbox without OpenShell": {"openshell: true", "openshell: false"},
		"sandbox and port":          {"sandbox:", "port: 8080\nsandbox:"},
		"bad sandbox name":          {"name: echo, port", "name: -echo, port"},
		"bad sandbox port":          {"port: 4000}", "port: 0}"},
		"sandbox on the host":       {"placement: vm", "placement: host"},
	} {
		if _, err := ParseType([]byte(strings.Replace(inSandbox, mod[0], mod[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A host service has no port of its own: brig allocates the host port.
	host := strings.Replace(echoManifest, "placement: vm", "placement: host", 1)
	host = strings.Replace(host, "secrets: [UPSTREAM_TOKEN]", "secrets: []", 1)
	if _, err := ParseType([]byte(strings.Replace(host, "port: 8080", "port: 0", 1))); err != nil {
		t.Errorf("host service without port: %v", err)
	}
}

func TestLoadCatalog(t *testing.T) {
	c, err := LoadCatalog(fstest.MapFS{
		"echo.yaml": {Data: []byte(echoManifest)},
		"README.md": {Data: []byte("# not a type")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := c.Names(); !reflect.DeepEqual(names, []string{"echo"}) {
		t.Fatalf("Names() = %v", names)
	}
	if _, err := c.Get("mirror"); !errors.Is(err, ErrUnknownType) || !strings.Contains(err.Error(), "have echo") {
		t.Errorf("Get(mirror) = %v", err)
	}
	if _, err := LoadCatalog(fstest.MapFS{"other.yaml": {Data: []byte(echoManifest)}}); err == nil {
		t.Error("a manifest named after another type loaded")
	}
	empty, err := NewCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Get("echo"); err == nil || !strings.Contains(err.Error(), "no service types yet") {
		t.Errorf("empty catalog: %v", err)
	}
}

func testType(t *testing.T) Type {
	t.Helper()
	typ, err := ParseType([]byte(echoManifest))
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func TestStore(t *testing.T) {
	s := NewStore(paths.Dirs{Data: t.TempDir()})
	if svcs, err := s.List(); err != nil || len(svcs) != 0 {
		t.Fatalf("List() on empty store = %v, %v", svcs, err)
	}
	svc := &Service{Name: "echo", Type: "echo", Placement: PlacementVM, Port: 41000, CreatedAt: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	if err := s.Save(svc); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("echo")
	if err != nil || !reflect.DeepEqual(got, svc) {
		t.Fatalf("Load() = %+v, %v", got, err)
	}
	if _, err := s.Load("other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load(other) = %v", err)
	}
	for _, bad := range []Service{
		{Name: "x", Type: "echo", Placement: PlacementVM, Port: 0},
		{Name: "x", Type: "echo", Placement: "cloud", Port: 1},
		{Name: "x", Type: "Echo", Placement: PlacementVM, Port: 1},
		{Name: "x", Type: "echo", Placement: PlacementVM, Port: 1, Binaries: []string{"curl"}},
	} {
		if err := s.Save(&bad); err == nil {
			t.Errorf("Save(%+v) succeeded", bad)
		}
	}

	if atts, err := s.Attachments("echo"); err != nil || atts != nil {
		t.Fatalf("Attachments() without tokens.json = %v, %v", atts, err)
	}
	created := time.Date(2026, 10, 21, 13, 0, 0, 0, time.UTC)
	atts := []Attachment{
		{SHA256: "bb", Gateway: "dev", Workspace: "default", Sandbox: "web", Provider: "brig-svc-echo-web", Created: created},
		{SHA256: "aa", Gateway: "dev", Workspace: "default", Sandbox: "api", Provider: "brig-svc-echo-api", Created: created},
	}
	if err := s.SaveAttachments("echo", atts); err != nil {
		t.Fatal(err)
	}
	got2, err := s.Attachments("echo")
	if err != nil || !reflect.DeepEqual(got2, []Attachment{atts[1], atts[0]}) {
		t.Fatalf("Attachments() = %+v, %v", got2, err)
	}
	fi, err := os.Stat(s.TokensPath("echo"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("tokens.json: %v, %v", fi, err)
	}
	data, _ := os.ReadFile(s.TokensPath("echo"))
	if !strings.Contains(string(data), `"version": 1`) || !strings.Contains(string(data), `"sha256": "aa"`) {
		t.Errorf("tokens.json =\n%s", data)
	}

	unlock, err := s.Lock("echo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("echo"); err == nil {
		t.Error("a second lock succeeded")
	}
	unlock()

	if err := os.WriteFile(filepath.Join(s.Dir("echo"), "service.json"), []byte(`{"name":"other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if svcs, err := s.List(); err == nil || len(svcs) != 0 {
		t.Errorf("List() with a broken record = %v, %v", svcs, err)
	}
	if err := s.Remove("echo"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("echo") {
		t.Error("the service exists after Remove")
	}
}

func TestOwner(t *testing.T) {
	a := Attachment{Gateway: "dev", Workspace: "default", Sandbox: "web"}
	if got := a.Owner(); got != "dev/web" {
		t.Errorf("Owner() = %q", got)
	}
	a.Workspace = "team"
	if got := a.Owner(); got != "dev/web (workspace team)" {
		t.Errorf("Owner() = %q", got)
	}
	if !a.Is("dev", "team", "web") || a.Is("dev", "default", "web") {
		t.Error("Is() does not compare the workspace")
	}
}

func TestHashDriver(t *testing.T) {
	var a Attachment
	token, err := HashDriver{}.Issue(context.Background(), Env{}, &a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix) || len(token) != len(TokenPrefix)+43 {
		t.Errorf("token %q", token)
	}
	if a.SHA256 != TokenHash(token) || len(a.SHA256) != 64 {
		t.Errorf("SHA256 = %q", a.SHA256)
	}
	var b Attachment
	other, _ := HashDriver{}.Issue(context.Background(), Env{}, &b)
	if other == token {
		t.Error("two tokens are the same")
	}
}

func TestProfile(t *testing.T) {
	typ := testType(t)
	svc := &Service{Name: "echo", Type: "echo", Placement: PlacementVM, Port: 41000}
	data, err := Profile(svc, typ)
	if err != nil {
		t.Fatal(err)
	}
	var p profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.ID != "brig-svc-echo" || len(p.Credentials) != 1 || len(p.Endpoints) != 1 {
		t.Fatalf("profile =\n%s", data)
	}
	c, e := p.Credentials[0], p.Endpoints[0]
	if !reflect.DeepEqual(c.EnvVars, []string{"ECHO_TOKEN"}) || c.AuthStyle != "bearer" || c.HeaderName != "authorization" || !c.Required {
		t.Errorf("credential = %+v", c)
	}
	if e != (profileEndpoint{Host: "host.openshell.internal", Port: 41000, Protocol: "rest", Access: "read-write", Enforcement: "enforce"}) {
		t.Errorf("endpoint = %+v", e)
	}
	if !reflect.DeepEqual(p.Binaries, []string{"/usr/bin/curl"}) {
		t.Errorf("binaries = %v", p.Binaries)
	}

	svc.Binaries = []string{"/usr/bin/python3"}
	typ.Credential = Credential{Env: []string{"X_KEY"}, Auth: AuthHeader, Header: "x-api-key"}
	data, _ = Profile(svc, typ)
	p = profile{}
	_ = yaml.Unmarshal(data, &p)
	if c := p.Credentials[0]; c.AuthStyle != "header" || c.HeaderName != "x-api-key" || !reflect.DeepEqual(c.EnvVars, []string{"X_KEY"}) {
		t.Errorf("credential = %+v", c)
	}
	if !reflect.DeepEqual(p.Binaries, []string{"/usr/bin/python3"}) {
		t.Errorf("the service's binaries do not replace the type's: %v", p.Binaries)
	}
	if got := svc.URL(); got != "http://host.openshell.internal:41000" {
		t.Errorf("URL() = %q", got)
	}
	if got := ProviderName("echo", "web"); got != "brig-svc-echo-web" {
		t.Errorf("ProviderName() = %q", got)
	}
}
