// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"

	"github.com/GSI-HPC/go-clikit/termtext"
)

// escWriter writes the lines of untrusted output to w with their control
// characters escaped. It holds back a line until it ends, so that no
// escape sequence or rune is cut in two.
type escWriter struct {
	mu   sync.Mutex
	w    io.Writer
	part []byte
}

func newEscWriter(w io.Writer) *escWriter { return &escWriter{w: w} }

func (e *escWriter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.part = append(e.part, p...)
	for {
		i := bytes.IndexByte(e.part, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := termtext.EscapeLines(string(e.part[:i]))
		e.part = e.part[i+1:]
		if _, err := io.WriteString(e.w, line+"\n"); err != nil {
			return len(p), err
		}
	}
}

// Close writes the line that did not end.
func (e *escWriter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.part) == 0 {
		return nil
	}
	_, err := io.WriteString(e.w, termtext.EscapeLines(string(e.part))+"\n")
	e.part = nil
	return err
}

// tailWriter keeps the last lines written to it.
type tailWriter struct {
	mu    sync.Mutex
	lines []string
	part  string
	max   int
}

func newTailWriter(max int) *tailWriter { return &tailWriter{max: max} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.part += string(p)
	for {
		line, rest, ok := strings.Cut(t.part, "\n")
		if !ok {
			return len(p), nil
		}
		t.part = rest
		t.lines = append(t.lines, line)
		if len(t.lines) > t.max {
			t.lines = t.lines[1:]
		}
	}
}

// String returns the kept lines, the last one included if it did not end.
func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := t.lines
	if t.part != "" {
		lines = append(lines[:len(lines):len(lines)], t.part)
	}
	return strings.Join(lines, "\n")
}
