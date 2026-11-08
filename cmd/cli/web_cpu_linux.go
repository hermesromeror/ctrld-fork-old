//go:build linux

package cli

import (
	"syscall"
	"time"
)

// processCPUTime returns the user+system CPU time used by this process.
func processCPUTime() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
