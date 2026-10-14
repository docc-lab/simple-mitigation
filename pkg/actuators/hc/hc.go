// Package hc drives HarvestContainers (HC) as a mitigation actuator.
//
// HC is three host components on the victim's node (see the
// HarvestContainers-docclab-mod repo): a kernel module that busy-polls
// idle_cpu() over the Primary's cores, a Listener (TCP :10101) that
// registers Primaries and Secondaries into a shared-memory segment, and a
// Balancer that rewrites the Secondaries' CPU affinity every time the idle
// mask changes -- lending idle Primary cores to the Secondary while keeping
// targetIdleCores of them free as a wake-up buffer.
//
// Mapped onto this repo: the victim is HC's Primary and the co-located
// aggressors are its Secondaries. Apply confines the aggressors to
// secondary_cpulist plus whatever victim cores are idle; Restore releases
// them back to the whole machine. That is a different mechanism from
// pkg/actuators/harvest (which raises a best-effort pod's cpu.max) -- hence
// the separate kind name.
//
// The victim's cores are NOT read from its cgroup. The DSB victim pins itself
// with `taskset -c $CPU_SET` inside the container, so its cpuset is the whole
// machine; the union of its tasks' Cpus_allowed_list is the real answer.
//
// Crash-safety follows the other actuators: victim pods carry
// mitigation/hc-* annotations (written before the Listener is told
// anything), aggressor pods carry their original cpuset.cpus (written before
// it is narrowed), and Reconcile restores everything it finds annotated.
package hc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/cgroup"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Defaults for Apply params. They match the Ice Lake CPU map in
// HarvestContainers-docclab-mod/Docs/07_icelake_hc_actuator_plan.md.
const (
	DefaultBinDir      = "/local/HarvestContainers/TestFramework/bin"
	DefaultLogDir      = "/mnt/extra/logs/hc"
	DefaultBalancerCPU = 14
	DefaultACBF        = 0.33 // HC's xapian value; tune per victim
	DefaultLowTIC      = 1
	DefaultTargetIdle  = 1
)

// Actuator implements actuators.Actuator for HarvestContainers.
type Actuator struct {
	client   kubernetes.Interface
	resolver *cgroup.Resolver
	logger   *slog.Logger
	nodeName string

	// Seams for tests; production values are set by New.
	procRoot     string
	runner       CommandRunner
	setAffinity  func(tid int, cpus []int) error
	onlineCPUs   func() ([]int, error)
	listenerAddr string // last Apply's listener_addr; Restore reuses it
	timeout      time.Duration
	stopGrace    time.Duration
	stopPoll     time.Duration

	// mu serialises Apply/Restore: HC's Listener state is node-global, so two
	// targets interleaving Primary/Reset messages would corrupt it.
	mu sync.Mutex
}

// Option customises an Actuator; used by tests.
type Option func(*Actuator)

// WithProcRoot overrides /proc (Cpus_allowed_list lookups and the HC
// module's /proc/idlecpu entries).
func WithProcRoot(p string) Option { return func(a *Actuator) { a.procRoot = p } }

// WithListenerAddr sets the Listener address Restore/Reconcile use before
// any Apply has supplied one.
func WithListenerAddr(addr string) Option { return func(a *Actuator) { a.listenerAddr = addr } }

// WithRunner overrides the host command runner (systemd calls).
func WithRunner(r CommandRunner) Option { return func(a *Actuator) { a.runner = r } }

// WithAffinityFunc overrides sched_setaffinity.
func WithAffinityFunc(f func(int, []int) error) Option {
	return func(a *Actuator) { a.setAffinity = f }
}

// WithOnlineCPUs overrides the online-CPU source.
func WithOnlineCPUs(f func() ([]int, error)) Option { return func(a *Actuator) { a.onlineCPUs = f } }

// WithStopTiming overrides how long Restore waits for the Balancer to exit.
func WithStopTiming(grace, poll time.Duration) Option {
	return func(a *Actuator) { a.stopGrace, a.stopPoll = grace, poll }
}

