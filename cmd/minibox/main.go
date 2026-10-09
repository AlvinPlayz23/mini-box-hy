// Command minibox is a fast, daemonless, Docker-like container runtime.
package main

import (
	"os"

	"github.com/AlvinPlayz23/minibox/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
