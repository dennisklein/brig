// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/sdcred"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/internal/vmnet"
)

// echoType is a service type for tests, of placement vm without OpenShell.
var echoType = service.Type{
	Name: "echo", Kind: service.Broker, Placement: service.PlacementVM, Port: 8080,
	Protocol: service.ProtocolREST, Binaries: []string{"/usr/bin/curl"},
}

// ctlType is a service type for tests, of placement host.
var ctlType = service.Type{
	Name: "ctl", Kind: service.Runner, Placement: service.PlacementHost,
	Protocol: service.ProtocolMCP, Binaries: []string{"/usr/bin/node"},
}

// fakeServiceTypes makes echoType and ctlType brig's service types.
func fakeServiceTypes(t *testing.T) {
	t.Helper()
	c, err := service.NewCatalog(echoType, ctlType)
	if err != nil {
		t.Fatal(err)
	}
	orig := serviceTypes
	serviceTypes = func() *service.Catalog { return c }
	t.Cleanup(func() { serviceTypes = orig })
}

// fakeServiceGateway is the workspaces of gateways, as openServiceGateway
// returns them, by VM and workspace.
type fakeServiceGateway struct {
	vm, ws    string
	calls     *[]string
	profiles  map[string][]openshell.Profile
	providers map[string][]openshell.Provider
	creds     map[string]map[string]string
	failOn    string
}

func (g *fakeServiceGateway) key() string { return g.vm + "/" + g.ws }

func (g *fakeServiceGateway) call(format string, args ...string) error {
	c := g.key() + " " + format
	for _, a := range args {
		c += " " + a
	}
	*g.calls = append(*g.calls, c)
	if g.failOn != "" && strings.HasPrefix(format, g.failOn) {
		return errors.New(format + " failed")
	}
	return nil
}

func (g *fakeServiceGateway) Workspaces(context.Context) ([]string, error) {
	return []string{"default", "team"}, nil
}

func (g *fakeServiceGateway) Profiles(context.Context) ([]openshell.Profile, error) {
	return g.profiles[g.key()], nil
}

func (g *fakeServiceGateway) ImportProfile(_ context.Context, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	id := strings.TrimPrefix(strings.SplitN(string(data), "\n", 2)[0], "id: ")
	g.profiles[g.key()] = append(g.profiles[g.key()], openshell.Profile{ID: id, Scope: openshell.ScopeWorkspace, ResourceVersion: 1})
	return g.call("profile import", id)
}

func (g *fakeServiceGateway) UpdateProfile(_ context.Context, id, file string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !strings.Contains(string(data), "resource_version: 1\n") {
		return errors.New("update without resource_version")
	}
	return g.call("profile update", id)
}

func (g *fakeServiceGateway) DeleteProfile(_ context.Context, id string) error {
	g.profiles[g.key()] = slices.DeleteFunc(g.profiles[g.key()], func(p openshell.Profile) bool { return p.ID == id })
	return g.call("profile delete", id)
}

func (g *fakeServiceGateway) Providers(context.Context) ([]openshell.Provider, error) {
	return g.providers[g.key()], nil
}

func (g *fakeServiceGateway) CreateProvider(_ context.Context, name, typ string, creds map[string]string) error {
	if err := g.call("provider create", name, typ); err != nil {
		return err
	}
	g.providers[g.key()] = append(g.providers[g.key()], openshell.Provider{Name: name, Type: typ})
	g.creds[name] = creds
	return nil
}

func (g *fakeServiceGateway) DeleteProvider(_ context.Context, name string) error {
	g.providers[g.key()] = slices.DeleteFunc(g.providers[g.key()], func(p openshell.Provider) bool { return p.Name == name })
	return g.call("provider delete", name)
}

func (g *fakeServiceGateway) AttachProvider(_ context.Context, sandbox, name string) error {
	return g.call("provider attach", sandbox, name)
}

func (g *fakeServiceGateway) DetachProvider(_ context.Context, sandbox, name string) error {
	return g.call("provider detach", sandbox, name)
}

