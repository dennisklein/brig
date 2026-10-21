// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/paths"
	"github.com/dennisklein/brig/internal/service"
	"github.com/dennisklein/brig/internal/vmrunner"
)

func newVMRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vm-runner",
		Short: "List and destroy the VMs that agents started",
		Long: `The vm runner is a service that lets agents in sandboxes start throwaway
VMs on this host, from the images that vm_runner.images in config.yaml
allows, and drive them through MCP tools: screenshots, clicks and typing,
commands and file copies through the guest agent, snapshots. Create it with
brig service create NAME --type vm and attach it to sandboxes like any
service.

These commands show and destroy those VMs. SERVICE is the vm runner
service; it may be left out when there is only one.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newVMRunnerListCmd(), newVMRunnerDestroyCmd(), newVMRunnerScreenshotCmd(), newVMRunnerServeCmd())
	return cmd
}

// vmRunnerService returns the vm runner service called name or, if name
// is empty, the only one.
func vmRunnerService(dirs paths.Dirs, name string) (*service.Service, error) {
	store := service.NewStore(dirs)
	if name != "" {
		svc, err := store.Load(name)
		if err != nil {
			return nil, err
		}
		if svc.Type != vmrunner.ServiceType {
			return nil, fmt.Errorf("service %s is of type %s, not %s", name, svc.Type, vmrunner.ServiceType)
		}
		return svc, nil
	}
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	var found []*service.Service
	for _, s := range all {
		if s.Type == vmrunner.ServiceType {
			found = append(found, s)
		}
	}
	switch len(found) {
	case 0:
		return nil, errors.New("there is no vm runner service; create one with brig service create NAME --type vm")
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, s := range found {
		names[i] = s.Name
	}
	return nil, fmt.Errorf("there are several vm runner services (%s); name one", strings.Join(names, ", "))
}

func vmRunnerClient(name string) (*vmrunner.AdminClient, error) {
	dirs, err := paths.Default()
	if err != nil {
		return nil, err
	}
	svc, err := vmRunnerService(dirs, name)
	if err != nil {
		return nil, err
	}
	return vmrunner.NewAdminClient(vmrunner.AdminSocket(os.Getenv("XDG_RUNTIME_DIR"), svc.Name)), nil
}

func optionalArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func newVMRunnerListCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:     "list [SERVICE]",
		Aliases: []string{"ls"},
		Short:   "List the VMs that agents started",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := vmRunnerClient(optionalArg(args))
			if err != nil {
				return err
			}
			vms, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			if format == "json" {
				return writeJSON(cmd.OutOrStdout(), vms)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSANDBOX\tIMAGE\tSTATE\tCPUS\tMEMORY\tDISK\tEXPIRES")
			for _, v := range vms {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", v.ID, termtext.Escape(v.Owner.String()), termtext.Escape(v.Image),
					v.State, v.CPUs, v.Memory, v.Disk, v.ExpiresAt.Local().Format(time.DateTime))
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newVMRunnerDestroyCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "destroy ID...",
		Short: "Destroy VMs that agents started",
		Long: `destroy powers the VMs off and deletes them with their disks and
snapshots, whichever sandbox started them.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := vmRunnerClient(name)
			if err != nil {
				return err
			}
			for _, id := range args {
				if err := client.Destroy(cmd.Context(), id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Destroyed VM %s\n", id)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "service", "", "the vm runner service (default: the only one)")
	return cmd
}

func newVMRunnerScreenshotCmd() *cobra.Command {
	var name, out string
	cmd := &cobra.Command{
		Use:   "screenshot ID",
		Short: "Save the screen of a VM that an agent started as PNG",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := vmRunnerClient(name)
			if err != nil {
				return err
			}
			png, err := client.Screenshot(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if out == "" {
				out = args[0] + ".png"
			}
			if err := os.WriteFile(out, png, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Saved %s\n", out)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "service", "", "the vm runner service (default: the only one)")
	cmd.Flags().StringVarP(&out, "file", "f", "", "the file to write (default: ID.png)")
	return cmd
}

func newVMRunnerServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "serve SERVICE",
		Short:  "Run the controller of a vm runner service",
		Hidden: true,
		Long: `serve runs the controller of the vm runner service until it is stopped.
brig runs it as the user unit brig-vm-runner-SERVICE.service; do not run it
yourself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			svc, err := vmRunnerService(a.dirs, args[0])
			if err != nil {
				return err
			}
			store := service.NewStore(a.dirs)
			log := vmrunner.Logger(cmd.ErrOrStderr())
			return vmrunner.Serve(cmd.Context(), vmrunner.ServeOptions{
				Options: vmrunner.Options{
					Dir:    store.Dir(svc.Name),
					Config: a.cfg.VMRunner,
					Tokens: vmrunner.NewTokens(store.TokensPath(svc.Name)),
					Log:    log,
				},
				Port:        svc.Port,
				AdminSocket: vmrunner.AdminSocket(os.Getenv("XDG_RUNTIME_DIR"), svc.Name),
			})
		},
	}
}
