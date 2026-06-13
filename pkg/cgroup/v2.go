// Package cgroup is a minimal cgroup v2 helper covering everything the
// isolate actuator needs: resolve the cgroup path for a (pod, container)
// pair from the kubelet's pods directory, then read/write the cpu.max file.
//
// It does NOT speak cgroup v1, which is end-of-life upstream and explicitly
// out of scope per plan-v2-centralized.md Section 1 ("cgroup write" is the
// 100ms isolate tier, cgroup v2 is the assumed substrate).
//
// All filesystem roots are configurable so unit tests can run against a
// fixture filesystem without root, and so the DaemonSet can mount the
// host's /sys/fs/cgroup at a non-standard path if needed.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default mount points inside the privileged DaemonSet container. These
// match the volumeMounts in deploy/controller/daemonset.yaml.
const (
	DefaultKubeletPodsRoot = "/var/lib/kubelet/pods"
	DefaultCgroupRoot      = "/sys/fs/cgroup"
)

// Resolver locates cgroupfs paths for kubelet-managed containers.
type Resolver struct {
	// KubeletPodsRoot is where the kubelet stores per-pod metadata; the
	// resolver reads container-id symlinks from this tree.
	KubeletPodsRoot string
	// CgroupRoot is the cgroup v2 unified hierarchy mount.
	CgroupRoot string
}

// NewDefaultResolver returns a Resolver using the standard mount points.
func NewDefaultResolver() *Resolver {
	return &Resolver{
		KubeletPodsRoot: DefaultKubeletPodsRoot,
		CgroupRoot:      DefaultCgroupRoot,
	}
}

// CPUMax is the parsed form of the cgroup v2 cpu.max file.
//
//	Quota  = -1 -> "max" (no quota)
//	Period > 0  always (default 100_000us)
type CPUMax struct {
	Quota  int64
	Period int64
}

// String formats CPUMax in the canonical cgroup v2 "quota period" shape;
// "max" is used for unbounded quota.
func (c CPUMax) String() string {
	if c.Quota < 0 {
		return fmt.Sprintf("max %d", c.Period)
	}
	return fmt.Sprintf("%d %d", c.Quota, c.Period)
}

// ParseCPUMax parses a "quota period" string. "max" is interpreted as -1.
// Whitespace around the tokens is tolerated. Returns an error for any
// other shape so callers don't silently consume malformed annotations.
func ParseCPUMax(s string) (CPUMax, error) {
	parts := strings.Fields(strings.TrimSpace(s))
	if len(parts) != 2 {
		return CPUMax{}, fmt.Errorf("cgroup: cpu.max %q: expected 2 fields, got %d", s, len(parts))
	}
	var quota int64
	if parts[0] == "max" {
		quota = -1
	} else {
		q, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return CPUMax{}, fmt.Errorf("cgroup: cpu.max quota %q: %w", parts[0], err)
		}
		quota = q
	}
	period, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return CPUMax{}, fmt.Errorf("cgroup: cpu.max period %q: %w", parts[1], err)
	}
	if period <= 0 {
		return CPUMax{}, fmt.Errorf("cgroup: cpu.max period must be > 0, got %d", period)
	}
	return CPUMax{Quota: quota, Period: period}, nil
}

// PathForPod returns the cgroup v2 directory for the named container of
// the pod with podUID. The walk is:
//
//  1. Read the container's runtime ID from
//     <kubeletPods>/<uid>/containers/<containerName>/<runtime-id-file>
//     (kubelet symlinks the file's name to the container id).
//  2. Pull the pod's cgroup path from
//     <kubeletPods>/<uid>/etc-hosts's grand-parent — too brittle.
//
// In practice the *reliable* way without parsing kubelet's QoS class out of
// the pod's Guaranteed/Burstable/BestEffort QoS path is to enumerate
// cgroupfs and pick the directory whose name contains podUID. We do that
// here; it's O(QoS-classes * pods-per-node) which is fine at typical scale.
func (r *Resolver) PathForPod(podUID, containerID string) (string, error) {
	if podUID == "" {
		return "", errors.New("cgroup: empty podUID")
	}
	uidVariants := uidVariants(podUID)
	root := r.CgroupRoot
	if root == "" {
		root = DefaultCgroupRoot
	}
	// Search up to two levels under root for a directory whose basename
	// contains pod<uid>. K8s nests cgroups as:
	//   /sys/fs/cgroup/kubepods.slice/kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice/...
	// or with cgroupfs driver:
	//   /sys/fs/cgroup/kubepods/<qos>/pod<uid>/...
	var podDir string
	walk := func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		for _, v := range uidVariants {
			if strings.Contains(base, v) {
				podDir = path
				return filepath.SkipAll
			}
		}
		return nil
	}
	if err := filepath.WalkDir(root, walk); err != nil {
		return "", fmt.Errorf("cgroup: walk %q: %w", root, err)
	}
	if podDir == "" {
		return "", fmt.Errorf("cgroup: no pod dir under %q for uid %q", root, podUID)
	}
	if containerID == "" {
		return podDir, nil
	}
	// Strip the common runtime prefix ("containerd://", "cri-o://", "docker://").
	id := containerID
	if idx := strings.Index(id, "://"); idx >= 0 {
		id = id[idx+3:]
	}
	// Container subdir contains the (possibly truncated) runtime ID.
	var containerDir string
	containerWalk := func(path string, d os.DirEntry, _ error) error {
		if !d.IsDir() || path == podDir {
			return nil
		}
		if strings.Contains(d.Name(), id) || strings.Contains(d.Name(), id[:min(12, len(id))]) {
			containerDir = path
			return filepath.SkipAll
		}
		return nil
	}
	if err := filepath.WalkDir(podDir, containerWalk); err != nil {
		return "", fmt.Errorf("cgroup: walk container %q: %w", podDir, err)
	}
	if containerDir == "" {
		// Some pod cgroup layouts don't have a per-container subdirectory; the
		// caller probably wants pod-level throttling and the pod dir is fine.
		return podDir, nil
	}
	return containerDir, nil
}

// uidVariants returns the underscore-substituted form used by systemd cgroup
// drivers ("pod<uid-with-underscores>") plus the original ("pod<uid>") used
// by cgroupfs.
func uidVariants(uid string) []string {
	dashed := uid
	underscored := strings.ReplaceAll(uid, "-", "_")
	return []string{
		"pod" + dashed,
		"pod" + underscored,
		dashed,
		underscored,
	}
}

// ReadCPUMax reads and parses cpu.max from the given cgroup directory.
func (r *Resolver) ReadCPUMax(cgroupDir string) (CPUMax, error) {
	raw, err := os.ReadFile(filepath.Join(cgroupDir, "cpu.max"))
	if err != nil {
		return CPUMax{}, fmt.Errorf("cgroup: read cpu.max: %w", err)
	}
	return ParseCPUMax(string(raw))
}

// WriteCPUMax overwrites cpu.max. The kernel ignores trailing whitespace;
// we include a newline to match how systemd-style tools write the file.
func (r *Resolver) WriteCPUMax(cgroupDir string, v CPUMax) error {
	body := v.String() + "\n"
	if err := os.WriteFile(filepath.Join(cgroupDir, "cpu.max"), []byte(body), 0644); err != nil {
		return fmt.Errorf("cgroup: write cpu.max: %w", err)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
