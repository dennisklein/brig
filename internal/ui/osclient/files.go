// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/GSI-HPC/go-clikit/termtext"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/dennisklein/brig/internal/ui/store"
)

const (
	// listTimeout bounds a directory listing in a sandbox.
	listTimeout = 20 * time.Second
	// maxListOutput is the most output of a listing that is read, in bytes.
	maxListOutput = 4 << 20
	// maxFiles is the most entries a listing returns.
	maxFiles = 1000
	// maxStderr is how much of a failed listing's error output is kept.
	maxStderr = 2 << 10
)

// ListCommand returns the command that lists the directory dir in a sandbox:
// find prints the entries NUL-separated, each as its type, size, modification
// time and name, so that no name can break the listing.
func ListCommand(dir string) []string {
	return []string{"find", dir, "-mindepth", "1", "-maxdepth", "1", "-printf", `%y\t%s\t%T@\t%f\0`}
}

// ListDir lists the directory dir in the sandbox, which must run. It runs
// ListCommand in the sandbox through the gateway and changes nothing.
func (c *Client) ListDir(ctx context.Context, sandbox, dir string) (store.Dir, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	stream, err := c.api.ExecSandbox(ctx, &pb.ExecSandboxRequest{
		WorkspaceScope:   scope(),
		Sandbox:          sandbox,
		Command:          ListCommand(dir),
		NoLoginShell:     true,
		ExecutionTimeout: durationpb.New(listTimeout),
	})
	if err != nil {
		return store.Dir{}, err
	}
	var out, errOut bytes.Buffer
	exit := -1
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return store.Dir{}, err
		}
		switch p := ev.GetPayload().(type) {
		case *pb.ExecSandboxEvent_Stdout:
			if out.Len() < maxListOutput {
				out.Write(p.Stdout.GetData())
			}
		case *pb.ExecSandboxEvent_Stderr:
			if errOut.Len() < maxStderr {
				errOut.Write(p.Stderr.GetData())
			}
		case *pb.ExecSandboxEvent_Exit:
			exit = int(p.Exit.GetExitCode())
		}
	}
	switch {
	case exit < 0:
		return store.Dir{}, errors.New("the listing ended without an exit status")
	case exit != 0:
		msg := strings.TrimSpace(termtext.EscapeLines(errOut.String()))
		if msg == "" {
			msg = "find exited with status " + strconv.Itoa(exit)
		}
		return store.Dir{}, errors.New(msg)
	}
	return parseDir(out.Bytes())
}

// kinds names the types that find's %y prints.
var kinds = map[string]string{"d": "dir", "f": "file", "l": "link"}

// parseDir reads the output of ListCommand.
func parseDir(out []byte) (store.Dir, error) {
	var d store.Dir
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		if len(d.Files) == maxFiles {
			d.Truncated = true
			break
		}
		fields := strings.SplitN(string(rec), "\t", 4)
		if len(fields) != 4 {
			return store.Dir{}, fmt.Errorf("unexpected listing line %q", termtext.EscapeLines(string(rec)))
		}
		kind, ok := kinds[fields[0]]
		if !ok {
			kind = "other"
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return store.Dir{}, fmt.Errorf("unexpected size %q in the listing", termtext.EscapeLines(fields[1]))
		}
		f := store.File{Name: fields[3], Kind: kind, Size: size}
		if secs, err := strconv.ParseFloat(fields[2], 64); err == nil {
			f.Mod = time.Unix(int64(secs), 0)
		}
		d.Files = append(d.Files, f)
	}
	return d, nil
}
