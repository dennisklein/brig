// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/dennisklein/brig/internal/service"
)

// Service types that do more than brig does for every type register here,
// by the name of their manifest in internal/service/types.
var (
	// serviceDrivers holds the drivers of types whose service issues or
	// checks tokens in its own way; the others use service.HashDriver.
	serviceDrivers = map[string]service.Driver{}
	// appDrivers holds the drivers that need brig's records, such as the
	// service's VM; brig makes one for each use.
	appDrivers = map[string]func(a *app) service.Driver{}
	// hostRunners holds what runs the controllers of types of placement
	// host, and what runs next to the VM of a type of placement vm on the
	// host, such as the forwarder to a type's sandbox (see
	// service.Type.Sandbox).
	hostRunners = map[string]service.HostRunner{}
)

// serviceTypes returns the service types that brig knows; tests replace it.
var serviceTypes = service.Builtin

// serviceType returns the type called name.
func serviceType(name string) (service.Type, error) {
	return serviceTypes().Get(name)
}

// serviceDriver returns the driver of type t.
func (a *app) serviceDriver(t service.Type) service.Driver {
	if d, ok := serviceDrivers[t.Name]; ok {
		return d
	}
	if f, ok := appDrivers[t.Name]; ok {
		return f(a)
	}
	return service.HashDriver{}
}

// hostRunner returns what runs the controller of type t, of placement host.
func hostRunner(t service.Type) (service.HostRunner, error) {
	if r, ok := hostRunners[t.Name]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("service type %s runs on the host, but this brig has nothing to run it with", t.Name)
}
