//go:build !linux

package cli

import "time"

// processCPUTime is only measured on Linux, where the Web UI runs.
func processCPUTime() (time.Duration, bool) {
	return 0, false
}
