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
)

// isInteractive reports whether brig may ask questions: whether in and out
// are terminals. Tests replace it.
var isInteractive = func(in io.Reader, out io.Writer) bool {
	i, ok := in.(*os.File)
	o, ok2 := out.(*os.File)
	return ok && ok2 && isTerminal(i) && isTerminal(o)
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
