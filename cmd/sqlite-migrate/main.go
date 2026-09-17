// Command sqlite-migrate is the CLI entrypoint. It is a thin binary that
// wires the internal/ generation tooling (schemadiff, rename, rebuild,
// sqldefwrap) together with the public sqlitemigrate runtime; it holds no
// generation or runtime logic of its own.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sqlite-migrate <command> [flags]")
		fmt.Fprintln(os.Stderr, "available commands: generate")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "generate":
		os.Exit(RunGenerate(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, time.Now))
	default:
		fmt.Fprintf(os.Stderr, "sqlite-migrate: unknown command %q\n", os.Args[1])
		fmt.Fprintln(os.Stderr, "available commands: generate")
		os.Exit(2)
	}
}
