package hc

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BalancerUnit is the transient systemd unit the actuator runs the Balancer
// as. One per node: HC's shared-memory segment is node-global. It is distinct
// from StaticBalancerUnit, the hand-run unit the node profile installs
// (reads /etc/default/hc-harvest), so the two never fight over a unit name.
const (
	BalancerUnit       = "hc-balancer-dyn"
	StaticBalancerUnit = "hc-balancer"
)

// CommandRunner runs a host command and returns its combined output. It is
// the seam tests use to observe systemd calls without a systemd.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

type balancerSpec struct {
	binary            string
	workDir           string
	bindCPU           int
	victimCPUs        []int
	secondaryCPUs     []int
	targetIdleCores   int
	minSecondaryCores int
	acbf              float64
	lowTIC            int
}

// balancerActive reports whether the unit is running (or starting).
// `systemctl is-active` exits non-zero for anything but "active", so the
// output, not the error, is what decides.
func balancerActive(ctx context.Context, r CommandRunner) bool {
	return unitActive(ctx, r, BalancerUnit)
}

func unitActive(ctx context.Context, r CommandRunner, unit string) bool {
	out, _ := r.Run(ctx, "systemctl", "is-active", unit)
	s := strings.TrimSpace(string(out))
	return s == "active" || s == "activating" || s == "reloading"
}

// startBalancer launches the Balancer as a transient unit pinned to its own
// core. Its argv mirrors TestFramework/bin/boilerplate.sh runBalancer; the
// first argument (secondary pid) is unused by the Balancer and passed as 0.
// It busy-waits until the Listener sets balancerControl=1, so starting it
// after registration is safe.
func startBalancer(ctx context.Context, r CommandRunner, s balancerSpec) ([]byte, error) {
	args := []string{
		"--unit=" + BalancerUnit, "--collect",
		"-p", "CPUAffinity=" + strconv.Itoa(s.bindCPU),
		"-p", "WorkingDirectory=" + s.workDir,
		"--setenv=LOWIDLEFREQ_THRESHOLD=" + strconv.FormatFloat(s.acbf, 'f', -1, 64),
		"--setenv=LOW_TIC=" + strconv.Itoa(s.lowTIC),
		s.binary, "0",
		strconv.Itoa(s.targetIdleCores),
		strconv.Itoa(s.minSecondaryCores),
		FormatCPUList(s.victimCPUs),
		FormatCPUList(s.secondaryCPUs),
	}
	return r.Run(ctx, "systemd-run", args...)
}

// stopBalancer sends SIGUSR1 (the Balancer's clean-exit signal: it writes
// its logs into its working directory and exits), waits up to grace for the
// unit to go inactive, then falls back to a hard stop.
func stopBalancer(ctx context.Context, r CommandRunner, grace, poll time.Duration) {
	if !balancerActive(ctx, r) {
		return
	}
	_, _ = r.Run(ctx, "systemctl", "kill", "-s", "SIGUSR1", BalancerUnit)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !balancerActive(ctx, r) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
	_, _ = r.Run(ctx, "systemctl", "stop", BalancerUnit)
}
