// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/image"
	"github.com/dennisklein/brig/internal/vm"
	"github.com/dennisklein/brig/packaging"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Manage base images and container images in VMs",
	}
	cmd.AddCommand(newImageBuildCmd(), newImageListCmd(), newImageRemoveCmd(), newImagePruneCmd(), newImagePushCmd())
	return cmd
}

// buildImage builds an image with o, adding brig's package repository and
// logging to cmd's standard error.
func (a *app) buildImage(ctx context.Context, cmd *cobra.Command, o image.BuildOptions) (image.Image, error) {
	var err error
	if o.RepoFile, o.RepoKey, err = packaging.Repo(); err != nil {
		return image.Image{}, err
	}
	o.Log = cmd.ErrOrStderr()
	img, built, err := a.images.Build(ctx, a.dirs.MkosiCacheDir(), o)
	if err != nil {
		return image.Image{}, fmt.Errorf("building base image: %w", err)
	}
	// With o.IfNone, Build may return an image that another build added
	// while this one waited for the lock.
	if built {
		fmt.Fprintf(cmd.OutOrStdout(), "Built image %s (Fedora %d, OpenShell %s).\n", img.ID, img.FedoraRelease, img.OpenShellVersion)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Using existing image %s (Fedora %d, OpenShell %s).\n", img.ID, img.FedoraRelease, img.OpenShellVersion)
	}
	return img, nil
}

func newImageBuildCmd() *cobra.Command {
	var (
		fedora    int
		openshell string
	)
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build a base image with mkosi",
		Long: `Build a base image with mkosi: Fedora with OpenShell from brig's signed
package repository. Building needs no root privileges but takes a while.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("fedora") {
				fedora = a.cfg.Defaults.FedoraRelease
			}
			_, err = a.buildImage(cmd.Context(), cmd, image.BuildOptions{FedoraRelease: fedora, OpenShellVersion: openshell})
			return err
		},
	}
	cmd.Flags().IntVar(&fedora, "fedora", 0, "Fedora release (default from config.yaml)")
	cmd.Flags().StringVar(&openshell, "openshell", "", "OpenShell version, e.g. 0.1.2 (default: newest in the repository)")
	return cmd
}

// imageUsers maps image IDs to the VMs using them.
func (a *app) imageUsers() (map[string][]string, error) {
	vms, err := a.vms.List()
	if err != nil {
		return nil, err
	}
	users := map[string][]string{}
	for _, v := range vms {
		users[v.Image] = append(users[v.Image], v.Name)
	}
	return users, nil
}

func newImageListCmd() *cobra.Command {
	var format outputFormat
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List base images, newest first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			imgs, err := a.images.List()
			if err != nil {
				return err
			}
			users, err := a.imageUsers()
			if err != nil {
				return err
			}
			if format == "json" {
				type entry struct {
					image.Image
					UsedBy []string `json:"used_by"`
				}
				out := make([]entry, len(imgs))
				for i, img := range imgs {
					out[i] = entry{img, append([]string{}, users[img.ID]...)}
				}
				return writeJSON(cmd.OutOrStdout(), out)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tFEDORA\tOPENSHELL\tCREATED\tUSED BY")
			for _, img := range imgs {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%v\n", img.ID, img.FedoraRelease, img.OpenShellVersion,
					img.CreatedAt.Local().Format(time.DateTime), users[img.ID])
			}
			return tw.Flush()
		},
	}
	addOutputFlag(cmd, &format)
	return cmd
}

func newImageRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "rm ID...",
		Short:             "Remove base images that no VM uses",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeImages,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			users, err := a.imageUsers()
			if err != nil {
				return err
			}
			for _, id := range args {
				if u := users[id]; len(u) > 0 {
					return fmt.Errorf("image %s is used by %v", id, u)
				}
				if err := a.images.Remove(id); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed image %s.\n", id)
			}
			return nil
		},
	}
}

func newImagePruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove all base images except the newest and those in use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := newApp()
			if err != nil {
				return err
			}
			imgs, err := a.images.List()
			if err != nil {
				return err
			}
			users, err := a.imageUsers()
			if err != nil {
				return err
			}
			for i, img := range imgs {
				if i == 0 || len(users[img.ID]) > 0 {
					continue
				}
				if err := a.images.Remove(img.ID); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed image %s.\n", img.ID)
			}
			return nil
		},
	}
}

func newImagePushCmd() *cobra.Command {
	cmd := vmCommand("push NAME IMAGE", "Copy a container image from the host's Podman into a VM", nil)
	cmd.Args = cobra.ExactArgs(2)
	cmd.Long = `Copy a container image from the host's Podman storage into the VM's, where
OpenShell sandboxes can use it, e.g. with openshell sandbox create --from.
OpenShell cannot build images itself, so build them on the host.`
	cmd.Example = `  podman build -t localhost/my-agent:latest .
  brig image push dev localhost/my-agent:latest`
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		a, err := newApp()
		if err != nil {
			return err
		}
		v, err := a.vms.Load(args[0])
		if err != nil {
			return err
		}
		return a.pushImage(cmd.Context(), v, args[1], cmd)
	}
	cmd.ValidArgsFunction = func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 1 {
			return completePodmanImages(c.Context())
		}
		return completeVMs(c, args, toComplete)
	}
	return cmd
}

// completePodmanImages completes the names of the host's container images.
func completePodmanImages(ctx context.Context) ([]string, cobra.ShellCompDirective) {
	out, err := exec.CommandContext(ctx, "podman", "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var refs []string
	for ref := range strings.Lines(string(out)) {
		if ref = strings.TrimSpace(ref); ref != "" && !strings.Contains(ref, "<none>") {
			refs = append(refs, ref)
		}
	}
	return refs, cobra.ShellCompDirectiveNoFileComp
}

func (a *app) pushImage(ctx context.Context, v *vm.VM, ref string, cmd *cobra.Command) error {
	// brig must not keep an end of the pipe open: podman save would block
	// forever on a full pipe once ssh exits, e.g. because the VM is down.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	save := exec.CommandContext(ctx, "podman", "save", ref)
	save.Stdout, save.Stderr = pw, cmd.ErrOrStderr()
	load := a.sshTarget(v, false).Command(ctx, "podman", "load")
	load.Stdin = pr
	load.Stdout, load.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err = save.Start(); err != nil {
		err = fmt.Errorf("podman save %s: %w (is podman installed?)", ref, err)
	} else if err = load.Start(); err != nil {
		_ = save.Process.Kill()
		_ = save.Wait()
	}
	_ = pr.Close()
	_ = pw.Close()
	if err != nil {
		return err
	}
	loadErr := load.Wait()
	if loadErr != nil {
		_ = save.Process.Kill()
	}
	saveErr := save.Wait()
	switch {
	case loadErr != nil:
		return fmt.Errorf("loading %s into %s: %w", ref, v.Name, loadErr)
	case saveErr != nil:
		return fmt.Errorf("podman save %s failed: %w", ref, saveErr)
	}
	return nil
}
