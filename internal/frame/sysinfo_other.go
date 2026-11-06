//go:build !linux

package frame

func diskUsage(string) (free, total uint64) { return 0, 0 }