// serviceFakes are the fakes of a service test.
type serviceFakes struct {
	a        *app
	conn     *fakeDomains
	calls    []string
	scripts  []string
	gateways *fakeServiceGateway
}

// newServiceFakes sets up an app with a gateway VM dev, whose gateway has
// the sandboxes testSandboxes, and a service echo of type echoType with its
// VM; all VMs run.
func newServiceFakes(t *testing.T) *serviceFakes {
	t.Helper()
	fakeServiceTypes(t)
	f := &serviceFakes{a: testApp(t), conn: &fakeDomains{state: libvirt.StateRunning}}
	testVM(t, f.a, "dev")
	f.gateways = &fakeServiceGateway{
		calls: &f.calls, profiles: map[string][]openshell.Profile{},
		providers: map[string][]openshell.Provider{}, creds: map[string]map[string]string{},
	}
	origGW, origIn, origConn := openServiceGateway, inVM, connectDomains
	openServiceGateway = func(v *vm.VM, ws string) (serviceGateway, error) {
		g := *f.gateways
		g.vm, g.ws = v.Name, ws
		return &g, nil
	}
	inVM = func(_ context.Context, target sshx.Target, script string, stdin []byte) error {
		f.scripts = append(f.scripts, target.User+"@"+strconv.Itoa(target.Port)+" "+script+"\n<<<"+string(stdin))
		return nil
	}
	connectDomains = func(context.Context, *app) (domainsConn, error) { return f.conn, nil }
	t.Cleanup(func() { openServiceGateway, inVM, connectDomains = origGW, origIn, origConn })
	fakeSandboxes(t, map[string][]store.Sandbox{"dev": testSandboxes})
	svc := &service.Service{Name: "echo", Type: "echo", Placement: service.PlacementVM, Port: 41000, CreatedAt: time.Now().UTC()}
	if err := f.a.services.Save(svc); err != nil {
		t.Fatal(err)
	}
	v := testVM(t, f.a, "echo")
	v.Role, v.Type, v.Ports = vm.RoleService, "echo", vm.Ports{SSH: 41022, Gateway: 41670}
	if err := f.a.vms.Save(v); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeDomains) Close() error { return nil }

// servicePorts reads the service ports file of the VM called name.
func servicePorts(t *testing.T, name string) []uint16 {
	t.Helper()
	path, err := servicePortsPath(name)
	if err != nil {
		t.Fatal(err)
	}
	ports, err := vmnet.ReadServicePorts(path)
	if err != nil {
		t.Fatal(err)
	}
	return ports
}

