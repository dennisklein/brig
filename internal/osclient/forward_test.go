// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// echoGateway answers TCP forwards to 127.0.0.1:4000 of sandbox litellm
// with what it gets, upper case, and refuses others.
type echoGateway struct {
	pb.UnimplementedOpenShellServer
}

// ForwardTcp is named by the generated server interface.
func (echoGateway) ForwardTcp(s grpc.BidiStreamingServer[pb.TcpForwardFrame, pb.TcpForwardFrame]) error { //nolint:revive // var-naming
	first, err := s.Recv()
	if err != nil {
		return err
	}
	init := first.GetInit()
	tcp := init.GetTcp()
	if init.GetSandbox() != "litellm" || init.GetWorkspace() != "default" || tcp.GetHost() != "127.0.0.1" || tcp.GetPort() != 4000 {
		return status.Error(codes.NotFound, "no such target")
	}
	for {
		f, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return s.Send(&pb.TcpForwardFrame{Payload: &pb.TcpForwardFrame_Data{Data: []byte("BYE")}})
		}
		if err != nil {
			return err
		}
		if err := s.Send(&pb.TcpForwardFrame{Payload: &pb.TcpForwardFrame_Data{Data: bytes.ToUpper(f.GetData())}}); err != nil {
			return err
		}
	}
}

func TestForwardTCP(t *testing.T) {
	c := serve(t, echoGateway{})
	ctx := context.Background()
	conn, err := c.ForwardTCP(ctx, "default", "litellm", 4000)
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 3*maxForwardFrame+5)
	if _, err := conn.Write([]byte("hello " + big)); err != nil {
		t.Fatal(err)
	}
	if err := conn.(interface{ CloseWrite() error }).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if want := "HELLO " + strings.ToUpper(big) + "BYE"; string(got) != want {
		t.Fatalf("read %d bytes, want %d", len(got), len(want))
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("x")); err == nil {
		t.Fatal("write after close worked")
	}

	bad, err := c.ForwardTCP(ctx, "default", "other", 4000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(bad); status.Code(err) != codes.NotFound {
		t.Fatalf("read from a refused forward: %v", err)
	}
	if _, err := c.ForwardTCP(ctx, "default", "litellm", 0); err == nil {
		t.Fatal("port 0 accepted")
	}
}