// New constructs an hc Actuator. resolver may be nil (host defaults).
func New(client kubernetes.Interface, nodeName string, resolver *cgroup.Resolver, logger *slog.Logger, opts ...Option) (*Actuator, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("hc: nodeName required")
	}
	if resolver == nil {
		resolver = cgroup.NewDefaultResolver()
	}
	if logger == nil {
		logger = slog.Default()
	}
	a := &Actuator{
		client:       client,
		resolver:     resolver,
		logger:       logger.With("actuator", "hc"),
		nodeName:     nodeName,
		procRoot:     "/proc",
		listenerAddr: DefaultListenerAddr,
		runner:       execRunner{},
		setAffinity:  setAffinity,
		onlineCPUs:   hostOnlineCPUs,
		timeout:      5 * time.Second,
		stopGrace:    15 * time.Second,
		stopPoll:     200 * time.Millisecond,
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Name returns "hc".
func (a *Actuator) Name() string { return "hc" }

type applyParams struct {
	listenerAddr      string
	binDir            string
	logDir            string
	balancerCPU       int
	secondaryCPUs     []int
	victimOverride    []int
	targetIdleCores   int
	lowTIC            int
	acbf              float64
	minSecondaryCores int
	aggSelector       string
	aggNamespace      string
}

func parseParams(target actuators.Target, params map[string]any) (applyParams, error) {
	p := applyParams{
		listenerAddr:      paramStr(params, "listener_addr", DefaultListenerAddr),
		binDir:            paramStr(params, "hc_bin_dir", DefaultBinDir),
		logDir:            paramStr(params, "log_dir", DefaultLogDir),
		balancerCPU:       paramInt(params, "balancer_cpu", DefaultBalancerCPU),
		targetIdleCores:   paramInt(params, "target_idle_cores", DefaultTargetIdle),
		lowTIC:            paramInt(params, "low_tic", DefaultLowTIC),
		acbf:              paramFloat(params, "acbf", DefaultACBF),
		minSecondaryCores: paramInt(params, "min_secondary_cores", 0),
		aggSelector:       paramStr(params, "aggressor_selector", ""),
		aggNamespace:      target.Spec.Namespace,
	}
	if p.aggSelector == "" {
		return p, fmt.Errorf("hc: aggressor_selector param required")
	}
	if v := paramStr(params, "aggressor_namespace", ""); v != "" {
		p.aggNamespace = v
		if v == "*" {
			p.aggNamespace = metav1.NamespaceAll
		}
	}
	sec, err := ParseCPUList(paramStr(params, "secondary_cpulist", ""))
	if err != nil {
		return p, err
	}
	if len(sec) == 0 {
		return p, fmt.Errorf("hc: secondary_cpulist param required")
	}
	p.secondaryCPUs = sec
	if p.victimOverride, err = ParseCPUList(paramStr(params, "victim_cpulist", "")); err != nil {
		return p, err
	}
	if p.targetIdleCores < 0 || p.lowTIC < 0 || p.minSecondaryCores < 0 {
		return p, fmt.Errorf("hc: target_idle_cores, low_tic, min_secondary_cores must be >= 0")
	}
	return p, nil
}

// Apply registers the victim as HC's Primary and every matching aggressor on
// this node as a Secondary, then starts the Balancer. Recognised params:
//
//	aggressor_selector: string   label selector (REQUIRED)
//	aggressor_namespace: string  defaults to the target's namespace; "*" = all
//	secondary_cpulist: string    cores the aggressors always keep (REQUIRED)
//	victim_cpulist: string       override the derived victim cores
//	target_idle_cores: int       idle victim cores HC keeps free (default 1;
//	                             must be < number of victim cores)
//	low_tic: int                 floor for the dynamic target (default 1)
//	acbf: float                  LOWIDLEFREQ_THRESHOLD (default 0.33)
//	min_secondary_cores: int     default 0
//	balancer_cpu: int            core the Balancer is pinned to (default 14)
//	listener_addr, hc_bin_dir, log_dir: host plumbing, see defaults above
//
// Re-applying while held is cheap and idempotent: only aggressor pods that
// appeared since the last call are registered.
func (a *Actuator) Apply(ctx context.Context, target actuators.Target, params map[string]any) (actuators.ActionResult, error) {
	if target.Spec == nil {
		return actuators.ActionResult{}, fmt.Errorf("hc: nil target spec")
	}
	p, err := parseParams(target, params)
	if err != nil {
		return actuators.ActionResult{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	victims, err := a.listVictims(ctx, target)
	if err != nil {
		return actuators.ActionResult{}, err
	}
	if len(victims) == 0 {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("no victim pods on node %q", a.nodeName),
		}, nil
	}

	held := false
	registered := map[string]bool{}
	var victimCPUs []int
	for i := range victims {
		ann := victims[i].Annotations
		if ann[actuators.AnnHCSetByNode] != a.nodeName {
			continue
		}
		held = true
		if victimCPUs == nil {
			victimCPUs, _ = ParseCPUList(ann[actuators.AnnHCApplied])
		}
		for _, uid := range strings.Split(ann[actuators.AnnHCSecondaries], ",") {
			if uid != "" {
				registered[uid] = true
			}
		}
	}
	if !held {
		if len(p.victimOverride) > 0 {
			victimCPUs = p.victimOverride
		} else if victimCPUs, err = a.victimCPUs(target, victims); err != nil {
			return actuators.ActionResult{}, err
		}
	}
	if p.targetIdleCores >= len(victimCPUs) {
		return actuators.ActionResult{}, fmt.Errorf(
			"hc: target_idle_cores=%d must be < number of victim cores (%d: %s)",
			p.targetIdleCores, len(victimCPUs), FormatCPUList(victimCPUs))
	}

	aggs, err := a.listAggressors(ctx, p, victims)
	if err != nil {
		return actuators.ActionResult{}, err
	}
	if len(aggs) == 0 && !held {
		return actuators.ActionResult{
			Applied: false,
			Reason:  fmt.Sprintf("no aggressors on node %q matching %q", a.nodeName, p.aggSelector),
		}, nil
	}

	a.listenerAddr = p.listenerAddr
	lc := listenerClient{addr: p.listenerAddr, timeout: a.timeout}
	if !held {
		// Annotate first: if we crash after the Listener has state, Reconcile
		// must be able to find something to undo.
		now := time.Now().UTC().Format(time.RFC3339)
		for i := range victims {
			if err := a.annotatePod(ctx, &victims[i], map[string]string{
				actuators.AnnHCApplied:   FormatCPUList(victimCPUs),
				actuators.AnnHCSetByNode: a.nodeName,
				actuators.AnnHCSetAt:     now,
			}); err != nil {
				return actuators.ActionResult{}, fmt.Errorf("hc: annotate victim %s: %w", victims[i].Name, err)
			}
		}
		// The Monitor kthread polls only the cpus in /proc/idlecpu/cpulist,
		// which was set at module load; point it at this victim.
		if err := a.syncMonitorCPUList(victimCPUs); err != nil {
			return actuators.ActionResult{}, err
		}
		if err := lc.send(ctx, PrimaryMsg(PrimaryConfig{
			CPUs:              victimCPUs,
			ACBF:              p.acbf,
			LowTIC:            p.lowTIC,
			TargetIdleCores:   p.targetIdleCores,
			MinSecondaryCores: p.minSecondaryCores,
		})); err != nil {
			return actuators.ActionResult{}, err
		}
	}

	allowed := UnionCPUs(p.secondaryCPUs, victimCPUs)
	var added []string
	var firstErr error
	for i := range aggs {
		pod := &aggs[i]
		uid := string(pod.UID)
		if registered[uid] {
			continue
		}
		if err := a.confine(ctx, pod, allowed); err != nil {
			// Not fatal: the Balancer still narrows affinity; the cpuset is
			// the belt to its braces for threads spawned between rebalances.
			a.logger.Warn("hc: cpuset confine failed; continuing", "pod", pod.Name, "err", err)
		}
		if err := lc.send(ctx, SecondaryMsg(uid)); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			a.logger.Warn("hc: secondary registration failed", "pod", pod.Name, "err", err)
			continue
		}
		registered[uid] = true
		added = append(added, pod.Name)
	}

	uids := make([]string, 0, len(registered))
	for uid := range registered {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	if len(added) > 0 {
		for i := range victims {
			if err := a.annotatePod(ctx, &victims[i], map[string]string{
				actuators.AnnHCSecondaries: strings.Join(uids, ","),
			}); err != nil {
				a.logger.Warn("hc: record secondaries failed", "pod", victims[i].Name, "err", err)
			}
		}
	}

	started := false
	if len(registered) > 0 && !balancerActive(ctx, a.runner) {
		if unitActive(ctx, a.runner, StaticBalancerUnit) {
			// A hand-started Balancer would race ours over the same shared
			// segment and Secondary affinities.
			a.logger.Warn("hc: stopping static balancer unit", "unit", StaticBalancerUnit)
			_, _ = a.runner.Run(ctx, "systemctl", "stop", StaticBalancerUnit)
		}
		workDir := filepath.Join(p.logDir, strconv.FormatInt(time.Now().UnixMilli(), 10))
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return actuators.ActionResult{}, fmt.Errorf("hc: balancer log dir: %w", err)
		}
		out, err := startBalancer(ctx, a.runner, balancerSpec{
			binary:            filepath.Join(p.binDir, "balancer"),
			workDir:           workDir,
			bindCPU:           p.balancerCPU,
			victimCPUs:        victimCPUs,
			secondaryCPUs:     p.secondaryCPUs,
			targetIdleCores:   p.targetIdleCores,
			minSecondaryCores: p.minSecondaryCores,
			acbf:              p.acbf,
			lowTIC:            p.lowTIC,
		})
		if err != nil {
			return actuators.ActionResult{}, fmt.Errorf("hc: start balancer: %w: %s", err, strings.TrimSpace(string(out)))
		}
		started = true
		a.logger.Info("hc: balancer started", "log_dir", workDir)
	}

	res := actuators.ActionResult{
		Applied: !held || len(added) > 0 || started,
		Reason: fmt.Sprintf("hc primary=%s secondaries=%d (+%d)",
			FormatCPUList(victimCPUs), len(registered), len(added)),
		After: map[string]any{
			"victim_cpulist":    FormatCPUList(victimCPUs),
			"secondary_cpulist": FormatCPUList(p.secondaryCPUs),
			"added_pods":        added,
			"secondaries":       len(registered),
			"balancer_started":  started,
		},
	}
	if firstErr != nil && len(registered) == 0 {
		return res, firstErr
	}
	return res, nil
}

// Restore undoes Apply for every annotated pod on this node: stop the
// Balancer (it must exit before Reset, which re-zeroes the control word it
// watches), Reset the Listener, hand the aggressors' threads back to every
// online CPU, then restore their original cpusets. A no-op when nothing on
// this node carries hc annotations.
func (a *Actuator) Restore(ctx context.Context, target actuators.Target) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.restoreLocked(ctx, a.listenerAddr)
}

