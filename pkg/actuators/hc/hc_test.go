package hc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-workspace/simple-mitigation-1/pkg/actuators"
	"github.com/coding-workspace/simple-mitigation-1/pkg/cgroup"
	"github.com/coding-workspace/simple-mitigation-1/pkg/targets"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "", false},
		{"18-20,22", "18,19,20,22", false},
		{" 3,1,2,2 \n", "1,2,3", false},
		{"0-3,2-5", "0,1,2,3,4,5", false},
		{"18,19,20,", "18,19,20", false}, // /proc/idlecpu/cpulist trailing comma
		{"5-3", "", true},
		{"a", "", true},
	}
	for _, c := range cases {
		got, err := ParseCPUList(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("ParseCPUList(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseCPUList(%q): %v", c.in, err)
		}
		if FormatCPUList(got) != c.want {
			t.Fatalf("ParseCPUList(%q) = %q, want %q", c.in, FormatCPUList(got), c.want)
		}
	}
	if got := FormatCPUList(UnionCPUs([]int{21, 22}, []int{18, 19, 20, 21})); got != "18,19,20,21,22" {
		t.Fatalf("UnionCPUs = %q", got)
	}
}

func TestMessagesAreAllStrings(t *testing.T) {
	for _, msg := range []map[string]string{
		PrimaryMsg(PrimaryConfig{CPUs: []int{18, 19, 20}, ACBF: 0.33, LowTIC: 1, TargetIdleCores: 1}),
		SecondaryMsg("abc-def"),
		ResetMsg(),
	} {
		body, _ := json.Marshal(msg)
		var generic map[string]any
		if err := json.Unmarshal(body, &generic); err != nil {
			t.Fatal(err)
		}
		for k, v := range generic {
			if _, ok := v.(string); !ok {
				t.Fatalf("%s: field %s is %T, Listener requires strings", body, k, v)
			}
		}
	}
	p := PrimaryMsg(PrimaryConfig{CPUs: []int{18, 19, 20}, ACBF: 0.33, LowTIC: 1, TargetIdleCores: 1})
	if p["cpuList"] != "18,19,20" || p["LOWIDLEFREQ_THRESHOLD"] != "0.33" ||
		p["targetIdleCores"] != "1" || p["static_targetIdleCores"] != "1" || p["minSecondaryCores"] != "0" {
		t.Fatalf("PrimaryMsg = %v", p)
	}
	if s := SecondaryMsg("abc-def"); s["pod_id"] != "podabc-def" || s["parent"] != "any" {
		t.Fatalf("SecondaryMsg = %v", s)
	}
}

// fakeListener mimics Listener/listener.cpp: read to EOF, reply 4 bytes.
type fakeListener struct {
	ln     net.Listener
	mu     sync.Mutex
	msgs   []map[string]string
	nak    func(map[string]string) bool
	events *eventLog
}

func newFakeListener(t *testing.T, ev *eventLog) *fakeListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeListener{ln: ln, events: ev}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			body, _ := io.ReadAll(c)
			var m map[string]string
			_ = json.Unmarshal(body, &m)
			f.mu.Lock()
			f.msgs = append(f.msgs, m)
			nak := f.nak != nil && f.nak(m)
			f.mu.Unlock()
			if ev != nil {
				ev.add("listener:" + m["ctr_type"])
			}
			if nak {
				_, _ = c.Write([]byte("NAK\x00"))
			} else {
				_, _ = c.Write([]byte("ACK\x00"))
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeListener) addr() string { return f.ln.Addr().String() }

func (f *fakeListener) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range f.msgs {
		s := m["ctr_type"]
		if m["pod_id"] != "" {
			s += ":" + m["pod_id"]
		}
		out = append(out, s)
	}
	return out
}

func TestListenerClientACKNAK(t *testing.T) {
	f := newFakeListener(t, nil)
	f.nak = func(m map[string]string) bool { return m["ctr_type"] == "Secondary" }
	c := listenerClient{addr: f.addr(), timeout: 2 * time.Second}
	start := time.Now()
	if err := c.send(context.Background(), ResetMsg()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("send took %v; half-close should avoid the Listener's 1s read timeout", time.Since(start))
	}
	if err := c.send(context.Background(), SecondaryMsg("u")); !errors.Is(err, ErrNAK) {
		t.Fatalf("Secondary: want ErrNAK, got %v", err)
	}
}

