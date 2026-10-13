package hc

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseCPUList parses the kernel's cpulist format ("18-20,22", as found in
// cpuset.cpus and /proc/<pid>/status Cpus_allowed_list). Whitespace is
// tolerated and an empty string yields an empty list. The result is sorted
// and de-duplicated.
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi := part, part
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, hi = part[:i], part[i+1:]
		}
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("hc: cpulist %q: bad cpu %q", s, lo)
		}
		b, err := strconv.Atoi(strings.TrimSpace(hi))
		if err != nil {
			return nil, fmt.Errorf("hc: cpulist %q: bad cpu %q", s, hi)
		}
		if a < 0 || b < a {
			return nil, fmt.Errorf("hc: cpulist %q: bad range %q", s, part)
		}
		for c := a; c <= b; c++ {
			seen[c] = true
		}
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

// FormatCPUList renders cpus as a plain comma list ("18,19,20"). The HC
// Listener and Balancer parse with strsep(",") + sscanf("%d"), so ranges
// would silently truncate to their first cpu -- never emit them.
func FormatCPUList(cpus []int) string {
	parts := make([]string, len(cpus))
	for i, c := range cpus {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

// UnionCPUs returns the sorted, de-duplicated union of the given lists.
func UnionCPUs(lists ...[]int) []int {
	seen := map[int]bool{}
	for _, l := range lists {
		for _, c := range l {
			seen[c] = true
		}
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

func equalCPUs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
