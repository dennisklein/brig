// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/ocsync"
	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/ports"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/ui/action"
	"github.com/dennisklein/brig/internal/vm"
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run services that sandboxes use, each with a token of its own",
		Long: `A service is a workload that agents in sandboxes may use but never control,
such as a model proxy that holds the API key. Its type, built into brig,
decides what runs and where: most run in a VM of their own, which brig
creates from the base image like a gateway VM, with or without OpenShell.

brig service attach gives one sandbox a token for the service. brig keeps
only the token's hash, which the service checks callers against, and stores
the token as an OpenShell provider on the sandbox's gateway, attached to the
sandbox. The agent sees a placeholder; OpenShell adds the token to the
requests that the type's programs send to the service, at
http://host.openshell.internal:PORT, and to no others. A relay in the
gateway VM passes them on to the service's port on the host, and the
gateway's firewall lets them through.

Service VMs are VMs like others for brig start, stop, show, ssh, update
and sync, but brig list leaves them out and brig delete refuses them: use
brig service list and delete. The secrets of a service go from your keyring
into its VM's memory at every start: bind them in services/NAME.yaml of a
config directory of the service (see brig sync --help).`,
	}
	cmd.AddCommand(newServiceTypesCmd(), newServiceCreateCmd(), newServiceListCmd(), newServiceShowCmd(),
		newServiceStartCmd(), newServiceStopCmd(), newServiceAttachCmd(), newServiceDetachCmd(), newServiceDeleteCmd())
	return cmd
}

func newServiceTypesCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:   "types",
		Short: "List the service types that this brig has",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			types := serviceTypes().Types()
			if format == "json" {
				if types == nil {
					types = []service.Type{}
				}
				return writeJSON(cmd.OutOrStdout(), types)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tPLACEMENT\tOPENSHELL\tDESCRIPTION")
			for _, t := range types {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%s\n", t.Name, t.Kind, t.Placement, t.OpenShell, t.Description)
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newServiceCreateCmd() *cobra.Command {
	var (
		typeName, profile string
		cpus              int
		memory            bytesize.Size
		binaries, configs []string
		noStart, wait     bool
		format            outputFormat
	)
	cmd := &cobra.Command{
		Use:   "create NAME --type TYPE",
		Short: "Create and start a service",
		Long: `Create a service of a type that brig service types lists, and start it. A
service of placement vm gets a VM called NAME, made like brig create makes
one; its CPUs, memory and network profile come from the flags, else from the
type, else from config.yaml. Its OpenShell config directories are only the
ones that --openshell-config names, never openshell.configs from config.yaml,
so that the service VM holds no secrets but its own. They bind the
service's secrets in services/NAME.yaml.

brig picks a port on the host's loopback interface for the service, which
stays the same for its life: gateways reach the service there, and
sandboxes at http://host.openshell.internal:PORT.

--binary replaces the type's programs that may call the service from a
sandbox; OpenShell adds a sandbox's token only to their requests.`,
		Example: `  brig service create mirror --type http-mirror --openshell-config ~/brig/mirror`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			name := args[0]
			if err := vm.ValidateName(name); err != nil {
				return err
			}
			t, err := serviceType(typeName)
			if err != nil {
				return notFound(err)
			}
			if a.services.Exists(name) || a.vms.Exists(name) {
				return alreadyExists(fmt.Errorf("a service or VM called %s exists already", name))
			}
			f := cmd.Flags()
			if t.Placement == service.PlacementHost {
				for _, flag := range []string{"profile", "cpus", "memory", "openshell-config", "wait"} {
					if f.Changed(flag) {
						return fmt.Errorf("--%s: a service of type %s runs on the host, not in a VM", flag, t.Name)
					}
				}
			}
			port, err := ports.Free()
			if err != nil {
				return err
			}
			svc := &service.Service{Name: name, Type: t.Name, Placement: t.Placement, Port: port, Binaries: binaries, CreatedAt: time.Now().UTC()}
			var v *vm.VM
			if t.Placement == service.PlacementVM {
				if v, err = a.serviceVM(svc, t, configs); err != nil {
					return err
				}
				if f.Changed("cpus") {
					v.CPUs = cpus
				}
				if f.Changed("memory") {
					v.Memory = memory
				}
				if f.Changed("profile") {
					v.NetworkProfile = profile
				}
				if _, err := a.cfg.Profile(v.NetworkProfile); err != nil {
					return err
				}
				if err := checkVM(v); err != nil {
					return err
				}
				if err := checkHost(cmd.Context(), cmd.ErrOrStderr()); err != nil {
					return err
				}
			}
			unlock, err := a.services.Lock(name)
			if err != nil {
				return err
			}
			defer unlock()
			if a.services.Exists(name) {
				return alreadyExists(fmt.Errorf("service %s exists already", name))
			}
			if err := a.services.Save(svc); err != nil {
				return err
			}
			if err := a.createService(cmd, svc, t, v, !noStart, waitReadiness(wait), resultWriter(cmd, format)); err != nil {
				return err
			}
			return a.printService(cmd.Context(), cmd.OutOrStdout(), svc, format)
		},
	}
	f := cmd.Flags()
	f.StringVar(&typeName, "type", "", "the service's type (see brig service types)")
	_ = cmd.MarkFlagRequired("type")
	_ = cmd.RegisterFlagCompletionFunc("type", completeServiceTypes)
	f.IntVar(&cpus, "cpus", 0, "number of virtual CPUs of the service VM")
	f.Var(&memory, "memory", "memory size of the service VM, e.g. 4GiB")
	f.StringVar(&profile, "profile", "", "network profile of the service VM (see config.yaml)")
	_ = cmd.RegisterFlagCompletionFunc("profile", completeProfiles)
	f.StringArrayVar(&binaries, "binary", nil, "absolute path of a program that may call the service from a sandbox (repeatable; default: the type's)")
	f.StringArrayVar(&configs, "openshell-config", nil, "OpenShell config directory of the service VM (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("openshell-config", completeDirs)
	f.BoolVar(&noStart, "no-start", false, "create the service without starting it")
	addWaitFlag(cmd, &wait)
	cmd.MarkFlagsMutuallyExclusive("no-start", "wait")
	addOutputFlag(cmd, &format)
	hideDefaults(cmd, "memory")
	return cmd
}

// serviceVM returns the record of a new VM for svc, of type t, with the
// config directories configs.
func (a *app) serviceVM(svc *service.Service, t service.Type, configs []string) (*vm.VM, error) {
	d := a.cfg.Defaults
	v := &vm.VM{
		Name: svc.Name, CreatedAt: svc.CreatedAt, Role: vm.RoleService, Type: t.Name,
		CPUs: d.CPUs, Memory: d.Memory, RootDisk: d.RootDisk, DataDisk: d.DataDisk,
		NetworkProfile: d.NetworkProfile,
	}
	if t.Resources.CPUs > 0 {
		v.CPUs = t.Resources.CPUs
	}
	if t.Resources.Memory > 0 {
		v.Memory = t.Resources.Memory
	}
	if t.NetworkProfile != "" {
		v.NetworkProfile = t.NetworkProfile
	}
	var err error
	if v.OpenShellConfigs, err = configDirs(configs); err != nil {
		return nil, err
	}
	set, err := ocsync.Load(v.OpenShellConfigs)
	if err != nil {
		return nil, err
	}
	if len(t.Secrets) > 0 {
		if _, ok := set.Services[svc.Name]; !ok {
			return nil, fmt.Errorf("service type %s needs the secrets %s: bind them in services/%s.yaml of a config directory given with --openshell-config", t.Name, strings.Join(t.Secrets, ", "), svc.Name)
		}
	}
	if v.RootDisk < config.MinRootDisk || v.DataDisk < config.MinDataDisk {
		return nil, fmt.Errorf("a VM needs a root disk of at least %s and a data disk of at least %s", config.MinRootDisk, config.MinDataDisk)
	}
	return v, nil
}

// createService creates what runs svc, whose record is saved, and with start
// starts it: the VM v for placement vm, the controller for placement host.
// If it fails before the VM exists, it removes the record again. The caller
// holds svc's lock.
func (a *app) createService(cmd *cobra.Command, svc *service.Service, t service.Type, v *vm.VM, start bool, ready readiness, result io.Writer) (err error) {
	ctx := cmd.Context()
	defer func() {
		if err != nil && (v == nil || !a.vms.Exists(v.Name)) {
			_ = a.services.Remove(svc.Name)
		}
	}()
	if v == nil {
		if start {
			if err := startHostService(ctx, svc, t); err != nil {
				return err
			}
		}
		fmt.Fprintf(result, "Created service %s of type %s on the host.\n", svc.Name, t.Name)
		return nil
	}
	if err := a.create(ctx, cmd, v, start, ready, result); err != nil {
		return err
	}
	fmt.Fprintf(result, "Created service %s of type %s.\n", svc.Name, t.Name)
	return nil
}

// serviceStatus is a service's record with its state and attachments, as
// brig service list and show print them.
type serviceStatus struct {
	*service.Service
	// State is the state of the service's VM, or "host".
	State       string               `json:"state"`
	URL         string               `json:"url"`
	Attachments []service.Attachment `json:"attachments"`
}

// serviceStatuses returns the states of svcs, and an error about the
// states and attachments it cannot read.
func (a *app) serviceStatuses(ctx context.Context, svcs []*service.Service) ([]serviceStatus, error) {
	var conn domains
	var errs []error
	for _, svc := range svcs {
		if svc.Placement == service.PlacementVM && conn == nil {
			c, err := connectDomains(ctx, a)
			if err != nil {
				errs = append(errs, err)
				break
			}
			defer func() { _ = c.Close() }()
			conn = c
		}
	}
	out := make([]serviceStatus, 0, len(svcs))
	for _, svc := range svcs {
		st := serviceStatus{Service: svc, State: "unknown", URL: svc.URL()}
		switch {
		case svc.Placement == service.PlacementHost:
			st.State = "host"
		case conn != nil:
			if s, err := conn.State((&vm.VM{Name: svc.Name}).DomainName()); err != nil {
				errs = append(errs, err)
			} else {
				st.State = s.String()
			}
		}
		atts, err := a.services.Attachments(svc.Name)
		errs = append(errs, err)
		if atts == nil {
			atts = []service.Attachment{}
		}
		st.Attachments = atts
		out = append(out, st)
	}
	return out, errors.Join(errs...)
}

func newServiceListCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List services",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			svcs, err := a.services.List()
			sts, serr := a.serviceStatuses(cmd.Context(), svcs)
			if err := errors.Join(err, serr); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", termtext.Escape(err.Error()))
			}
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), sts)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTYPE\tSTATE\tPORT\tATTACHED")
			for _, st := range sts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", st.Name, st.Type, st.State, st.Port, len(st.Attachments))
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newServiceShowCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:               "show NAME",
		Short:             "Show a service and the sandboxes attached to it",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := a.loadService(args[0])
			if err != nil {
				return err
			}
			return a.printService(cmd.Context(), cmd.OutOrStdout(), svc, format)
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

