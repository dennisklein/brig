// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/termtext"

	"github.com/dennisklein/brig/internal/guest"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/openshell"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/sshx"
	"github.com/dennisklein/brig/internal/ui/store"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/internal/vmnet"
)

// serviceGateway is what brig needs of one workspace of a gateway to give
// its sandboxes access to services; *openshell.Gateway is one.
type serviceGateway interface {
	Workspaces(ctx context.Context) ([]string, error)
	Profiles(ctx context.Context) ([]openshell.Profile, error)
	ImportProfile(ctx context.Context, file string) error
	UpdateProfile(ctx context.Context, id, file string) error
	DeleteProfile(ctx context.Context, id string) error
	Providers(ctx context.Context) ([]openshell.Provider, error)
	CreateProvider(ctx context.Context, name, typ string, creds map[string]string) error
	DeleteProvider(ctx context.Context, name string) error
	AttachProvider(ctx context.Context, sandbox, name string) error
	DetachProvider(ctx context.Context, sandbox, name string) error
}

// openServiceGateway returns a client for workspace ws of v's gateway; tests
// replace it.
var openServiceGateway = func(v *vm.VM, ws string) (serviceGateway, error) {
	cli, err := openshell.Find()
	if err != nil {
		return nil, err
	}
	gw, err := cli.Gateway(v.GatewayName())
	if err != nil {
		return nil, err
	}
	return gw.In(ws)
}

// inVM runs the shell script on t with stdin as its standard input; tests
// replace it.
var inVM = func(ctx context.Context, t sshx.Target, script string, stdin []byte) error {
	cmd := t.Command(ctx, "sh -c "+shellQuote(script))
	cmd.Stdin = bytes.NewReader(stdin)
	stderr := newTailWriter(5)
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("ssh %s@%s:%d: %w", t.User, t.Host, t.Port, err)
	}
	return nil
}

// relayScript starts the relay of svc in a gateway VM, unless it runs
// already: a socket-activated user unit that listens on the VM's loopback
// interface at the service's port, where OpenShell sends a sandbox's
// requests to service.SandboxHost, and passes connections on to the same
// port on the host, which the VM reaches at its default gateway. It asks
// the VM for that address each time, as it follows the host's network.
func relayScript(svc *service.Service) string {
	return fmt.Sprintf(`set -eu
unit=%s port=%d
gw=$(ip -4 route show default | awk '$1 == "default" { print $3; exit }')
if [ -z "$gw" ]; then
	echo "the VM has no IPv4 default gateway" >&2
	exit 1
fi
to=$gw:$port
if systemctl --user is-active --quiet "$unit.socket" &&
	[ "$(systemctl --user show -P Environment "$unit.service")" = "BRIG_RELAY_TO=$to" ]; then
	exit 0
fi
systemctl --user stop "$unit.socket" "$unit.service" 2>/dev/null || true
systemctl --user reset-failed "$unit.socket" "$unit.service" 2>/dev/null || true
exec systemd-run --user --quiet --unit="$unit" --description=%s \
	--setenv=BRIG_RELAY_TO="$to" --socket-property=ListenStream=127.0.0.1:"$port" \
	/usr/lib/systemd/systemd-socket-proxyd "$to"
`, service.RelayUnit(svc.Name), svc.Port, shellQuote("brig relay to service "+svc.Name))
}

// stopRelayScript stops the relay of the named service in a gateway VM.
func stopRelayScript(name string) string {
	return fmt.Sprintf(`unit=%s
systemctl --user stop "$unit.socket" "$unit.service" 2>/dev/null || true
systemctl --user reset-failed "$unit.socket" "$unit.service" 2>/dev/null || true
`, service.RelayUnit(name))
}

