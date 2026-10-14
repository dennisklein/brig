// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// MaxStdin is the most standard input that Exec passes to a command: the
// gateway takes messages of up to 1 MiB, of which the command and its
// environment need some.
const MaxStdin = 1<<20 - 64<<10

// ExecRequest is a command for Exec to run in a sandbox.
type ExecRequest struct {
	Workspace, Sandbox string
	Command            []string
	// Workdir is where the command runs, the sandbox's working directory
	// if empty.
	Workdir string
	Env     map[string]string
	// Timeout stops the command after this long, unless zero; the gateway
	// then reports exit status 124.
	Timeout time.Duration
	Stdin   []byte
}

// Exec runs a command in a ready sandbox through the gateway, as openshell
// sandbox exec does, copying its output to stdout and stderr, and returns
// its exit status. An error means that the command's outcome is unknown.
func (c *Client) Exec(ctx context.Context, r ExecRequest, stdout, stderr io.Writer) (int, error) {
	if len(r.Stdin) > MaxStdin {
		return 0, fmt.Errorf("standard input is %d bytes, more than the %d that a command can get", len(r.Stdin), MaxStdin)
	}
	req := &pb.ExecSandboxRequest{
		WorkspaceScope: scope(r.Workspace),
		Sandbox:        r.Sandbox,
		Command:        r.Command,
		Workdir:        r.Workdir,
		Environment:    maps.Clone(r.Env),
		Stdin:          r.Stdin,
	}
	if r.Timeout > 0 {
		req.ExecutionTimeout = durationpb.New(r.Timeout)
	}
	stream, err := c.api.ExecSandbox(ctx, req)
	if err != nil {
		return 0, err
	}
	exit := -1
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		switch p := ev.GetPayload().(type) {
		case *pb.ExecSandboxEvent_Stdout:
			if _, err := stdout.Write(p.Stdout.GetData()); err != nil {
				return 0, err
			}
		case *pb.ExecSandboxEvent_Stderr:
			if _, err := stderr.Write(p.Stderr.GetData()); err != nil {
				return 0, err
			}
		case *pb.ExecSandboxEvent_Exit:
			exit = int(p.Exit.GetExitCode())
		}
	}
	if exit < 0 {
		return 0, errors.New("the command ended without an exit status")
	}
	return exit, nil
}

// deleteTimeout bounds a deletion, which waits for the sandbox's container
// to stop and often takes longer than callTimeout.
const deleteTimeout = 2 * time.Minute

// deleteRetry is the first pause before a call that the gateway refused as
// busy asks again.
var deleteRetry = 500 * time.Millisecond

// DeleteSandbox deletes a sandbox; one that is gone already is no error.
// While the gateway is still busy with the sandbox, as right after an
// interrupted command, it asks again.
func (c *Client) DeleteSandbox(ctx context.Context, workspace, name string) error {
	return retryBusy(ctx, deleteTimeout, func(ctx context.Context) error {
		_, err := c.api.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{WorkspaceScope: scope(workspace), Name: name, AllowMissing: true})
		return err
	})
}

// busy tells whether err refuses an operation because the gateway has
// another one pending on the sandbox.
func busy(err error) bool {
	s := status.Convert(err)
	return err != nil && s.Code() == codes.FailedPrecondition && strings.Contains(s.Message(), "still pending")
}
