// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package svcforward serves a service's host port for a service whose
// workload runs in an OpenShell sandbox of its VM, such as llm-proxy's
// LiteLLM. Nothing in the VM's network can connect into a sandbox, so brig
// itself listens on the host port, in a user unit, and forwards each
// connection over the VM's OpenShell gateway to the sandbox's loopback port.
package svcforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Dial opens a connection to the sandbox's port.
type Dial func(ctx context.Context) (io.ReadWriteCloser, error)

// Serve accepts connections on l until ctx ends, and bridges each one to a
// connection that dial opens. A connection that dial cannot open is closed,
// and logf says why; the caller then tries again, as an HTTP client does.
func Serve(ctx context.Context, l net.Listener, dial Dial, logf func(format string, args ...any)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			remote, err := dial(ctx)
			if err != nil {
				logf("forward from %s: %v", conn.RemoteAddr(), err)
				return
			}
			bridge(ctx, conn, remote)
		}()
	}
}

// bridge copies both ways until both directions end, or ctx does. When one
// side stops sending, the other learns it by a half-close, so that a client
// that closes its write side still gets the whole answer.
func bridge(ctx context.Context, local net.Conn, remote io.ReadWriteCloser) {
	defer func() { _ = remote.Close() }()
	stop := context.AfterFunc(ctx, func() {
		_ = local.Close()
		_ = remote.Close()
	})
	defer stop()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		closeWrite(remote)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		closeWrite(local)
		done <- struct{}{}
	}()
	<-done
	<-done
}

func closeWrite(c any) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else if cl, ok := c.(io.Closer); ok {
		_ = cl.Close()
	}
}

// UnitName is the systemd user unit that serves the service's host port.
func UnitName(service string) string { return "brig-svc-forward-" + service + ".service" }

// netUnit is the unit of the service VM's network, which the forward lives
// as long as; see package vmnet.
func netUnit(vm string) string { return "brig-net-" + vm + ".service" }

// unitEnv are the variables that the forward needs from brig's environment,
// which the user manager's may lack.
var unitEnv = []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"}

// UnitArgs are the arguments of systemd-run that start the forward of the
// service, whose VM has the same name, brig at path brig running brig
// service forward.
func UnitArgs(service, brig string) []string {
	args := []string{
		"--user", "--quiet", "--collect",
		"--unit=" + UnitName(service),
		"--description=brig: host port of service " + service,
		"--property=Type=exec",
		"--property=BindsTo=" + netUnit(service),
		"--property=After=" + netUnit(service),
		"--property=Restart=on-failure",
		"--property=RestartSec=2s",
	}
	for _, name := range unitEnv {
		if v, ok := os.LookupEnv(name); ok {
			args = append(args, "--setenv="+name+"="+v)
		}
	}
	return append(args, "--", brig, "service", "forward", service)
}

// StartUnit starts the service's forward unless it runs.
func StartUnit(ctx context.Context, service string) error {
	if exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", UnitName(service)).Run() == nil {
		return nil
	}
	brig, err := os.Executable()
	if err != nil {
		return err
	}
	systemctl(ctx, "reset-failed", UnitName(service))
	if out, err := exec.CommandContext(ctx, "systemd-run", UnitArgs(service, brig)...).CombinedOutput(); err != nil {
		return fmt.Errorf("starting %s: %w: %s", UnitName(service), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StopUnit stops the service's forward if it runs.
func StopUnit(ctx context.Context, service string) {
	systemctl(ctx, "stop", UnitName(service))
	systemctl(ctx, "reset-failed", UnitName(service))
}

func systemctl(ctx context.Context, verb, unit string) {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", verb, unit)
	// Kept from the terminal's Ctrl-C, like the network unit's stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	_ = cmd.Run()
}
