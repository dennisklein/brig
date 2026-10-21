// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/GSI-HPC/go-clikit/termtext"
	"github.com/spf13/cobra"

	"github.com/dennisklein/brig/internal/vm"
)

// gatewayEnv names the variable through which brig use selects a VM: it holds
// the VM's gateway name, brig-<VM>.
const gatewayEnv = "OPENSHELL_GATEWAY"

// errNoVMSelected is why selectedVM finds no VM when brig use selected none.
var errNoVMSelected = errors.New("name the VM, or point this shell at one with brig use NAME")

// selectedVM returns the name of the VM that brig use selected in this shell.
func selectedVM() (string, error) {
	gw := os.Getenv(gatewayEnv)
	if name, ok := strings.CutPrefix(gw, "brig-"); ok && name != "" {
		return name, nil
	}
	if gw != "" {
		return "", fmt.Errorf("%s names gateway %q, which is no brig VM's; name the VM", gatewayEnv, gw)
	}
	return "", errNoVMSelected
}

// resolveVM loads the VM that a command works on: the one named by given, or,
// if given is empty, the one that brig use selected, which it then says on
// standard error so that a stray brig use cannot retarget a command unseen.
//
// Every command that works on one VM resolves it here.
func (a *app) resolveVM(cmd *cobra.Command, given string) (*vm.VM, error) {
	name := given
	if name == "" {
		var err error
		if name, err = selectedVM(); err != nil {
			return nil, err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Using VM %s (selected by brig use).\n", termtext.Escape(name))
	}
	v, err := a.vms.Load(name)
	if err != nil {
		return nil, err
	}
	recordVM(cmd.Context(), v.Name)
	if !anyVMCommands[topCommand(cmd).Name()] {
		if err := requireOpenShell(v); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// anyVMCommands work on every VM; the other commands that work on one VM
// need its OpenShell gateway, which a service VM may not have.
var anyVMCommands = map[string]bool{
	"start": true, "stop": true, "show": true, "ssh": true, "console": true,
	"update": true, "upgrade": true, "sync": true, "delete": true,
}

// topCommand returns the command below brig that cmd is or belongs to.
func topCommand(cmd *cobra.Command) *cobra.Command {
	for cmd.HasParent() && cmd.Parent().HasParent() {
		cmd = cmd.Parent()
	}
	return cmd
}

// requireOpenShell fails for a service VM whose type runs no OpenShell
// gateway.
func requireOpenShell(v *vm.VM) error {
	ok, err := runsOpenShell(v)
	if err == nil && !ok {
		err = fmt.Errorf("VM %s is service %[1]s, which runs no OpenShell gateway", v.Name)
	}
	return err
}

// resolveVMArg is resolveVM for a command whose arguments are args, of which
// the first names the VM unless there are none.
func (a *app) resolveVMArg(cmd *cobra.Command, args []string) (*vm.VM, error) {
	given := ""
	if len(args) > 0 {
		given = args[0]
	}
	return a.resolveVM(cmd, given)
}
