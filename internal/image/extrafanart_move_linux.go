//go:build linux

package image

import "golang.org/x/sys/unix"

// renameNoReplace atomically renames srcDir/name to dstDir/dest and fails with
// EEXIST instead of replacing anything at dest.
func renameNoReplace(srcDirFd int, name string, dstDirFd int, dest string) error {
	return unix.Renameat2(srcDirFd, name, dstDirFd, dest, unix.RENAME_NOREPLACE)
}
