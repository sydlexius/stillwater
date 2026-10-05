//go:build linux || darwin

package image

import "golang.org/x/sys/unix"

// openDirFd opens a directory and returns its fd. noFollow refuses a symlink
// as the final component (O_NOFOLLOW), so a swapped-in link cannot redirect it.
func openDirFd(path string, noFollow bool) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	if noFollow {
		flags |= unix.O_NOFOLLOW
	}
	return unix.Open(path, flags, 0)
}

func closeFd(fd int) { _ = unix.Close(fd) }

// isRegularAt reports whether name inside dirFd is a regular file, without
// following a final symlink.
func isRegularAt(dirFd int, name string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false, err
	}
	// Untyped constants, so this compiles for uint32 (linux) and uint16 (darwin) Mode.
	return st.Mode&unix.S_IFMT == unix.S_IFREG, nil
}

// removeDirOnly removes an EMPTY directory and nothing else. os.Remove tries
// unlink first, so a file swapped in at this name after the emptiness check
// would be deleted; rmdir(2) fails with ENOTDIR on a non-directory instead.
func removeDirOnly(path string) error { return unix.Rmdir(path) }
