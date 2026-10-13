// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Package osclient talks to an OpenShell gateway over gRPC, with the mTLS
// client bundle that brig start keeps in the openshell CLI's configuration
// directory. It reads, and makes the changes that need gRPC: AddRule, which
// carries brig's annotations, and what brig sandbox run needs, running a
// command in a sandbox and deleting one. Every other change goes through a
// command.
//
// It uses the stubs that OpenShell generates for Go, pinned in go.mod to the
// OpenShell release in StubsVersion. A gateway of another minor version may
// speak a changed protocol; protobuf ignores fields it does not know, so
// reads go on, but callers should say so.
package osclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/dennisklein/brig/internal/ui/store"
)

// StubsVersion is the OpenShell release whose generated stubs brig uses.
// Bump it with the SDK's pin in go.mod.
const StubsVersion = "0.1.3"

// callTimeout bounds each call to the gateway.
const callTimeout = 10 * time.Second

// pageSize is the page size of list calls, the most a gateway returns.
const pageSize = 1000

// bundleFiles are the client bundle's files in the gateway's mtls
// directory.
var bundleFiles = []string{"ca.crt", "tls.crt", "tls.key"}

// Client reads one gateway.
type Client struct {
	conn  *grpc.ClientConn
	api   pb.OpenShellClient
	dir   string
	stamp []fileStamp
}

// fileStamp is what tells a changed bundle file from the one read.
type fileStamp struct {
	mod  time.Time
	size int64
}

// Dial prepares a client of the gateway listening on 127.0.0.1:port, with
// the client bundle in mtlsDir (ca.crt, tls.crt and tls.key). It reads the
// bundle but does not connect: the first call does.
func Dial(port int, mtlsDir string) (*Client, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid gateway port %d", port)
	}
	stamp, err := stamps(mtlsDir)
	if err != nil {
		return nil, err
	}
	cfg, err := tlsConfig(mtlsDir)
	if err != nil {
		return nil, err
	}
	// passthrough: the address is an IP literal, nothing to resolve.
	conn, err := grpc.NewClient(fmt.Sprintf("passthrough:///127.0.0.1:%d", port),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, api: pb.NewOpenShellClient(conn), dir: mtlsDir, stamp: stamp}, nil
}

// tlsConfig trusts only the gateway's CA and presents the client
// certificate. The gateway's certificate names 127.0.0.1.
func tlsConfig(dir string) (*tls.Config, error) {
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return nil, bundleErr(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no certificate", filepath.Join(dir, "ca.crt"))
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		return nil, bundleErr(err)
	}
	return &tls.Config{
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func bundleErr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no mTLS client bundle yet; brig start copies it from the VM: %w", err)
	}
	return err
}

func stamps(dir string) ([]fileStamp, error) {
	out := make([]fileStamp, len(bundleFiles))
	for i, name := range bundleFiles {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			return nil, bundleErr(err)
		}
		out[i] = fileStamp{fi.ModTime(), fi.Size()}
	}
	return out, nil
}

// BundleChanged reports whether the client bundle changed on disk since
// Dial read it, as when brig start fetched a regenerated one. The client
// then needs dialing again.
func (c *Client) BundleChanged() bool {
	if c.dir == "" {
		return false
	}
	cur, err := stamps(c.dir)
	return err != nil || !slices.Equal(cur, c.stamp)
}

// Close closes the connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// scope selects the named workspace.
func scope(workspace string) *dm.WorkspaceSelector {
	return &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: workspace}}
}

// allScope selects every workspace, for the list calls that accept it.
func allScope() *dm.WorkspaceSelector {
	return &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}
}

// workspaceOf returns the workspace an object's metadata names; an empty
// one is the default workspace, as the gateway normalizes it.
func workspaceOf(m *dm.ObjectMeta) string {
	if w := m.GetWorkspace(); w != "" {
		return w
	}
	return store.DefaultWorkspace
}

// call runs f with the call timeout.
func call[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return f(ctx)
}

// Version asks the gateway for its OpenShell version. It is the first call
// on a new client, so its error says whether the gateway answers at all.
func (c *Client) Version(ctx context.Context) (string, error) {
	r, err := call(ctx, func(ctx context.Context) (*pb.HealthResponse, error) {
		return c.api.Health(ctx, &pb.HealthRequest{})
	})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(r.GetVersion(), "v"), nil
}

// maxPages bounds a list, against a gateway that keeps sending pages.
const maxPages = 100

// pages collects the items of a paged list call, each page with the call
// timeout.
func pages[T any](ctx context.Context, list func(ctx context.Context, token string) ([]T, string, error)) ([]T, error) {
	var all []T
	token := ""
	for range maxPages {
		cctx, cancel := context.WithTimeout(ctx, callTimeout)
		items, next, err := list(cctx, token)
		cancel()
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if next == "" {
			return all, nil
		}
		token = next
	}
	return nil, fmt.Errorf("the gateway sent more than %d pages", maxPages)
}