// serviceScript puts what brig hands a service VM below guest.ServiceDir,
// in /run, which is a tmpfs: each line of its standard input names a file,
// tokens.json or a secret, and carries its content in base64. With secrets,
// the secrets that no line names go. Unless action is empty, it then runs
// systemctl action on guest.ServiceTarget.
func serviceScript(secrets bool, action string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `set -eu
umask 077
dir=%s
secrets=%s
mkdir -p "$secrets"
chmod 0755 "$dir"
chmod 0700 "$secrets"
`, guest.ServiceDir, guest.ServiceSecretsDir)
	if secrets {
		b.WriteString("find \"$secrets\" -mindepth 1 -delete\n")
	}
	fmt.Fprintf(&b, `while read -r name value; do
	case $name in
	tokens.json) f=%s mode=0644 ;;
	*) f=$secrets/$name mode=0600 ;;
	esac
	printf '%%s' "$value" | base64 -d >"$f.new"
	chmod "$mode" "$f.new"
	mv -f "$f.new" "$f"
done
`, guest.ServiceTokensFile)
	if action != "" {
		fmt.Fprintf(&b, "systemctl %s %s\n", action, guest.ServiceTarget)
	}
	return b.String()
}

// serviceFiles is the standard input of serviceScript.
func serviceFiles(secrets map[string]string, tokens []byte) []byte {
	var b bytes.Buffer
	line := func(name string, data []byte) {
		b.WriteString(name + " " + base64.StdEncoding.EncodeToString(data) + "\n")
	}
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		line(name, []byte(secrets[name]))
	}
	if tokens != nil {
		line("tokens.json", tokens)
	}
	return b.Bytes()
}

// serviceEnv is what svc's driver gets to work with.
func (a *app) serviceEnv(svc *service.Service, t service.Type) service.Env {
	env := service.Env{Service: *svc, Type: t}
	if svc.Placement == service.PlacementVM {
		if v, err := a.vms.Load(svc.Name); err == nil {
			env.Root = a.sshTarget(v, true)
		}
	}
	env.Secret = func(ctx context.Context, name string) (string, error) {
		return a.serviceSecret(ctx, svc, name)
	}
	return env
}

// serviceSecret looks up svc's secret name where the config directories of
// its VM bind it.
func (a *app) serviceSecret(ctx context.Context, svc *service.Service, name string) (string, error) {
	if svc.Placement != service.PlacementVM {
		return "", fmt.Errorf("service %s runs on the host and has no secrets", svc.Name)
	}
	v, err := a.vms.Load(svc.Name)
	if err != nil {
		return "", err
	}
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return "", err
	}
	ss, ok := set.Services[svc.Name]
	if !ok {
		return "", fmt.Errorf("no config directory of service %s binds its secret %s in services/%[1]s.yaml; add one with brig update %[1]s --add-openshell-config DIR", svc.Name, name)
	}
	return ss.Lookup(ctx, ocsync.SecretTool{Path: a.cfg.OpenShell.SecretTool}, name)
}

// serviceSecrets looks up all secrets of svc, of type t.
func (a *app) serviceSecrets(ctx context.Context, svc *service.Service, t service.Type) (map[string]string, error) {
	secrets := map[string]string{}
	for _, name := range t.Secrets {
		v, err := a.serviceSecret(ctx, svc, name)
		if err != nil {
			return nil, err
		}
		secrets[name] = v
	}
	return secrets, nil
}

// running reports whether the VM called name runs.
func running(conn domains, name string) bool {
	state, err := conn.State((&vm.VM{Name: name}).DomainName())
	return err == nil && state == libvirt.StateRunning
}

// pushServiceTokens hands a service of placement vm whose VM runs the
// tokens of atts; the VM gets them again at its next start, and a service
// on the host reads its tokens.json itself.
func (a *app) pushServiceTokens(ctx context.Context, conn domains, svc *service.Service, atts []service.Attachment) error {
	if svc.Placement != service.PlacementVM || !running(conn, svc.Name) {
		return nil
	}
	v, err := a.vms.Load(svc.Name)
	if err != nil {
		return err
	}
	tokens, err := service.TokensJSON(atts)
	if err != nil {
		return err
	}
	if err := inVM(ctx, a.sshTarget(v, true), serviceScript(false, ""), serviceFiles(nil, tokens)); err != nil {
		return fmt.Errorf("handing service %s its tokens: %w", svc.Name, err)
	}
	return nil
}

