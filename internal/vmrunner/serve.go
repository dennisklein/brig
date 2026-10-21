// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dennisklein/brig/internal/libvirt"
	"github.com/dennisklein/brig/internal/paths"
)

// ServeOptions configure Serve.
type ServeOptions struct {
	Options
	// Port is the port on the host's loopback interface that the MCP
	// server listens on.
	Port int
	// AdminSocket is the UNIX socket of the admin API, for brig's own
	// commands and brig ui.
	AdminSocket string
}

// AdminSocket returns the admin API's socket of the vm runner service
// called name, in the user's runtime directory.
func AdminSocket(runtimeDir, name string) string {
	return filepath.Join(runtimeDir, "brig", "vm-runner", name+".sock")
}

// reapInterval is how often the controller looks for VMs to delete.
const reapInterval = 30 * time.Second

// Serve runs the controller until ctx ends: the MCP server on the host's
// loopback interface, the admin API and the reaper. The VMs keep running
// when it returns.
func Serve(ctx context.Context, o ServeOptions) error {
	if o.Connect == nil {
		o.Connect = func(ctx context.Context) (Hypervisor, error) { return libvirt.Connect(ctx) }
	}
	c, err := New(o.Options)
	if err != nil {
		return err
	}
	mcpLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port)))
	if err != nil {
		return err
	}
	adminLn, err := listenUnix(o.AdminSocket)
	if err != nil {
		_ = mcpLn.Close()
		return err
	}
	mcpSrv := &http.Server{
		Handler:           c.Server(o.Log),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		// vm_exec waits up to maxExecTimeout for a command.
		WriteTimeout:   maxExecTimeout + time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 64 << 10,
	}
	adminSrv := &http.Server{Handler: c.Admin(), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- mcpSrv.Serve(mcpLn) }()
	go func() { errc <- adminSrv.Serve(adminLn) }()
	go c.RunReaper(ctx, reapInterval)
	c.log.Info("serving", "mcp", mcpLn.Addr().String(), "admin", o.AdminSocket, "vms", len(c.List()))
	select {
	case <-ctx.Done():
		err = nil
	case err = <-errc:
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer scancel()
	_ = mcpSrv.Shutdown(sctx)
	_ = adminSrv.Shutdown(sctx)
	_ = os.Remove(o.AdminSocket)
	return err
}

// listenUnix listens on a UNIX socket that only the user can reach.
func listenUnix(path string) (net.Listener, error) {
	if err := paths.EnsurePrivate(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

// AdminVM is a VM as the admin API lists it.
type AdminVM struct {
	Record
	State string `json:"state"`
}

// Admin returns the handler of the admin API, which lists every VM, takes
// screenshots of and destroys any. It is for the user's own commands only,
// so it serves on a UNIX socket of mode 0600.
func (c *Controller) Admin() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /vms", func(w http.ResponseWriter, r *http.Request) {
		recs := c.List()
		out := make([]AdminVM, len(recs))
		hv, err := c.connect(r.Context())
		for i, rec := range recs {
			out[i] = AdminVM{Record: rec, State: "unknown"}
			if err == nil {
				if st, serr := hv.State(rec.DomainName()); serr == nil {
					out[i].State = st.String()
				}
			}
		}
		if err == nil {
			_ = hv.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /vms/{id}/screenshot", func(w http.ResponseWriter, r *http.Request) {
		png, err := c.adminScreenshot(r.Context(), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(png) //nolint:gosec // G705: a PNG that toPNG checked, served as one
	})
	mux.HandleFunc("POST /vms/{id}/destroy", func(w http.ResponseWriter, r *http.Request) {
		if err := c.DestroyAny(r.Context(), r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func (c *Controller) adminScreenshot(ctx context.Context, id string) ([]byte, error) {
	c.mu.Lock()
	e, ok := c.vms[id]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no VM %s", id)
	}
	png, _, _, err := c.Screenshot(ctx, e.rec.Owner, id)
	return png, err
}

// AdminClient calls the admin API of a running controller.
type AdminClient struct {
	http *http.Client
}

// NewAdminClient returns a client of the admin API at socket.
func NewAdminClient(socket string) *AdminClient {
	return &AdminClient{http: &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}},
	}}
}

func (a *AdminClient) do(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://vm-runner"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the vm runner does not answer (is its service running?): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vm runner: %s", strings.TrimSpace(string(body)))
	}
	return body, nil
}

// List returns all VMs.
func (a *AdminClient) List(ctx context.Context) ([]AdminVM, error) {
	body, err := a.do(ctx, http.MethodGet, "/vms")
	if err != nil {
		return nil, err
	}
	var vms []AdminVM
	if err := json.Unmarshal(body, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

// Screenshot returns a VM's display as PNG.
func (a *AdminClient) Screenshot(ctx context.Context, id string) ([]byte, error) {
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("invalid VM id %q", id)
	}
	return a.do(ctx, http.MethodGet, "/vms/"+id+"/screenshot")
}

// Destroy deletes a VM.
func (a *AdminClient) Destroy(ctx context.Context, id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("invalid VM id %q", id)
	}
	_, err := a.do(ctx, http.MethodPost, "/vms/"+id+"/destroy")
	return err
}

// Logger returns the controller's logger, which writes to w for the
// journal of its unit.
func Logger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{} // the journal stamps each line
			}
			return a
		},
	}))
}
