package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCPUMax(t *testing.T) {
	cases := []struct {
		in   string
		want CPUMax
		err  bool
	}{
		{"max 100000", CPUMax{Quota: -1, Period: 100000}, false},
		{"50000 100000", CPUMax{Quota: 50000, Period: 100000}, false},
		{"  50000   100000  \n", CPUMax{Quota: 50000, Period: 100000}, false},
		{"max", CPUMax{}, true},
		{"abc 100000", CPUMax{}, true},
		{"50000 0", CPUMax{}, true},
	}
	for _, c := range cases {
		got, err := ParseCPUMax(c.in)
		if c.err {
			if err == nil {
				t.Fatalf("ParseCPUMax(%q): expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseCPUMax(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("ParseCPUMax(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestCPUMaxString(t *testing.T) {
	if got := (CPUMax{Quota: -1, Period: 100000}).String(); got != "max 100000" {
		t.Fatalf("got %q", got)
	}
	if got := (CPUMax{Quota: 50000, Period: 100000}).String(); got != "50000 100000" {
		t.Fatalf("got %q", got)
	}
}

// TestResolverFixtureFilesystem builds a small cgroup tree in a tempdir
// and verifies PathForPod + Read/WriteCPUMax all work end-to-end.
func TestResolverFixtureFilesystem(t *testing.T) {
	root := t.TempDir()
	podUID := "abcdef12-3456-7890-abcd-ef1234567890"
	// systemd-style nesting with underscores in the slice name.
	podDir := filepath.Join(root,
		"kubepods.slice",
		"kubepods-burstable.slice",
		"kubepods-burstable-pod"+replaceDashWithUnderscore(podUID)+".slice",
	)
	containerDir := filepath.Join(podDir, "cri-containerd-1234567890abcdef.scope")
	if err := os.MkdirAll(containerDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "cpu.max"), []byte("max 100000\n"), 0644); err != nil {
		t.Fatalf("write cpu.max: %v", err)
	}

	r := &Resolver{CgroupRoot: root}
	got, err := r.PathForPod(podUID, "containerd://1234567890abcdef")
	if err != nil {
		t.Fatalf("PathForPod: %v", err)
	}
	if got != containerDir {
		t.Fatalf("PathForPod = %q, want %q", got, containerDir)
	}

	v, err := r.ReadCPUMax(got)
	if err != nil {
		t.Fatalf("ReadCPUMax: %v", err)
	}
	if v.Quota != -1 || v.Period != 100000 {
		t.Fatalf("read got %+v", v)
	}

	if err := r.WriteCPUMax(got, CPUMax{Quota: 50000, Period: 100000}); err != nil {
		t.Fatalf("WriteCPUMax: %v", err)
	}
	v2, err := r.ReadCPUMax(got)
	if err != nil {
		t.Fatalf("ReadCPUMax 2: %v", err)
	}
	if v2.Quota != 50000 {
		t.Fatalf("after write got %+v", v2)
	}
}

func replaceDashWithUnderscore(s string) string {
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '-' {
			out[i] = '_'
		} else {
			out[i] = s[i]
		}
	}
	return string(out)
}

func TestCpusetAndIDListRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := &Resolver{CgroupRoot: dir}
	if err := os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte("\n"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := r.ReadCpusetCpus(dir)
	if err != nil || got != "" {
		t.Fatalf("ReadCpusetCpus empty = %q, %v", got, err)
	}
	if err := r.WriteCpusetCpus(dir, "18-24"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ReadCpusetCpus(dir); got != "18-24" {
		t.Fatalf("ReadCpusetCpus = %q, want 18-24", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.threads"), []byte("10\n11\n12\n"), 0644); err != nil {
		t.Fatal(err)
	}
	ids, err := r.ReadThreads(dir)
	if err != nil || len(ids) != 3 || ids[2] != 12 {
		t.Fatalf("ReadThreads = %v, %v", ids, err)
	}
	if _, err := r.ReadProcs(dir); err == nil {
		t.Fatal("ReadProcs on missing file: expected error")
	}
}