// loadService loads the named service's record.
func (a *app) loadService(name string) (*service.Service, error) {
	svc, err := a.services.Load(name)
	if errors.Is(err, service.ErrNotFound) {
		return nil, notFound(err)
	}
	return svc, err
}

// printService prints svc as brig service show does.
func (a *app) printService(ctx context.Context, w io.Writer, svc *service.Service, format outputFormat) error {
	sts, err := a.serviceStatuses(ctx, []*service.Service{svc})
	if len(sts) == 0 {
		return err
	}
	st := sts[0]
	if format == "json" {
		return errors.Join(writeJSON(w, st), err)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v any) { fmt.Fprintf(tw, "%v:\t%s\n", k, termtext.Escape(fmt.Sprint(v))) }
	row("Name", svc.Name)
	row("Type", svc.Type)
	row("State", st.State)
	row("Created", svc.CreatedAt.Local().Format(time.DateTime))
	row("Host port", fmt.Sprintf("127.0.0.1:%d", svc.Port))
	row("URL in sandboxes", st.URL)
	if t, terr := serviceType(svc.Type); terr == nil {
		row("Token in", strings.Join(t.CredentialEnv(svc.Name), ", "))
		row("Callers", strings.Join(svc.CallerBinaries(t), ", "))
	}
	if len(st.Attachments) == 0 {
		row("Attached", "none")
	}
	for i, att := range st.Attachments {
		k := ""
		if i == 0 {
			k = "Attached"
		}
		fmt.Fprintf(tw, "%s\t%s\n", k+":", termtext.Escape(att.Owner()+", since "+att.Created.Local().Format(time.DateTime)))
	}
	return errors.Join(tw.Flush(), err)
}

