//go:build darwin

package image

import "golang.org/x/sys/unix"

// renameNoReplace atomically renames srcDir/name to dstDir/dest and fails with
// EEXIST instead of replacing anything at dest.
func renameNoReplace(srcDirFd int, name string, dstDirFd int, dest string) error {
	return unix.RenameatxNp(srcDirFd, name, dstDirFd, dest, unix.RENAME_EXCL)
}
