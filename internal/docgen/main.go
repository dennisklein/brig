// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

// Command docgen makes the parts of brig's documentation that its code
// knows: the reference pages in docs/reference. It reads
// brig's command tree, help texts, config structs and exit codes, so that the
// documentation cannot drift from them.
//
// Usage:
//
//	go run ./internal/docgen [reference]    refresh docs/reference
//
// Run it in brig's source tree, or point it there with -C. go generate ./...
// refreshes the reference.
package main

import (
	"flag"
	"fmt"
	"os"
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmd := "reference"
	if fs.NArg() > 0 {
		cmd = fs.Arg(0)
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
	}
	return fmt.Errorf("unknown command %q: want reference", cmd)
}
