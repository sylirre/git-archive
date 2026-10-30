//go:build unix

package gitx

import (
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// setProcAttr starts git in a new session. The session has no
// controlling terminal, so neither git nor anything it spawns (ssh,
// credential helpers, filters) can prompt on or write to the user's
// terminal; and the whole process tree shares one process group that is
// terminated together. The returned function must be called after Wait.
func setProcAttr(cmd *exec.Cmd, killWait time.Duration) (stop func()) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var (
		mu     sync.Mutex
		timer  *time.Timer
		exited bool
	)
	cmd.Cancel = func() error {
		pid := cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		mu.Lock()
		defer mu.Unlock()
		timer = time.AfterFunc(killWait, func() {
			mu.Lock()
			defer mu.Unlock()
			if !exited { // never signal a process group id that may have been reused
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		})
		return nil
	}
	cmd.WaitDelay = killWait + 5*time.Second
	return func() {
		mu.Lock()
		defer mu.Unlock()
		exited = true
		if timer != nil {
			timer.Stop()
		}
	}
}
