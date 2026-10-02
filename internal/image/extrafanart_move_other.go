//go:build !linux && !darwin

package image

import "errors"

var errNoAtomicMove = errors.New("no atomic no-replace move on this platform")

func openDirFd(string, bool) (int, error)            { return -1, errNoAtomicMove }
func closeFd(int)                                    {}
func isRegularAt(int, string) (bool, error)          { return false, errNoAtomicMove }
func renameNoReplace(int, string, int, string) error { return errNoAtomicMove }
