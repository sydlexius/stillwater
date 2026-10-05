//go:build !linux && !darwin

package image

import (
	"errors"
	"syscall"
)

var errNoAtomicMove = errors.New("no atomic no-replace move on this platform")

func openDirFd(string, bool) (int, error)            { return -1, errNoAtomicMove }
func closeFd(int)                                    {}
func isRegularAt(int, string) (bool, error)          { return false, errNoAtomicMove }
func renameNoReplace(int, string, int, string) error { return errNoAtomicMove }

// removeDirOnly removes an EMPTY directory and nothing else; unlike os.Remove
// it never falls back to unlink, so a file swapped in after the emptiness
// check survives (see the unix variant).
func removeDirOnly(path string) error { return syscall.Rmdir(path) }
