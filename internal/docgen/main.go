// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Command docgen makes the parts of brig's documentation that its code
// knows: the reference pages in docs/reference and the man pages. It reads
// brig's command tree, help texts, config structs and exit codes, so that the
// documentation cannot drift from them.
//
// Usage:
//
//	go run ./internal/docgen [reference]    refresh docs/reference
//	go run ./internal/docgen man [-version V] DIR
//	                                         write the man pages into DIR
//
// The reference is made in brig's source tree, or where -C points. go generate
// ./... refreshes it. The man pages are not committed: the RPM build and
// GoReleaser make them.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dennisklein/brig/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("docgen", flag.ContinueOnError)
	src := fs.String("C", ".", "run in this directory of brig's source tree")
	ver := fs.String("version", version.Version(), "the brig version that the man pages name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Flags may come before and after the command.
	cmd, rest := "reference", fs.Args()
	if len(rest) > 0 {
		cmd = rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		rest = fs.Args()
	}
	switch cmd {
	case "reference":
		repo, err := findRepo(*src)
		if err != nil {
			return err
		}
		out, err := generate(repo)
		if err != nil {
			return err
		}
		return out.write(repo)
	case "man":
		if len(rest) != 1 {
			return fmt.Errorf("usage: docgen man [-version V] DIR")
		}
		return writeMan(rest[0], *ver)
	}
	return fmt.Errorf("unknown command %q: want reference or man", cmd)
}