func newServiceStartCmd() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:               "start NAME",
		Short:             "Start a service",
		Long:              `Start a service: its VM, as brig start does, or its controller on the host.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := a.loadService(args[0])
			if err != nil {
				return err
			}
			if svc.Placement == service.PlacementVM {
				v, err := a.vms.Load(svc.Name)
				if err != nil {
					return err
				}
				return a.lockAndStart(cmd.Context(), v, cmd.ErrOrStderr(), waitReadiness(wait))
			}
			t, err := serviceType(svc.Type)
			if err != nil {
				return err
			}
			return startHostService(cmd.Context(), svc, t)
		},
	}
	addWaitFlag(cmd, &wait)
	return cmd
}

func newServiceStopCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:               "stop NAME",
		Short:             "Stop a service",
		Long:              `Stop a service: shut its VM down, as brig stop does, or stop its controller on the host. Sandboxes keep their tokens.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := a.loadService(args[0])
			if err != nil {
				return err
			}
			if svc.Placement == service.PlacementHost {
				t, err := serviceType(svc.Type)
				if err != nil {
					return err
				}
				r, err := hostRunner(t)
				if err != nil {
					return err
				}
				return r.Stop(ctx, *svc)
			}
			v, err := a.vms.Load(svc.Name)
			if err != nil {
				return err
			}
			unlock, err := a.lockLoadedVM(v)
			if err != nil {
				return err
			}
			defer unlock()
			conn, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			return a.stop(ctx, conn, v, force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "power the VM off immediately instead of shutting it down gracefully")
	return cmd
}

// domainsConn is a libvirt connection as the service commands use it.
type domainsConn interface {
	domains
	Close() error
}

// connectDomains connects to libvirt for the service commands; tests
// replace it.
var connectDomains = func(ctx context.Context, a *app) (domainsConn, error) {
	return a.connect(ctx)
}

// serviceSandboxArgs checks the arguments SERVICE [VM] SANDBOX.
func serviceSandboxArgs(_ *cobra.Command, args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return fmt.Errorf("accepts SERVICE [VM] SANDBOX, received %d arguments", len(args))
	}
	if sb := args[len(args)-1]; !action.ValidSandboxName(sb) {
		return fmt.Errorf("invalid sandbox name %q", sb)
	}
	return nil
}

// serviceSandbox is what brig service attach and detach work on, with the
// locks of the gateway VM and the service held.
type serviceSandbox struct {
	svc     *service.Service
	t       service.Type
	gw      *vm.VM
	ws, sb  string
	conn    domainsConn
	release func()
}