type eventLog struct {
	mu sync.Mutex
	ev []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	e.ev = append(e.ev, s)
	e.mu.Unlock()
}

func (e *eventLog) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ev...)
}

// fakeSystemd tracks only whether the dynamic unit is up.
type fakeSystemd struct {
	mu       sync.Mutex
	active   bool
	starts   int
	lastArgs []string
	events   *eventLog
}

func (s *fakeSystemd) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case name == "systemd-run":
		s.active = true
		s.starts++
		s.lastArgs = args
		s.events.add("systemd-run")
		return nil, nil
	case name == "systemctl" && args[0] == "is-active":
		if args[1] == BalancerUnit && s.active {
			return []byte("active\n"), nil
		}
		return []byte("inactive\n"), errors.New("exit status 3")
	case name == "systemctl" && args[0] == "kill":
		s.active = false
		s.events.add("balancer:kill")
		return nil, nil
	case name == "systemctl" && args[0] == "stop":
		s.events.add("stop:" + args[1])
		if args[1] == BalancerUnit {
			s.active = false
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %s %v", name, args)
}

type fixture struct {
	t        *testing.T
	client   *stubClient
	cgRoot   string
	procRoot string
	events   *eventLog
	sysd     *fakeSystemd
	lis      *fakeListener
	act      *Actuator
	target   actuators.Target
	affinity map[int][]int
}

const node = "node-4"

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:        t,
		client:   newStubClient(),
		cgRoot:   t.TempDir(),
		procRoot: t.TempDir(),
		events:   &eventLog{},
		affinity: map[int][]int{},
	}
	f.sysd = &fakeSystemd{events: f.events}
	f.lis = newFakeListener(t, f.events)
	mustWrite(t, filepath.Join(f.procRoot, "idlecpu", "cpulist"), "2,4,6,8,")
	var amu sync.Mutex
	act, err := New(f.client, node, &cgroup.Resolver{CgroupRoot: f.cgRoot}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithProcRoot(f.procRoot),
		WithRunner(f.sysd),
		WithListenerAddr(f.lis.addr()),
		WithOnlineCPUs(func() ([]int, error) { return ParseCPUList("0-31") }),
		WithAffinityFunc(func(tid int, cpus []int) error {
			amu.Lock()
			f.affinity[tid] = cpus
			amu.Unlock()
			f.events.add(fmt.Sprintf("affinity:%d", tid))
			return nil
		}),
		WithStopTiming(time.Second, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	f.act = act
	f.target = actuators.Target{
		Spec: &targets.Target{
			Name: "search", Namespace: "default",
			Selector:      map[string]string{"io.kompose.service": "search"},
			ContainerName: "hotel-reserv-search",
		},
		NodeName: node,
	}
	return f
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// addPod creates the pod in the fake API and a cgroupfs-driver layout for
// it: kubepods/<qos>/pod<uid>/<cid>/{cgroup.procs,cgroup.threads,cpuset.cpus}.
func (f *fixture) addPod(name, uid, ns string, lbls map[string]string, container, cid string, pids []int, allowed string) string {
	f.t.Helper()
	dir := filepath.Join(f.cgRoot, "kubepods", "besteffort", "pod"+uid, cid)
	var ids []string
	for _, p := range pids {
		ids = append(ids, fmt.Sprint(p))
		mustWrite(f.t, filepath.Join(f.procRoot, fmt.Sprint(p), "status"),
			"Name:\tx\nCpus_allowed:\tffffffff\nCpus_allowed_list:\t"+allowed+"\n")
	}
	mustWrite(f.t, filepath.Join(dir, "cgroup.procs"), strings.Join(ids, "\n")+"\n")
	mustWrite(f.t, filepath.Join(dir, "cgroup.threads"), strings.Join(ids, "\n")+"\n")
	mustWrite(f.t, filepath.Join(dir, "cpuset.cpus"), "\n")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), Labels: lbls},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: container, ContainerID: "docker://" + cid, Ready: true},
			},
		},
	}
	if _, err := f.client.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func (f *fixture) pod(ns, name string) *corev1.Pod {
	f.t.Helper()
	p, err := f.client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) params() map[string]any {
	return map[string]any{
		"listener_addr":       f.lis.addr(),
		"log_dir":             f.t.TempDir(),
		"secondary_cpulist":   "21-24",
		"target_idle_cores":   1,
		"aggressor_selector":  "gordion-aggr=true",
		"aggressor_namespace": "*",
	}
}