func TestServiceAttachAndDetach(t *testing.T) {
	f := newServiceFakes(t)
	a, ctx := f.a, t.Context()
	svc, _ := a.services.Load("echo")
	gw, _ := a.vms.Load("dev")
	att, err := a.attachService(ctx, f.conn, svc, echoType, gw, "default", "webapp")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"dev/default profile import brig-svc-echo",
		"dev/default provider create brig-svc-echo-webapp brig-svc-echo",
		"dev/default provider attach webapp brig-svc-echo-webapp",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("gateway calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	token := f.gateways.creds["brig-svc-echo-webapp"]["ECHO_TOKEN"]
	if !strings.HasPrefix(token, service.TokenPrefix) || att.SHA256 != service.TokenHash(token) {
		t.Errorf("token %q, attachment %+v", token, att)
	}
	atts, err := a.services.Attachments("echo")
	if err != nil || len(atts) != 1 || !atts[0].Is("dev", "default", "webapp") || atts[0].SHA256 != att.SHA256 {
		t.Fatalf("attachments = %+v, %v", atts, err)
	}
	if ports := servicePorts(t, "dev"); !slices.Equal(ports, []uint16{41000}) {
		t.Errorf("ports of dev = %v", ports)
	}
	if len(f.scripts) != 2 || !strings.HasPrefix(f.scripts[0], "agent@40022 ") || !strings.Contains(f.scripts[0], "ListenStream=127.0.0.1:") ||
		!strings.Contains(f.scripts[0], "unit=brig-svc-echo port=41000") {
		t.Fatalf("scripts:\n%s", strings.Join(f.scripts, "\n---\n"))
	}
	// The service VM gets the token's hash, never the token.
	push := f.scripts[1]
	if !strings.HasPrefix(push, "root@41022 ") || strings.Contains(push, "brig-service.target") {
		t.Errorf("push:\n%s", push)
	}
	_, data, _ := strings.Cut(push, "<<<tokens.json ")
	tokens, err := base64.StdEncoding.DecodeString(strings.TrimSpace(data))
	if err != nil || !strings.Contains(string(tokens), att.SHA256) || strings.Contains(string(tokens), token) {
		t.Errorf("tokens.json = %s, %v", tokens, err)
	}

	if _, err := a.attachService(ctx, f.conn, svc, echoType, gw, "default", "webapp"); exitStatus(err) != exitExists {
		t.Errorf("second attach: %v", err)
	}
	if _, err := a.attachService(ctx, f.conn, svc, echoType, gw, "default", "old"); exitStatus(err) != exitNotReady {
		t.Errorf("attach to a stopped sandbox: %v", err)
	}

	f.calls, f.scripts = nil, nil
	var w bytes.Buffer
	if err := a.detachService(ctx, f.conn, svc, echoType, atts[0], true, &w); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"dev/default provider detach webapp brig-svc-echo-webapp",
		"dev/default provider delete brig-svc-echo-webapp",
		"dev/default profile delete brig-svc-echo",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("gateway calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if atts, _ := a.services.Attachments("echo"); len(atts) != 0 {
		t.Errorf("attachments after detach = %+v", atts)
	}
	if ports := servicePorts(t, "dev"); len(ports) != 0 {
		t.Errorf("ports of dev after detach = %v", ports)
	}
	if len(f.scripts) != 2 || !strings.Contains(f.scripts[1], `systemctl --user stop "$unit.socket"`) {
		t.Errorf("scripts:\n%s", strings.Join(f.scripts, "\n---\n"))
	}
}

func TestServiceAttachUndoesItself(t *testing.T) {
	f := newServiceFakes(t)
	f.gateways.failOn = "provider attach"
	svc, _ := f.a.services.Load("echo")
	gw, _ := f.a.vms.Load("dev")
	if _, err := f.a.attachService(t.Context(), f.conn, svc, echoType, gw, "default", "webapp"); err == nil {
		t.Fatal("attach succeeded")
	}
	if !slices.Contains(f.calls, "dev/default provider delete brig-svc-echo-webapp") {
		t.Errorf("provider not deleted: %v", f.calls)
	}
	if atts, _ := f.a.services.Attachments("echo"); len(atts) != 0 {
		t.Errorf("attachments = %+v", atts)
	}
	if ports := servicePorts(t, "dev"); len(ports) != 0 {
		t.Errorf("ports = %v", ports)
	}
}

func TestReconcileServices(t *testing.T) {
	f := newServiceFakes(t)
	a := f.a
	created := time.Now().UTC()
	atts := []service.Attachment{
		// Its provider is gone, so it gets a new token.
		{SHA256: "old", Gateway: "dev", Workspace: "default", Sandbox: "webapp", Provider: "brig-svc-echo-webapp", Created: created},
		// Its sandbox is gone, so its token is revoked.
		{SHA256: "gone", Gateway: "dev", Workspace: "default", Sandbox: "deleted", Provider: "brig-svc-echo-deleted", Created: created},
		// Another gateway's.
		{SHA256: "other", Gateway: "prod", Workspace: "default", Sandbox: "webapp", Provider: "brig-svc-echo-webapp", Created: created},
	}
	if err := a.services.SaveAttachments("echo", atts); err != nil {
		t.Fatal(err)
	}
	f.gateways.profiles["dev/default"] = []openshell.Profile{{ID: "brig-svc-echo", Scope: openshell.ScopeWorkspace, ResourceVersion: 1}}
	f.gateways.providers["dev/default"] = []openshell.Provider{{Name: "brig-svc-echo-deleted"}, {Name: "github"}}
	f.gateways.providers["dev/team"] = []openshell.Provider{{Name: "brig-svc-gone-kit"}}
	f.gateways.profiles["dev/team"] = []openshell.Profile{{ID: "brig-svc-gone", Scope: openshell.ScopeWorkspace}}
	gw, _ := a.vms.Load("dev")
	var w bytes.Buffer
	if err := a.reconcileServices(t.Context(), f.conn, gw, &w); err != nil {
		t.Fatalf("%v\n%s", err, &w)
	}
	got, _ := a.services.Attachments("echo")
	if len(got) != 2 || !got[0].Is("dev", "default", "webapp") || got[0].SHA256 == "old" || got[1].Gateway != "prod" {
		t.Errorf("attachments = %+v", got)
	}
	for _, call := range []string{
		"dev/default provider delete brig-svc-echo-deleted",
		"dev/default profile update brig-svc-echo",
		"dev/default provider create brig-svc-echo-webapp brig-svc-echo",
		"dev/default provider attach webapp brig-svc-echo-webapp",
		"dev/team provider delete brig-svc-gone-kit",
		"dev/team profile delete brig-svc-gone",
	} {
		if !slices.Contains(f.calls, call) {
			t.Errorf("no call %q in:\n%s", call, strings.Join(f.calls, "\n"))
		}
	}
	if slices.Contains(f.calls, "dev/default provider delete github") {
		t.Error("deleted a provider that is not brig's")
	}
	if !strings.Contains(w.String(), "Sandbox dev/deleted is gone") || !strings.Contains(w.String(), "issuing a new one") {
		t.Errorf("output:\n%s", &w)
	}
}

func TestRemovingAGatewayRevokesItsTokens(t *testing.T) {
	f := newServiceFakes(t)
	att := service.Attachment{SHA256: "x", Gateway: "dev", Workspace: "default", Sandbox: "webapp", Provider: "brig-svc-echo-webapp"}
	if err := f.a.services.SaveAttachments("echo", []service.Attachment{att}); err != nil {
		t.Fatal(err)
	}
	gw, _ := f.a.vms.Load("dev")
	conn := &fakeDomains{state: libvirt.StateShutoff}
	if err := f.a.remove(t.Context(), conn, gw, false, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if atts, _ := f.a.services.Attachments("echo"); len(atts) != 0 {
		t.Errorf("attachments = %+v", atts)
	}
	if len(f.calls) != 0 {
		t.Errorf("gateway calls on a removed VM: %v", f.calls)
	}
}

func TestStartServiceHandsOverSecretsAndTokens(t *testing.T) {
	f := newServiceFakes(t)
	v, _ := f.a.vms.Load("echo")
	if err := f.a.startService(t.Context(), v, false); err != nil {
		t.Fatal(err)
	}
	if len(f.scripts) != 1 || !strings.HasPrefix(f.scripts[0], "root@41022 ") ||
		!strings.Contains(f.scripts[0], "systemctl start brig-service.target") ||
		!strings.Contains(f.scripts[0], `find "$secrets" -mindepth 1 -delete`) {
		t.Errorf("scripts:\n%s", strings.Join(f.scripts, "\n---\n"))
	}
}

func TestServiceFiles(t *testing.T) {
	got := string(serviceFiles(map[string]string{"B": "two\nlines", "A": "one"}, []byte("{}\n")))
	want := "A b25l\nB dHdvCmxpbmVz\ntokens.json e30K\n"
	if got != want {
		t.Errorf("serviceFiles = %q, want %q", got, want)
	}
}

func TestServiceVMNetwork(t *testing.T) {
	f := newServiceFakes(t)
	v, _ := f.a.vms.Load("echo")
	c, err := f.a.netConfig(v)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServicePort != 41000 || c.GuestServicePort != 8080 {
		t.Errorf("service port %d to %d", c.ServicePort, c.GuestServicePort)
	}
	if err := f.a.writeKeys(v); err != nil {
		t.Fatal(err)
	}
	spec, err := f.a.domainSpec(v)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(spec.Credentials, func(c sdcred.Credential) bool { return c.Name == "tmpfiles.extra" })
	if i < 0 || !strings.Contains(string(spec.Credentials[i].Data), "/run/systemd/user/openshell-gateway.service") {
		t.Errorf("the service VM boots OpenShell: %+v", spec.Credentials)
	}
}

func TestCommandsThatNeedOpenShellRefuseServices(t *testing.T) {
	newServiceFakes(t)
	captureExec(t)
	if _, err := run(t, "sandbox", "connect", "echo", "webapp"); err == nil || !strings.Contains(err.Error(), "runs no OpenShell gateway") {
		t.Errorf("sandbox connect: %v", err)
	}
	if _, err := run(t, "delete", "echo"); err == nil || !strings.Contains(err.Error(), "brig service delete") {
		t.Errorf("delete: %v", err)
	}
	out, err := run(t, "list")
	if err != nil || strings.Contains(out, "echo") || !strings.Contains(out, "dev") {
		t.Errorf("list: %v\n%s", err, out)
	}
}

// fakeRunner is a HostRunner that records its calls.
type fakeRunner struct{ calls []string }

func (r *fakeRunner) Start(_ context.Context, svc service.Service) error {
	r.calls = append(r.calls, "start "+svc.Name)
	return nil
}

func (r *fakeRunner) Stop(_ context.Context, svc service.Service) error {
	r.calls = append(r.calls, "stop "+svc.Name)
	return nil
}

func (r *fakeRunner) Delete(_ context.Context, svc service.Service) error {
	r.calls = append(r.calls, "delete "+svc.Name)
	return nil
}

func TestServiceOnTheHost(t *testing.T) {
	f := newServiceFakes(t)
	r := &fakeRunner{}
	hostRunners["ctl"] = r
	t.Cleanup(func() { delete(hostRunners, "ctl") })
	out, err := run(t, "service", "create", "vms", "--type", "ctl")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	svc, err := f.a.services.Load("vms")
	if err != nil || svc.Placement != service.PlacementHost || svc.Port == 0 {
		t.Fatalf("record = %+v, %v", svc, err)
	}
	if !strings.Contains(out, "http://host.openshell.internal:") {
		t.Errorf("output:\n%s", out)
	}
	if _, err := run(t, "service", "create", "vms", "--type", "ctl"); exitStatus(err) != exitExists {
		t.Errorf("second create: %v", err)
	}
	if _, err := run(t, "service", "create", "x", "--type", "ctl", "--cpus", "2"); err == nil {
		t.Error("create on the host with --cpus succeeded")
	}
	out, err = run(t, "service", "list")
	if err != nil || !strings.Contains(out, "vms") || !strings.Contains(out, "host") {
		t.Errorf("list: %v\n%s", err, out)
	}
	if out, err := run(t, "service", "delete", "vms", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if f.a.services.Exists("vms") {
		t.Error("record left after delete")
	}
	if !slices.Equal(r.calls, []string{"start vms", "delete vms"}) {
		t.Errorf("runner calls = %v", r.calls)
	}
}

func TestServiceAttachCommand(t *testing.T) {
	f := newServiceFakes(t)
	out, err := run(t, "service", "attach", "echo", "dev", "webapp")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Attached sandbox dev/webapp to service echo at http://host.openshell.internal:41000, with the token in $ECHO_TOKEN.") {
		t.Errorf("output:\n%s", out)
	}
	out, err = run(t, "service", "show", "echo")
	if err != nil || !strings.Contains(out, "dev/webapp, since") {
		t.Errorf("show: %v\n%s", err, out)
	}
	if _, err := run(t, "service", "detach", "echo", "dev", "kit"); exitStatus(err) != exitNotFound {
		t.Errorf("detach of a sandbox not attached: %v", err)
	}
	if out, err := run(t, "service", "detach", "echo", "dev", "webapp"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if atts, _ := f.a.services.Attachments("echo"); len(atts) != 0 {
		t.Errorf("attachments = %+v", atts)
	}
}
