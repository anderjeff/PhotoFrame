//go:build linux

package frame

import "golang.org/x/sys/unix"

func diskUsage(path string) (free, total uint64) {
	var st unix.Statfs_t
	if unix.Statfs(path, &st) != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}
