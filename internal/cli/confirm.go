// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GSI-HPC/go-clikit/progress"
	"github.com/GSI-HPC/go-clikit/progress/cliprogress"
)

// isInteractive reports whether brig may ask questions: whether in and out
// are terminals. It looks through the progress display's writers. Tests
// replace it.
var isInteractive = func(in io.Reader, out io.Writer) bool {
	i, ok := in.(*os.File)
	o, ok2 := fileOf(out)
	return ok && ok2 && cliprogress.IsTerminal(i) && cliprogress.IsTerminal(o)
}

// fileWriter is a writer, such as the progress display's, that writes to
// the file f in the end.
type fileWriter struct {
	io.Writer
	f *os.File
}

// overFile returns shown, a writer that writes to w, such that fileOf finds
// the file w is. It is shown itself where w is no file or shown is w.
//
// The display does not see through a fileWriter: what is passed to its
// Writer must not be one, or a write would lock the display twice.
func overFile(shown, w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok || shown == w {
		return shown
	}
	return &fileWriter{Writer: shown, f: f}
}

// fileOf returns the file that w writes to, if it is one or a fileWriter.
func fileOf(w io.Writer) (*os.File, bool) {
	switch v := w.(type) {
	case *os.File:
		return v, true
	case *fileWriter:
		return v.f, true
	}
	return nil, false
}

// confirmer returns the function that asks a yes or no question on w, with
// the progress display taken off the terminal, and reads the answer from in,
// or nil where brig does not ask: with yes, and where in or w is no
// terminal. The function fails when ctx ends before an answer comes, as it
// does for Ctrl-C.
func confirmer(ctx context.Context, in io.Reader, w io.Writer, yes bool) func(prompt string) (bool, error) {
	if yes || !isInteractive(in, w) {
		return nil
	}
	r := bufio.NewReader(in)
	return func(prompt string) (bool, error) {
		defer progress.Suspend(ctx)()
		fmt.Fprint(w, prompt)
		type answer struct {
			line string
			err  error
		}
		got := make(chan answer, 1)
		go func() {
			line, err := r.ReadString('\n')
			got <- answer{line, err}
		}()
		select {
		case <-ctx.Done():
			fmt.Fprintln(w)
			return false, ctx.Err()
		case a := <-got:
			if a.err != nil && !errors.Is(a.err, io.EOF) {
				return false, a.err
			}
			if a.line == "" {
				fmt.Fprintln(w)
			}
			switch strings.ToLower(strings.TrimSpace(a.line)) {
			case "y", "yes":
				return true, nil
			}
			return false, nil
		}
	}
}