// startService hands the started service VM v its secrets and tokens, starts
// its workload and tells its driver. With restart, it restarts the workload,
// which then reads changed secrets.
func (a *app) startService(ctx context.Context, v *vm.VM, restart bool) error {
	return inSpan(ctx, progress.KindStep, "start the service", func(ctx context.Context) error {
		svc, err := a.services.Load(v.Name)
		if err != nil {
			return err
		}
		t, err := serviceType(svc.Type)
		if err != nil {
			return err
		}
		// A keyring that waits for an unlock must not hold up the start
		// for long.
		lookupCtx, cancel := context.WithTimeout(ctx, startSyncTimeout)
		secrets, err := a.serviceSecrets(lookupCtx, svc, t)
		cancel()
		if err != nil {
			return err
		}
		atts, err := a.services.Attachments(svc.Name)
		if err != nil {
			return err
		}
		tokens, err := service.TokensJSON(atts)
		if err != nil {
			return err
		}
		action := "start"
		if restart {
			action = "restart"
		}
		if err := inVM(ctx, a.sshTarget(v, true), serviceScript(true, action), serviceFiles(secrets, tokens)); err != nil {
			return fmt.Errorf("starting service %s: %w", svc.Name, err)
		}
		if err := serviceDriver(t).Started(ctx, a.serviceEnv(svc, t)); err != nil {
			return err
		}
		if r, ok := hostRunners[t.Name]; ok {
			return r.Start(ctx, *svc)
		}
		return nil
	})
}

// stopServiceRunner stops what runs on the host next to the stopped service
// VM v, if its type has a HostRunner.
func (a *app) stopServiceRunner(ctx context.Context, v *vm.VM) error {
	if !v.IsService() {
		return nil
	}
	r, ok := hostRunners[v.Type]
	if !ok {
		return nil
	}
	svc, err := a.services.Load(v.Name)
	if err != nil {
		return err
	}
	return r.Stop(ctx, *svc)
}

// servicePortsPath is the file that lists the host ports of the services
// attached to sandboxes of the gateway VM called name, which its firewall
// lets through.
func servicePortsPath(name string) (string, error) {
	socket, err := netSocket(&vm.VM{Name: name})
	if err != nil {
		return "", err
	}
	return vmnet.ServicePortsPath(socket), nil
}

// writeServicePorts writes the service ports file of the gateway VM called
// name, from the records of all services.
func (a *app) writeServicePorts(name string) error {
	path, err := servicePortsPath(name)
	if err != nil {
		return err
	}
	svcs, err := a.services.List()
	var ports []uint16
	for _, svc := range svcs {
		atts, aerr := a.services.Attachments(svc.Name)
		err = errors.Join(err, aerr)
		if slices.ContainsFunc(atts, func(att service.Attachment) bool { return att.Gateway == name }) {
			ports = append(ports, uint16(svc.Port)) //nolint:gosec // G115: Service.check keeps it a TCP port
		}
	}
	return errors.Join(err, vmnet.WriteServicePorts(path, ports))
}

