// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package run

import (
	"context"
	"os"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress"
)

// openSpan records an openshell command in the activity log, as brig's own
// commands record themselves: the OpenShell CLI writes no events, so brig ui
// writes one span for the whole command under the task's trace, with the VM
// of its gateway as the span's node, as brig records the VM it works on. It
// returns the function that ends the span with the command's outcome. The
// log is a convenience, so a log that cannot be opened is no reason to fail
// the task.
func openSpan(path, trace, parent string, argv []string) (end func(error)) {
	tc, ok := progress.ParseTraceContext("00-"+trace+"-"+parent+"-01", "")
	if path == "" || !ok {
		return func(error) {}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func(error) {}
	}
	log := progress.NewLog(f, progress.LogOptions{Program: "openshell"})
	bus := progress.NewBus(progress.BusOptions{
		Sinks:   []progress.Sink{log},
		Trace:   tc,
		Program: "openshell",
	})
	var opts []progress.Option
	if vm := gatewayVM(argv); vm != "" {
		opts = append(opts, progress.Node(vm))
	}
	_, span := progress.Start(progress.WithBus(context.Background(), bus), progress.KindCommand, spanName(argv), opts...)
	return func(err error) {
		span.End(err)
		bus.Close()
		if log.Close() == nil {
			_ = f.Close()
		}
	}
}

// spanName names a command as the activity log lists it: the openshell
// command without its gateway option, such as "sandbox delete webapp".
func spanName(argv []string) string {
	words := make([]string, 0, len(argv))
	for i := 1; i < len(argv); i++ {
		if argv[i] == "-g" && i+1 < len(argv) {
			i++
			continue
		}
		words = append(words, argv[i])
	}
	return strings.Join(words, " ")
}

// gatewayVM returns the VM whose gateway an openshell command's -g names,
// brig-VM, or "".
func gatewayVM(argv []string) string {
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "-g" {
			vm, _ := strings.CutPrefix(argv[i+1], "brig-")
			if vm == argv[i+1] {
				return ""
			}
			return vm
		}
	}
	return ""
}