func (a *Actuator) restoreLocked(ctx context.Context, listenerAddr string) error {
	pods, err := a.client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return fmt.Errorf("hc.Restore: list pods: %w", err)
	}
	var victims, aggs []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Annotations[actuators.AnnHCSetByNode] == a.nodeName {
			victims = append(victims, pod)
		}
		if pod.Annotations[actuators.AnnHCCpusetSetByNode] == a.nodeName {
			aggs = append(aggs, pod)
		}
	}
	if len(victims) == 0 && len(aggs) == 0 {
		return nil
	}

	stopBalancer(ctx, a.runner, a.stopGrace, a.stopPoll)

	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := (listenerClient{addr: listenerAddr, timeout: a.timeout}).send(ctx, ResetMsg()); err != nil {
		a.logger.Warn("hc.Restore: listener reset failed", "err", err)
		keep(err)
	}

	online, err := a.onlineCPUs()
	if err != nil {
		return fmt.Errorf("hc.Restore: online cpus: %w", err)
	}
	for _, pod := range aggs {
		originals := map[string]string{}
		if raw := pod.Annotations[actuators.AnnHCCpusetOriginal]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &originals); err != nil {
				keep(fmt.Errorf("hc.Restore: pod %s: parse cpuset original: %w", pod.Name, err))
				continue
			}
		}
		dirs := make([]string, 0, len(originals))
		for dir := range originals {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		for _, dir := range dirs {
			tids, err := a.resolver.ReadThreads(dir)
			if err != nil {
				// Container gone: nothing left to un-pin.
				a.logger.Info("hc.Restore: no threads to reset", "pod", pod.Name, "dir", dir, "err", err)
			}
			for _, tid := range tids {
				if err := a.setAffinity(tid, online); err != nil {
					keep(fmt.Errorf("hc.Restore: affinity tid %d: %w", tid, err))
				}
			}
		}
		for _, dir := range dirs {
			if _, err := os.Stat(dir); err != nil {
				continue
			}
			if err := a.resolver.WriteCpusetCpus(dir, originals[dir]); err != nil {
				keep(fmt.Errorf("hc.Restore: pod %s: %w", pod.Name, err))
			}
		}
		if err := a.annotatePod(ctx, pod, map[string]string{
			actuators.AnnHCCpusetOriginal:  "",
			actuators.AnnHCCpusetSetByNode: "",
		}); err != nil {
			a.logger.Warn("hc.Restore: clear aggressor annotations failed", "pod", pod.Name, "err", err)
		}
	}
	for _, pod := range victims {
		if err := a.annotatePod(ctx, pod, map[string]string{
			actuators.AnnHCApplied:     "",
			actuators.AnnHCSecondaries: "",
			actuators.AnnHCSetByNode:   "",
			actuators.AnnHCSetAt:       "",
		}); err != nil {
			a.logger.Warn("hc.Restore: clear victim annotations failed", "pod", pod.Name, "err", err)
		}
	}
	a.logger.Info("hc: restored", "victims", len(victims), "aggressors", len(aggs))
	return firstErr
}

