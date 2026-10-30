// Command git-archive clones, updates and verifies archives of git
// repositories. See "git-archive help" or git-archive(1).
package main

import (
	"os"

	"github.com/sylirre/git-archive/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