func TestApplyIdempotentThenRestore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	victim := map[string]string{"io.kompose.service": "search"}
	aggr := map[string]string{"gordion-aggr": "true"}
	f.addPod("search-1", "vic-1", "default", victim, "hotel-reserv-search", "vicc1", []int{100, 101}, "18-20")
	agg1 := f.addPod("cp-1", "agg-1", "aggressor-sn", aggr, "compose-post", "aggc1", []int{200, 201}, "0-31")

	res, err := f.act.Apply(ctx, f.target, f.params())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Applied || res.After["victim_cpulist"] != "18,19,20" {
		t.Fatalf("Apply result = %+v", res)
	}
	if got := strings.Join(f.lis.types(), " "); got != "Primary Secondary:podagg-1" {
		t.Fatalf("listener got %q", got)
	}
	if got := readFile(t, filepath.Join(f.procRoot, "idlecpu", "cpulist")); got != "18,19,20" {
		t.Fatalf("monitor cpulist = %q", got)
	}
	if got := readFile(t, filepath.Join(agg1, "cpuset.cpus")); got != "18,19,20,21,22,23,24" {
		t.Fatalf("aggressor cpuset = %q", got)
	}
	if f.sysd.starts != 1 {
		t.Fatalf("balancer starts = %d", f.sysd.starts)
	}
	args := strings.Join(f.sysd.lastArgs, " ")
	if !strings.Contains(args, "--unit="+BalancerUnit) || !strings.HasSuffix(args, "balancer 0 1 0 18,19,20 21,22,23,24") {
		t.Fatalf("systemd-run args = %q", args)
	}
	v := f.pod("default", "search-1")
	if v.Annotations[actuators.AnnHCApplied] != "18,19,20" || v.Annotations[actuators.AnnHCSecondaries] != "agg-1" {
		t.Fatalf("victim annotations = %v", v.Annotations)
	}

	// A second aggressor appears; re-Apply registers only it.
	agg2 := f.addPod("cart-1", "agg-2", "aggressor-ss", aggr, "cart", "aggc2", []int{300}, "0-31")
	res, err = f.act.Apply(ctx, f.target, f.params())
	if err != nil {
		t.Fatalf("re-Apply: %v", err)
	}
	if got := strings.Join(f.lis.types(), " "); got != "Primary Secondary:podagg-1 Secondary:podagg-2" {
		t.Fatalf("listener after re-Apply got %q", got)
	}
	if f.sysd.starts != 1 || !res.Applied {
		t.Fatalf("re-Apply: starts=%d res=%+v", f.sysd.starts, res)
	}
	// Nothing new: a third Apply is a no-op.
	if res, _ = f.act.Apply(ctx, f.target, f.params()); res.Applied || len(f.lis.types()) != 3 {
		t.Fatalf("third Apply should be a no-op: %+v, msgs=%v", res, f.lis.types())
	}

	before := len(f.events.list())
	if err := f.act.Restore(ctx, f.target); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	ev := f.events.list()[before:]
	idx := func(s string) int {
		for i, e := range ev {
			if e == s {
				return i
			}
		}
		return -1
	}
	kill, reset, aff := idx("balancer:kill"), idx("listener:Reset"), idx("affinity:200")
	if kill < 0 || reset < 0 || aff < 0 || !(kill < reset && reset < aff) {
		t.Fatalf("restore order wrong: %v", ev)
	}
	for _, tid := range []int{200, 201, 300} {
		if FormatCPUList(f.affinity[tid]) != FormatCPUList(mustCPUs(t, "0-31")) {
			t.Fatalf("tid %d affinity = %v", tid, f.affinity[tid])
		}
	}
	for _, dir := range []string{agg1, agg2} {
		if got := readFile(t, filepath.Join(dir, "cpuset.cpus")); got != "" {
			t.Fatalf("%s cpuset not restored: %q", dir, got)
		}
	}
	for _, p := range []*corev1.Pod{f.pod("default", "search-1"), f.pod("aggressor-sn", "cp-1"), f.pod("aggressor-ss", "cart-1")} {
		for k := range p.Annotations {
			if strings.HasPrefix(k, "mitigation/hc-") {
				t.Fatalf("%s still has %s", p.Name, k)
			}
		}
	}

	// Nothing annotated: Restore and Reconcile are no-ops.
	before = len(f.events.list())
	if err := f.act.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.events.list()) != before {
		t.Fatalf("Reconcile on clean node acted: %v", f.events.list()[before:])
	}
}