// Reconcile restores anything a crashed instance left applied. Restoring is
// the safe startup direction: a stranded Balancer would keep aggressors
// confined with no controller watching the victim.
func (a *Actuator) Reconcile(ctx context.Context) error {
	if err := a.Restore(ctx, actuators.Target{}); err != nil {
		return fmt.Errorf("hc.Reconcile: %w", err)
	}
	return nil
}

// syncMonitorCPUList rewrites /proc/idlecpu/cpulist when it differs from
// cpus. The module prints the list with a trailing comma ("18,19,20,"), and
// parses writes with the same strsep/sscanf as the Listener, so no ranges.
func (a *Actuator) syncMonitorCPUList(cpus []int) error {
	path := filepath.Join(a.procRoot, "idlecpu", "cpulist")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("hc: read %s (is qidlecpu loaded?): %w", path, err)
	}
	if cur, err := ParseCPUList(strings.TrimRight(strings.TrimSpace(string(raw)), ",")); err == nil && equalCPUs(cur, cpus) {
		return nil
	}
	if err := os.WriteFile(path, []byte(FormatCPUList(cpus)), 0644); err != nil {
		return fmt.Errorf("hc: write %s: %w", path, err)
	}
	a.logger.Info("hc: monitor cpulist updated", "cpus", FormatCPUList(cpus))
	return nil
}

