// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

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
