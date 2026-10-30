//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package archive

// lockFile is a no-op where flock(2) is unavailable.
func lockFile(path string) (func(), error) { return func() {}, nil }
