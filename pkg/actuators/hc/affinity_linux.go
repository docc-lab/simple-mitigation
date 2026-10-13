//go:build linux

package hc

import (
	"errors"

	"golang.org/x/sys/unix"
)

// setAffinity pins thread tid to cpus. A thread that exited between reading
// cgroup.threads and this call (ESRCH) is not an error.
func setAffinity(tid int, cpus []int) error {
	var set unix.CPUSet
	set.Zero()
	for _, c := range cpus {
		set.Set(c)
	}
	err := unix.SchedSetaffinity(tid, &set)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
