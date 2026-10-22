// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package svcforward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// TestServeHTTP forwards HTTP requests, streamed ones too, to a server that
// stands for the sandbox's port.
func TestServeHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s %s %s", r.Method, r.URL.Path, r.Header.Get("Authorization"), body) //nolint:gosec // G705: a test echo
	}))
	defer backend.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var logs []string
	served := make(chan error, 1)
	fail := true
	go func() {
		served <- Serve(ctx, l, func(ctx context.Context) (io.ReadWriteCloser, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				fail = false
				return nil, errors.New("gateway not ready")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
		}, func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		})
	}()
	url := "http://" + l.Addr().String() + "/v1/messages"
	// The first connection fails, as while the gateway starts.
	if resp, err := http.Post(url, "text/plain", nil); err == nil { //nolint:gosec // G107: the test's own listener
		_ = resp.Body.Close()
		t.Fatal("request through a failed dial worked")
	}
	for i := range 20 {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(fmt.Sprint(i))))
		req.Header.Set("Authorization", "Bearer sk-x")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if want := fmt.Sprintf("POST /v1/messages Bearer sk-x %d", i); string(body) != want {
			t.Fatalf("got %q, want %q", body, want)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) != 1 || !strings.Contains(logs[0], "gateway not ready") {
		t.Fatalf("logs = %q", logs)
	}
}

// TestHalfClose keeps the answer coming after the client stops sending.
func TestHalfClose(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		data, _ := io.ReadAll(c)
		_, _ = c.Write(bytes.ToUpper(data))
		_ = c.Close()
	}()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = Serve(ctx, l, func(ctx context.Context) (io.ReadWriteCloser, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", backend.Addr().String())
		}, t.Logf)
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("ping"))
	_ = c.(*net.TCPConn).CloseWrite()
	got, _ := io.ReadAll(c)
	if string(got) != "PING" {
		t.Fatalf("got %q", got)
	}
}

func TestUnitArgs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/d")
	args := UnitArgs("llm-proxy", "/usr/bin/brig")
	for _, want := range []string{"--unit=brig-svc-forward-llm-proxy.service", "--property=BindsTo=brig-net-llm-proxy.service", "--setenv=XDG_DATA_HOME=/d"} {
		if !slices.Contains(args, want) {
			t.Errorf("args lack %s: %q", want, args)
		}
	}
	if tail := args[len(args)-5:]; !slices.Equal(tail, []string{"--", "/usr/bin/brig", "service", "forward", "llm-proxy"}) {
		t.Errorf("args end in %q", tail)
	}
}
