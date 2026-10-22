// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package osclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// maxForwardFrame is the most data that one frame of a TCP forward
// carries, well below the gateway's message limit.
const maxForwardFrame = 32 << 10

// ForwardTCP opens a TCP connection to port on the loopback interface of a
// sandbox, through the gateway, as openshell forward does. Reads return
// what the sandbox's service sends, and io.EOF once it closes; Close ends
// the connection.
func (c *Client) ForwardTCP(ctx context.Context, workspace, sandbox string, port uint32) (io.ReadWriteCloser, error) {
	if sandbox == "" || port == 0 || port > 65535 {
		return nil, fmt.Errorf("forward to sandbox %q port %d: invalid target", sandbox, port)
	}
	ctx, cancel := context.WithCancel(ctx)
	stream, err := c.api.ForwardTcp(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	init := &pb.TcpForwardFrame{Payload: &pb.TcpForwardFrame_Init{Init: &pb.TcpForwardInit{
		Sandbox: sandbox, Workspace: workspace,
		Target: &pb.TcpForwardInit_Tcp{Tcp: &pb.TcpRelayTarget{Host: "127.0.0.1", Port: port}},
	}}}
	if err := stream.Send(init); err != nil {
		cancel()
		return nil, err
	}
	pr, pw := io.Pipe()
	f := &forward{stream: stream, cancel: cancel, r: pr}
	go func() {
		for {
			frame, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				_ = pw.Close()
				return
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if data := frame.GetData(); len(data) > 0 {
				if _, err := pw.Write(data); err != nil {
					return
				}
			}
		}
	}()
	return f, nil
}

// forward is one connection of ForwardTCP.
type forward struct {
	stream pb.OpenShell_ForwardTcpClient
	cancel context.CancelFunc
	r      *io.PipeReader
	// mu orders the writes on the stream, which allows one sender.
	mu     sync.Mutex
	closed bool
	once   sync.Once
}

func (f *forward) Read(p []byte) (int, error) { return f.r.Read(p) }

func (f *forward) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) > 0 {
		chunk := p[:min(len(p), maxForwardFrame)]
		if err := f.stream.Send(&pb.TcpForwardFrame{Payload: &pb.TcpForwardFrame_Data{Data: chunk}}); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// CloseWrite tells the sandbox's service that no more data comes, and
// leaves the other direction open.
func (f *forward) CloseWrite() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return f.stream.CloseSend()
}

func (f *forward) Close() error {
	f.once.Do(func() {
		_ = f.CloseWrite()
		f.cancel()
		_ = f.r.Close()
	})
	return nil
}
