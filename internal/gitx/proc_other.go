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