// lockServiceSandbox resolves the arguments SERVICE [VM] SANDBOX and takes
// the locks of the gateway VM and of the service, in that order.
func (a *app) lockServiceSandbox(cmd *cobra.Command, args []string, workspace string) (*serviceSandbox, error) {
	ws, err := chosenWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	if _, err := a.loadService(args[0]); err != nil {
		return nil, err
	}
	gw, err := a.resolveVMArg(cmd, args[1:len(args)-1])
	if err != nil {
		return nil, err
	}
	unlockVM, err := a.lockLoadedVM(gw)
	if err != nil {
		return nil, err
	}
	unlockSvc, err := a.services.Lock(args[0])
	if err != nil {
		unlockVM()
		return nil, err
	}
	s := &serviceSandbox{gw: gw, ws: ws, sb: args[len(args)-1], release: func() { unlockSvc(); unlockVM() }}
	if s.svc, err = a.loadService(args[0]); err == nil {
		if s.t, err = serviceType(s.svc.Type); err == nil {
			s.conn, err = connectDomains(cmd.Context(), a)
		}
	}
	if err != nil {
		s.release()
		return nil, err
	}
	release := s.release
	s.release = func() { _ = s.conn.Close(); release() }
	return s, nil
}

func newServiceAttachCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "attach SERVICE [VM] SANDBOX",
		Short: "Give a sandbox a token for a service",
		Long: `Give a ready sandbox of a VM a token of its own for a service. The VM
defaults to the one that brig use selected, and the workspace to the one
that -w or BRIG_WORKSPACE names, else default.

brig mints the token, hands the service its hash, and stores the token as
the provider brig-svc-SERVICE-SANDBOX of the gateway, with a profile
brig-svc-SERVICE that brig generates, attached to the sandbox. Programs
that the sandbox starts afterwards find a placeholder in the variables that
brig service show lists; OpenShell puts the token in its place when one of
the service's callers sends a request to the service's URL.`,
		Example: `  brig service attach mirror dev webapp`,
		Args:    serviceSandboxArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			s, err := a.lockServiceSandbox(cmd, args, workspace)
			if err != nil {
				return err
			}
			defer s.release()
			att, err := a.attachService(cmd.Context(), s.conn, s.svc, s.t, s.gw, s.ws, s.sb)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Attached sandbox %s to service %s at %s, with the token in $%s.\n",
				termtext.Escape(att.Owner()), s.svc.Name, s.svc.URL(), strings.Join(s.t.CredentialEnv(s.svc.Name), ", $"))
			return nil
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	cmd.ValidArgsFunction = completeServices
	return cmd
}

func newServiceDetachCmd() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "detach SERVICE [VM] SANDBOX",
		Short: "Take a sandbox's token for a service away",
		Long: `Revoke a sandbox's token for a service, so that the service refuses it, and
delete its provider from the gateway. If the VM does not run, its gateway
keeps the provider until the VM's next start; the service refuses the token
at once.`,
		Args: serviceSandboxArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			s, err := a.lockServiceSandbox(cmd, args, workspace)
			if err != nil {
				return err
			}
			defer s.release()
			atts, err := a.services.Attachments(s.svc.Name)
			if err != nil {
				return err
			}
			i := slices.IndexFunc(atts, func(att service.Attachment) bool { return att.Is(s.gw.Name, s.ws, s.sb) })
			if i < 0 {
				return notFound(fmt.Errorf("sandbox %s of VM %s (workspace %s) is not attached to service %s", s.sb, s.gw.Name, s.ws, s.svc.Name))
			}
			if err := a.detachService(cmd.Context(), s.conn, s.svc, s.t, atts[i], true, cmd.ErrOrStderr()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Detached sandbox %s from service %s.\n", termtext.Escape(atts[i].Owner()), s.svc.Name)
			return nil
		},
	}
	addWorkspaceFlag(cmd, &workspace)
	cmd.ValidArgsFunction = completeServices
	return cmd
}

