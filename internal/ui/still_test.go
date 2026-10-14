// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dennisklein/brig/internal/ui/demo"
)

// stillPages are the pages whose text the screenshots depend on.
var stillPages = []string{
	"/", "/vms", "/vms/dev", "/images", "/host", "/activity", "/sandboxes",
	"/sandboxes/dev/default/webapp", "/sandboxes/dev/default/webapp/history",
	"/gateways/dev", "/traffic", "/topology",
}

// token matches what differs from run to run in a page: the CSRF token and
// the port.
var token = regexp.MustCompile(`"X-Brig-CSRF": "[^"]*"|127\.0\.0\.1:\d+`)

// servedStill serves a still demo, fetches the pages and stops it.
func servedStill(t *testing.T) map[string]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Demo: true, Still: true, NoOpen: true, Stdout: out, Stderr: io.Discard})
	}()
	var url string
	for range 100 {
		if _, rest, ok := strings.Cut(out.String(), "brig ui: "); ok {
			url, _, _ = strings.Cut(rest, "\n")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("brig ui printed no URL: %q", out.String())
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	pages := map[string]string{}
	base, _, _ := strings.Cut(url, "/?")
	for _, p := range append([]string{""}, stillPages...) {
		target := base + p
		if p == "" {
			target = url
		}
		resp, err := c.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %s", p, resp.Status)
		}
		if p != "" {
			pages[p] = token.ReplaceAllString(string(body), "")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return pages
}

func TestStillDemoRepeats(t *testing.T) {
	was := clock
	t.Cleanup(func() { clock = was })
	first, second := servedStill(t), servedStill(t)
	for _, p := range stillPages {
		if first[p] != second[p] {
			t.Errorf("GET %s differs between two runs of the still demo:\n%s", p, firstDiff(first[p], second[p]))
		}
	}
	if !clock().Equal(demo.Epoch) {
		t.Errorf("the pages measure from %s, not from demo.Epoch", clock())
	}
}

// syncBuffer is a bytes.Buffer that the test reads while Run writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// firstDiff shows the lines around the first difference of a and b.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range min(len(al), len(bl)) {
		if al[i] != bl[i] {
			return "first run:  " + al[i] + "\nsecond run: " + bl[i]
		}
	}
	return "the lengths differ"
}