func TestApplyStopsStaticBalancer(t *testing.T) {
	f := newFixture(t)
	f.addPod("search-1", "vic-1", "default", map[string]string{"io.kompose.service": "search"},
		"hotel-reserv-search", "vicc1", []int{100}, "18-20")
	f.addPod("cp-1", "agg-1", "default", map[string]string{"gordion-aggr": "true"}, "c", "aggc1", []int{200}, "0-31")
	static := &staticActive{fakeSystemd: f.sysd}
	f.act.runner = static
	if _, err := f.act.Apply(context.Background(), f.target, f.params()); err != nil {
		t.Fatal(err)
	}
	if !static.stopped {
		t.Fatal("static hc-balancer unit was not stopped before starting the dynamic one")
	}
}

type staticActive struct {
	*fakeSystemd
	stopped bool
}

func (s *staticActive) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == "systemctl" && len(args) == 2 && args[1] == StaticBalancerUnit {
		switch args[0] {
		case "is-active":
			if !s.stopped {
				return []byte("active\n"), nil
			}
		case "stop":
			s.stopped = true
			return nil, nil
		}
	}
	return s.fakeSystemd.Run(ctx, name, args...)
}

func TestApplyValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.addPod("search-1", "vic-1", "default", map[string]string{"io.kompose.service": "search"},
		"hotel-reserv-search", "vicc1", []int{100}, "18-20")
	f.addPod("cp-1", "agg-1", "default", map[string]string{"gordion-aggr": "true"}, "c", "aggc1", []int{200}, "0-31")

	p := f.params()
	p["target_idle_cores"] = 3
	if _, err := f.act.Apply(ctx, f.target, p); err == nil || !strings.Contains(err.Error(), "target_idle_cores") {
		t.Fatalf("want target_idle_cores error, got %v", err)
	}
	p = f.params()
	delete(p, "secondary_cpulist")
	if _, err := f.act.Apply(ctx, f.target, p); err == nil {
		t.Fatal("want secondary_cpulist error")
	}
	if len(f.lis.types()) != 0 {
		t.Fatalf("rejected Apply must not talk to the Listener: %v", f.lis.types())
	}
}

func TestApplyRefusesUnpinnedVictim(t *testing.T) {
	f := newFixture(t)
	f.addPod("search-1", "vic-1", "default", map[string]string{"io.kompose.service": "search"},
		"hotel-reserv-search", "vicc1", []int{100}, "0-31")
	f.addPod("cp-1", "agg-1", "default", map[string]string{"gordion-aggr": "true"}, "c", "aggc1", []int{200}, "0-31")
	_, err := f.act.Apply(context.Background(), f.target, f.params())
	if err == nil || !strings.Contains(err.Error(), "not pinned") {
		t.Fatalf("want not-pinned error, got %v", err)
	}
	// The override unblocks it.
	p := f.params()
	p["victim_cpulist"] = "18-20"
	res, err := f.act.Apply(context.Background(), f.target, p)
	if err != nil || res.After["victim_cpulist"] != "18,19,20" {
		t.Fatalf("override Apply: %+v, %v", res, err)
	}
}

func mustCPUs(t *testing.T, s string) []int {
	t.Helper()
	c, err := ParseCPUList(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
