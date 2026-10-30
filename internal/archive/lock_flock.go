// SPDX-FileCopyrightText: 2026 Sylirre <107305601+sylirre@users.noreply.github.com>
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package archive

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// lockFile takes a non-blocking exclusive flock. The kernel releases it
// automatically when the process dies, so a crashed run never leaves a
// stale lock behind. The PID is recorded only to improve the message
// shown to a concurrent run.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			buf := make([]byte, 32)
			n, _ := f.ReadAt(buf, 0)
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(buf[:n]))); perr == nil {
				return nil, fmt.Errorf("the archive is in use by another git-archive process (pid %d)", pid)
			}
			return nil, errors.New("the archive is in use by another git-archive process")
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