// confine narrows every container cgroup of pod to cpus, recording the
// originals on the pod first.
func (a *Actuator) confine(ctx context.Context, pod *corev1.Pod, cpus []int) error {
	dirs, err := a.containerDirs(pod, "")
	if err != nil {
		return err
	}
	originals := map[string]string{}
	for _, dir := range dirs {
		orig, err := a.resolver.ReadCpusetCpus(dir)
		if err != nil {
			return err
		}
		originals[dir] = orig
	}
	body, _ := json.Marshal(originals)
	if err := a.annotatePod(ctx, pod, map[string]string{
		actuators.AnnHCCpusetOriginal:  string(body),
		actuators.AnnHCCpusetSetByNode: a.nodeName,
	}); err != nil {
		return fmt.Errorf("annotate: %w", err)
	}
	want := FormatCPUList(cpus)
	for _, dir := range dirs {
		if err := a.resolver.WriteCpusetCpus(dir, want); err != nil {
			return err
		}
	}
	return nil
}

// victimCPUs is the union of Cpus_allowed_list over every task in the victim
// containers. It refuses an answer equal to the whole machine: that means
// the victim is not pinned and HC would have nothing to protect.
func (a *Actuator) victimCPUs(target actuators.Target, victims []corev1.Pod) ([]int, error) {
	var union []int
	for i := range victims {
		dirs, err := a.containerDirs(&victims[i], target.Spec.ContainerName)
		if err != nil {
			return nil, fmt.Errorf("hc: victim %s: %w", victims[i].Name, err)
		}
		for _, dir := range dirs {
			pids, err := a.resolver.ReadProcs(dir)
			if err != nil {
				return nil, fmt.Errorf("hc: victim %s: %w", victims[i].Name, err)
			}
			for _, pid := range pids {
				cpus, err := a.allowedCPUs(pid)
				if err != nil {
					continue // task exited
				}
				union = UnionCPUs(union, cpus)
			}
		}
	}
	if len(union) == 0 {
		return nil, fmt.Errorf("hc: could not derive victim cpulist; set victim_cpulist")
	}
	if online, err := a.onlineCPUs(); err == nil && equalCPUs(union, online) {
		return nil, fmt.Errorf("hc: victim tasks may run on every online cpu (not pinned); set victim_cpulist")
	}
	return union, nil
}