// syncServiceProfile imports the profile of svc into the workspace of g if
// it lacks it, and with update replaces the one it has.
func syncServiceProfile(ctx context.Context, g serviceGateway, svc *service.Service, t service.Type, update bool) error {
	id := service.ProfileID(svc.Name)
	profiles, err := g.Profiles(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(profiles, func(p openshell.Profile) bool {
		return p.ID == id && p.Scope == openshell.ScopeWorkspace
	})
	if i >= 0 && !update {
		return nil
	}
	data, err := service.Profile(svc, t)
	if err != nil {
		return err
	}
	if i >= 0 {
		data = fmt.Appendf(data, "resource_version: %d\n", profiles[i].ResourceVersion)
	}
	dir, err := os.MkdirTemp("", "brig-service-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, id+".yaml")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return err
	}
	if i >= 0 {
		return g.UpdateProfile(ctx, id, file)
	}
	return g.ImportProfile(ctx, file)
}

// tokenCredentials are the credentials of an attachment's provider: the
// token in every variable that the type names.
func tokenCredentials(svc *service.Service, t service.Type, token string) map[string]string {
	creds := map[string]string{}
	for _, env := range t.CredentialEnv(svc.Name) {
		creds[env] = token
	}
	return creds
}

// startHostService starts the controller of svc, of placement host.
func startHostService(ctx context.Context, svc *service.Service, t service.Type) error {
	r, err := hostRunner(t)
	if err != nil {
		return err
	}
	return r.Start(ctx, *svc)
}

// attachService gives the ready sandbox sb in workspace ws of gateway VM gw
// a token for svc, of type t: brig records the attachment, opens gw's
// firewall to the service, hands the service the token's hash, starts the
// relay in gw and stores the token as a provider attached to the sandbox.
// It undoes what it did if it fails. The caller holds the locks of gw and
// svc, in that order.
func (a *app) attachService(ctx context.Context, conn domains, svc *service.Service, t service.Type, gw *vm.VM, ws, sb string) (att service.Attachment, err error) {
	if gw.IsService() {
		return att, fmt.Errorf("VM %s is service %[1]s, not a gateway", gw.Name)
	}
	atts, err := a.services.Attachments(svc.Name)
	if err != nil {
		return att, err
	}
	if slices.ContainsFunc(atts, func(x service.Attachment) bool { return x.Is(gw.Name, ws, sb) }) {
		return att, alreadyExists(fmt.Errorf("sandbox %s of VM %s (workspace %s) is attached to service %s already", sb, gw.Name, ws, svc.Name))
	}
	s, err := findSandbox(ctx, gw, ws, sb)
	if err != nil {
		return att, err
	}
	if !s.Ready() {
		return att, notReadyf("sandbox %s is %s; start it with brig sandbox start %s %[1]s", sb, strings.ToLower(s.Phase), gw.Name)
	}
	g, err := openServiceGateway(gw, ws)
	if err != nil {
		return att, err
	}
	provider := service.ProviderName(svc.Name, sb)
	providers, err := g.Providers(ctx)
	if err != nil {
		return att, err
	}
	if slices.ContainsFunc(providers, func(p openshell.Provider) bool { return p.Name == provider }) {
		return att, alreadyExists(fmt.Errorf("the gateway of VM %s has a provider %s in workspace %s already", gw.Name, provider, ws))
	}
	if svc.Placement == service.PlacementHost {
		if err := startHostService(ctx, svc, t); err != nil {
			return att, err
		}
	}
	if err := inVM(ctx, a.sshTarget(gw, false), relayScript(svc), nil); err != nil {
		return att, fmt.Errorf("starting the relay to service %s in VM %s: %w", svc.Name, gw.Name, err)
	}
	if err := syncServiceProfile(ctx, g, svc, t, false); err != nil {
		return att, err
	}
	att = service.Attachment{Gateway: gw.Name, Workspace: ws, Sandbox: sb, Provider: provider, Created: time.Now().UTC()}
	env := a.serviceEnv(svc, t)
	d := serviceDriver(t)
	token, err := d.Issue(ctx, env, &att)
	if err != nil {
		return att, fmt.Errorf("issuing a token: %w", err)
	}
	created := false
	defer func() {
		if err == nil {
			return
		}
		// Undo it all, also when the command is interrupted.
		ctx := context.WithoutCancel(ctx)
		if created {
			_ = g.DeleteProvider(ctx, provider)
		}
		_ = d.Revoke(ctx, env, att)
		_ = a.services.SaveAttachments(svc.Name, atts)
		_ = a.writeServicePorts(gw.Name)
		_ = a.pushServiceTokens(ctx, conn, svc, atts)
	}()
	with := append(slices.Clone(atts), att)
	if err = a.services.SaveAttachments(svc.Name, with); err != nil {
		return att, err
	}
	if err = a.writeServicePorts(gw.Name); err != nil {
		return att, err
	}
	if err = a.pushServiceTokens(ctx, conn, svc, with); err != nil {
		return att, err
	}
	if err = g.CreateProvider(ctx, provider, service.ProfileID(svc.Name), tokenCredentials(svc, t, token)); err != nil {
		return att, err
	}
	created = true
	if err = g.AttachProvider(ctx, sb, provider); err != nil {
		return att, err
	}
	return att, nil
}

// detachService revokes the token of att, an attachment of svc, and then,
// with clean, removes its provider from the gateway as far as it can: a
// gateway that does not run keeps it until its next start, when brig deletes
// it. It warns on w about what it could not clean up. The caller holds the
// locks of the attachment's gateway VM, if it exists, and of svc, in that
// order.
func (a *app) detachService(ctx context.Context, conn domains, svc *service.Service, t service.Type, att service.Attachment, clean bool, w io.Writer) error {
	rerr := serviceDriver(t).Revoke(ctx, a.serviceEnv(svc, t), att)
	if rerr != nil {
		rerr = fmt.Errorf("revoking the token of %s: %w", att.Owner(), rerr)
	}
	atts, err := a.services.Attachments(svc.Name)
	if err != nil {
		return errors.Join(rerr, err)
	}
	rest := slices.DeleteFunc(slices.Clone(atts), func(x service.Attachment) bool {
		return x.Is(att.Gateway, att.Workspace, att.Sandbox)
	})
	if err := a.services.SaveAttachments(svc.Name, rest); err != nil {
		return errors.Join(rerr, err)
	}
	warn := func(err error) {
		if err != nil {
			fmt.Fprintf(w, "Warning: %s\n", termtext.Escape(err.Error()))
		}
	}
	warn(a.pushServiceTokens(ctx, conn, svc, rest))
	warn(a.writeServicePorts(att.Gateway))
	if !clean {
		return rerr
	}
	gw, err := a.vms.Load(att.Gateway)
	switch {
	case errors.Is(err, vm.ErrNotFound):
	case err != nil:
		warn(err)
	case !running(conn, gw.Name):
		fmt.Fprintf(w, "VM %s does not run; brig deletes provider %s at its next start.\n", gw.Name, att.Provider)
	default:
		warn(a.removeFromGateway(ctx, gw, svc.Name, att, rest))
	}
	return rerr
}

// removeFromGateway detaches and deletes the provider of att, an attachment
// of the named service, and the service's profile and relay once rest, the
// service's other attachments, need them no longer.
func (a *app) removeFromGateway(ctx context.Context, gw *vm.VM, name string, att service.Attachment, rest []service.Attachment) error {
	g, err := openServiceGateway(gw, att.Workspace)
	if err != nil {
		return err
	}
	// The sandbox may be gone, which detaches the provider.
	derr := g.DetachProvider(ctx, att.Sandbox, att.Provider)
	if err := g.DeleteProvider(ctx, att.Provider); err != nil {
		return fmt.Errorf("deleting provider %s from the gateway of VM %s: %w", att.Provider, gw.Name, errors.Join(derr, err))
	}
	var errs []error
	if !slices.ContainsFunc(rest, func(x service.Attachment) bool { return x.Gateway == gw.Name && x.Workspace == att.Workspace }) {
		if err := g.DeleteProfile(ctx, service.ProfileID(name)); err != nil {
			errs = append(errs, fmt.Errorf("deleting profile %s from the gateway of VM %s: %w", service.ProfileID(name), gw.Name, err))
		}
	}
	if !slices.ContainsFunc(rest, func(x service.Attachment) bool { return x.Gateway == gw.Name }) {
		if err := inVM(ctx, a.sshTarget(gw, false), stopRelayScript(name), nil); err != nil {
			errs = append(errs, fmt.Errorf("stopping the relay to service %s in VM %s: %w", name, gw.Name, err))
		}
	}
	return errors.Join(errs...)
}

// gatewayAttachments returns the services with their attachments to
// sandboxes of the gateway VM called name.
func (a *app) gatewayAttachments(name string) (map[string][]service.Attachment, []*service.Service, error) {
	svcs, err := a.services.List()
	byService := map[string][]service.Attachment{}
	var with []*service.Service
	for _, svc := range svcs {
		atts, aerr := a.services.Attachments(svc.Name)
		if aerr != nil {
			err = errors.Join(err, aerr)
			continue
		}
		for _, att := range atts {
			if att.Gateway == name {
				byService[svc.Name] = append(byService[svc.Name], att)
			}
		}
		if len(byService[svc.Name]) > 0 {
			with = append(with, svc)
		}
	}
	return byService, with, err
}

// revokeGateway revokes the tokens of all sandboxes of the gateway VM v,
// which brig delete is removing, so it leaves v's gateway as it is. The
// caller holds v's lock.
func (a *app) revokeGateway(ctx context.Context, conn domains, v *vm.VM, w io.Writer) error {
	byService, svcs, err := a.gatewayAttachments(v.Name)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, svc := range svcs {
		unlock, err := a.services.Lock(svc.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t, err := serviceType(svc.Type)
		if err == nil {
			for _, att := range byService[svc.Name] {
				errs = append(errs, a.detachService(ctx, conn, svc, t, att, false, w))
			}
		}
		errs = append(errs, err)
		unlock()
	}
	return errors.Join(errs...)
}

// detachSandbox revokes the tokens of the sandbox sb in workspace ws of the
// gateway VM v, which brig sandbox delete is deleting. The caller holds v's
// lock.
func (a *app) detachSandbox(ctx context.Context, conn domains, v *vm.VM, ws, sb string, w io.Writer) error {
	byService, svcs, err := a.gatewayAttachments(v.Name)
	errs := []error{err}
	for _, svc := range svcs {
		i := slices.IndexFunc(byService[svc.Name], func(x service.Attachment) bool { return x.Is(v.Name, ws, sb) })
		if i < 0 {
			continue
		}
		unlock, err := a.services.Lock(svc.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t, err := serviceType(svc.Type)
		if err == nil {
			err = a.detachService(ctx, conn, svc, t, byService[svc.Name][i], true, w)
		}
		errs = append(errs, err)
		unlock()
	}
	return errors.Join(errs...)
}

// reconcileServices brings what the started gateway VM v holds for services
// in line with brig's records: each service with a sandbox of v attached
// gets its relay, its controller if it runs on the host, and its profile, as
// this brig generates it, in each workspace with such a sandbox. A sandbox
// that is gone loses its token; one whose provider is gone gets a new token.
// Providers and profiles of services that no attachment needs any more go.
// The caller holds v's lock.
func (a *app) reconcileServices(ctx context.Context, conn domains, v *vm.VM, w io.Writer) error {
	byService, svcs, err := a.gatewayAttachments(v.Name)
	errs := []error{err}
	var sandboxes []store.Sandbox
	if len(svcs) > 0 {
		// Without the list, no sandbox counts as gone.
		if sandboxes, err = gatewaySandboxes(ctx, v); err != nil {
			errs = append(errs, err)
			sandboxes = nil
		}
	}
	g, err := openServiceGateway(v, openshell.DefaultWorkspace)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	workspaces, err := g.Workspaces(ctx)
	if err != nil {
		errs = append(errs, err)
		workspaces = []string{openshell.DefaultWorkspace}
	}
	for _, list := range byService {
		for _, att := range list {
			if !slices.Contains(workspaces, att.Workspace) {
				workspaces = append(workspaces, att.Workspace)
			}
		}
	}
	for _, svc := range svcs {
		unlock, err := a.services.Lock(svc.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs = append(errs, a.reconcileService(ctx, conn, v, svc, byService[svc.Name], sandboxes, w))
		unlock()
	}
	// What the service's records no longer name is left over from a
	// detach while the gateway did not run, or from a deleted service.
	byService, _, err = a.gatewayAttachments(v.Name)
	errs = append(errs, err)
	for _, ws := range workspaces {
		errs = append(errs, a.pruneServiceObjects(ctx, v, ws, byService))
	}
	return errors.Join(errs...)
}

// reconcileService is reconcileServices for svc, whose attachments to
// sandboxes of v are atts. sandboxes lists v's sandboxes; nil means that
// brig could not list them.
func (a *app) reconcileService(ctx context.Context, conn domains, v *vm.VM, svc *service.Service, atts []service.Attachment, sandboxes []store.Sandbox, w io.Writer) error {
	t, err := serviceType(svc.Type)
	if err != nil {
		return err
	}
	var errs []error
	var live []service.Attachment
	for _, att := range atts {
		gone := sandboxes != nil && !slices.ContainsFunc(sandboxes, func(s store.Sandbox) bool {
			return s.Workspace == att.Workspace && s.Name == att.Sandbox
		})
		if !gone {
			live = append(live, att)
			continue
		}
		fmt.Fprintf(w, "Sandbox %s is gone; revoking its token for service %s.\n", att.Owner(), svc.Name)
		errs = append(errs, a.detachService(ctx, conn, svc, t, att, true, w))
	}
	if len(live) == 0 {
		return errors.Join(errs...)
	}
	if svc.Placement == service.PlacementHost {
		if err := startHostService(ctx, svc, t); err != nil {
			errs = append(errs, fmt.Errorf("starting service %s: %w", svc.Name, err))
		}
	}
	if err := inVM(ctx, a.sshTarget(v, false), relayScript(svc), nil); err != nil {
		errs = append(errs, fmt.Errorf("starting the relay to service %s: %w", svc.Name, err))
	}
	synced := map[string]bool{}
	for _, att := range live {
		g, err := openServiceGateway(v, att.Workspace)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !synced[att.Workspace] {
			synced[att.Workspace] = true
			if err := syncServiceProfile(ctx, g, svc, t, true); err != nil {
				errs = append(errs, fmt.Errorf("profile of service %s in workspace %s: %w", svc.Name, att.Workspace, err))
				continue
			}
		}
		providers, err := g.Providers(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if slices.ContainsFunc(providers, func(p openshell.Provider) bool { return p.Name == att.Provider }) {
			continue
		}
		fmt.Fprintf(w, "The gateway lost the token of %s for service %s; issuing a new one.\n", att.Owner(), svc.Name)
		errs = append(errs, a.reissue(ctx, conn, g, svc, t, att))
	}
	return errors.Join(errs...)
}

// reissue gives att, whose provider is gone from its gateway g, a new token.
func (a *app) reissue(ctx context.Context, conn domains, g serviceGateway, svc *service.Service, t service.Type, att service.Attachment) error {
	d := serviceDriver(t)
	env := a.serviceEnv(svc, t)
	if err := d.Revoke(ctx, env, att); err != nil {
		return err
	}
	atts, err := a.services.Attachments(svc.Name)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(atts, func(x service.Attachment) bool { return x.Is(att.Gateway, att.Workspace, att.Sandbox) })
	if i < 0 {
		return nil
	}
	fresh := att
	fresh.SHA256 = ""
	token, err := d.Issue(ctx, env, &fresh)
	if err != nil {
		return err
	}
	atts[i] = fresh
	if err := a.services.SaveAttachments(svc.Name, atts); err != nil {
		return err
	}
	if err := a.pushServiceTokens(ctx, conn, svc, atts); err != nil {
		return err
	}
	if err := g.CreateProvider(ctx, att.Provider, service.ProfileID(svc.Name), tokenCredentials(svc, t, token)); err != nil {
		return err
	}
	return g.AttachProvider(ctx, att.Sandbox, att.Provider)
}

// pruneServiceObjects deletes the providers and profiles for services in
// workspace ws of v's gateway that no attachment in byService needs.
func (a *app) pruneServiceObjects(ctx context.Context, v *vm.VM, ws string, byService map[string][]service.Attachment) error {
	g, err := openServiceGateway(v, ws)
	if err != nil {
		return err
	}
	keepProviders, keepProfiles := map[string]bool{}, map[string]bool{}
	for name, atts := range byService {
		for _, att := range atts {
			if att.Workspace == ws {
				keepProviders[att.Provider] = true
				keepProfiles[service.ProfileID(name)] = true
			}
		}
	}
	var errs []error
	providers, err := g.Providers(ctx)
	if err != nil {
		return err
	}
	for _, p := range providers {
		if strings.HasPrefix(p.Name, ocsync.ReservedPrefix) && !keepProviders[p.Name] {
			if err := g.DeleteProvider(ctx, p.Name); err != nil {
				errs = append(errs, fmt.Errorf("deleting provider %s, which no service needs: %w", p.Name, err))
			}
		}
	}
	profiles, err := g.Profiles(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, p := range profiles {
		if p.Scope == openshell.ScopeWorkspace && strings.HasPrefix(p.ID, ocsync.ReservedPrefix) && !keepProfiles[p.ID] {
			if err := g.DeleteProfile(ctx, p.ID); err != nil {
				errs = append(errs, fmt.Errorf("deleting profile %s, which no service needs: %w", p.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}
