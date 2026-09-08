//go:build windows

package main

// Windows is supported for development. Docker production images use the
// Unix implementation, which reports the underlying filesystem capacity.
func diskCapacity(_ string, used int64) (int64, int64, int64) {
	return 0, used, 0
}
