//go:build !linux

package hc

import "errors"

func setAffinity(int, []int) error {
	return errors.New("hc: sched_setaffinity is linux-only")
}