func newServiceDeleteCmd() *cobra.Command {
	var force, yes bool
	cmd := &cobra.Command{
		Use:     "delete NAME",
		Aliases: []string{"rm"},
		Short:   "Delete a service, its VM and its tokens",
		Long: `Delete a service: revoke the tokens of all sandboxes attached to it, delete
their providers from the gateways that run, and delete the service's VM with
all its data, or what its controller on the host made.

On a terminal, delete asks first; --yes skips the question. Without a
terminal on standard input, as in a script, it deletes without asking.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeServices,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stdout, stderr := cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr()
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := a.loadService(args[0])
			if err != nil {
				return err
			}
			atts, err := a.services.Attachments(svc.Name)
			if err != nil {
				return err
			}
			if confirm := confirmer(ctx, cmd.InOrStdin(), stderr, yes); confirm != nil {
				fmt.Fprintf(stderr, "Deleting service %s revokes the tokens of %d sandboxes", svc.Name, len(atts))
				if svc.Placement == service.PlacementVM {
					fmt.Fprintf(stderr, " and deletes VM %s with all its data", svc.Name)
				}
				fmt.Fprintln(stderr, ".")
				ok, err := confirm("Delete service " + svc.Name + "? [y/N] ")
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("service %s not deleted", svc.Name)
				}
			}
			release, err := a.lockServiceDelete(svc.Name, atts)
			if err != nil {
				return err
			}
			defer release()
			conn, err := connectDomains(ctx, a)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()
			if err := a.deleteService(ctx, conn, svc.Name, force, stdout, stderr); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Deleted service %s.\n", svc.Name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "delete a service whose VM runs")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// lockServiceDelete takes the locks that deleting the named service, with
// the attachments atts, needs: those of the gateway VMs of atts and of the
// service's VM, and then the service's.
func (a *app) lockServiceDelete(name string, atts []service.Attachment) (func(), error) {
	var unlocks []func()
	release := func() {
		for _, u := range slices.Backward(unlocks) {
			u()
		}
	}
	names := []string{name}
	for _, att := range atts {
		names = append(names, att.Gateway)
	}
	slices.Sort(names)
	for _, n := range slices.Compact(names) {
		u, err := a.lockVM(n)
		if err != nil {
			release()
			return nil, err
		}
		unlocks = append(unlocks, u)
	}
	u, err := a.services.Lock(name)
	if err != nil {
		release()
		return nil, err
	}
	unlocks = append(unlocks, u)
	return release, nil
}

// deleteService deletes the named service: it detaches every sandbox, then
// removes the service's VM or what its controller made, and its record. The
// caller holds the locks that lockServiceDelete takes.
func (a *app) deleteService(ctx context.Context, conn domains, name string, force bool, stdout, stderr io.Writer) error {
	svc, err := a.loadService(name)
	if err != nil {
		return err
	}
	t, err := serviceType(svc.Type)
	if err != nil {
		return err
	}
	var v *vm.VM
	if svc.Placement == service.PlacementVM {
		v, err = a.vms.Load(svc.Name)
		switch {
		case errors.Is(err, vm.ErrNotFound):
			v = nil
		case err != nil:
			return err
		default:
			if state, err := conn.State(v.DomainName()); err != nil {
				return err
			} else if state != libvirt.StateShutoff && state != libvirt.StateMissing && !force {
				return fmt.Errorf("service %s is %s; stop it first (brig service stop %[1]s) or use --force", svc.Name, state)
			}
		}
	}
	atts, err := a.services.Attachments(svc.Name)
	if err != nil {
		return err
	}
	var errs []error
	for _, att := range atts {
		errs = append(errs, a.detachService(ctx, conn, svc, t, att, true, stderr))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("service %s not deleted: %w", svc.Name, err)
	}
	if v != nil {
		if err := a.remove(ctx, conn, v, true, nil, stdout, stderr); err != nil {
			return err
		}
	}
	if svc.Placement == service.PlacementHost {
		if _, err := hostRunner(t); err != nil {
			return err
		}
	}
	if r, ok := hostRunners[t.Name]; ok {
		if err := r.Delete(ctx, *svc); err != nil {
			return err
		}
	}
	return a.services.Remove(svc.Name)
}

// completeServices completes the first argument with service names.
func completeServices(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	dirs, err := paths.Default()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	svcs, _ := service.NewStore(dirs).List()
	names := make([]string, 0, len(svcs))
	for _, svc := range svcs {
		names = append(names, svc.Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeServiceTypes completes service type names.
func completeServiceTypes(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return serviceTypes().Names(), cobra.ShellCompDirectiveNoFileComp
}
