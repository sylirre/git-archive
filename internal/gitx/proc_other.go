// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package gitx

import (
	"os/exec"
	"time"
)

func setProcAttr(cmd *exec.Cmd, killWait time.Duration) (stop func()) {
	cmd.WaitDelay = killWait + 5*time.Second
	return func() {}
}