func (a *Actuator) allowedCPUs(pid int) ([]int, error) {
	f, err := os.Open(filepath.Join(a.procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "Cpus_allowed_list:"); ok {
			return ParseCPUList(v)
		}
	}
	return nil, fmt.Errorf("no Cpus_allowed_list in /proc/%d/status", pid)
}

// containerDirs resolves the cgroup dir of each running container of pod
// (only containerName when non-empty), de-duplicated.
func (a *Actuator) containerDirs(pod *corev1.Pod, containerName string) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID == "" || (containerName != "" && cs.Name != containerName) {
			continue
		}
		dir, err := a.resolver.PathForPod(string(pod.UID), cs.ContainerID)
		if err != nil {
			return nil, err
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("no running container with ID")
	}
	return dirs, nil
}

func (a *Actuator) listVictims(ctx context.Context, target actuators.Target) ([]corev1.Pod, error) {
	sel := labels.SelectorFromSet(labels.Set(target.Spec.Selector)).String()
	pods, err := a.client.CoreV1().Pods(target.Spec.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: sel,
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("hc: list victim pods: %w", err)
	}
	return pods.Items, nil
}

func (a *Actuator) listAggressors(ctx context.Context, p applyParams, victims []corev1.Pod) ([]corev1.Pod, error) {
	pods, err := a.client.CoreV1().Pods(p.aggNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: p.aggSelector,
		FieldSelector: "spec.nodeName=" + a.nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("hc: list aggressors: %w", err)
	}
	isVictim := map[types.UID]bool{}
	for i := range victims {
		isVictim[victims[i].UID] = true
	}
	out := pods.Items[:0]
	for _, pod := range pods.Items {
		if isVictim[pod.UID] || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

func (a *Actuator) annotatePod(ctx context.Context, pod *corev1.Pod, ann map[string]string) error {
	out := make(map[string]any, len(ann))
	for k, v := range ann {
		if v == "" {
			out[k] = nil // merge-patch null removes the annotation
		} else {
			out[k] = v
		}
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": out},
	})
	if err != nil {
		return err
	}
	_, err = a.client.CoreV1().Pods(pod.Namespace).Patch(
		ctx, pod.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func hostOnlineCPUs() ([]int, error) {
	raw, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return nil, err
	}
	return ParseCPUList(string(raw))
}

func paramStr(params map[string]any, key, def string) string {
	if v, ok := params[key]; ok {
		switch x := v.(type) {
		case string:
			if x != "" {
				return x
			}
		case fmt.Stringer:
			return x.String()
		case int, int64, float64:
			return fmt.Sprint(x)
		}
	}
	return def
}

func paramInt(params map[string]any, key string, def int) int {
	v, ok := params[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case float32:
		return int(x)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return def
}

func paramFloat(params map[string]any, key string, def float64) float64 {
	v, ok := params[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case float64:
		return x
	case float32:
		return float64(x)
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return f
		}
	}
	return def
}

// Compile-time interface check.
var _ actuators.Actuator = (*Actuator)(nil)
