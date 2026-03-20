package api

import "os"

// FileRemover abstracts file removal for testability. Production code uses
// osRemover (the default); tests can inject a stub that returns errors on demand
// to exercise error paths that are otherwise unreachable as root or on Windows.
// Implementations must return an error wrapping os.ErrNotExist when the target
// file does not exist, since callers use errors.Is to distinguish missing files
// from genuine failures.
type FileRemover interface {
	Remove(name string) error
}

// osRemover delegates to os.Remove.
type osRemover struct{}

func (osRemover) Remove(name string) error { return os.Remove(name) }
