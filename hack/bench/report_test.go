package main

import (
	"strings"
	"testing"
)

func TestCPUModel(t *testing.T) {
	for _, c := range []struct{ host, want string }{
		{"linux/amd64 runnervm8k3b6 · AMD EPYC 7763 64-Core Processor · 4 CPU", "AMD EPYC 7763 64-Core Processor"},
		{"darwin/arm64 host.local · Apple M1 (Virtual) · 3 CPU", "Apple M1 (Virtual)"},
		{"linux/amd64 box · Intel(R)  Xeon(R)   Platinum · 8 CPU", "Intel(R) Xeon(R) Platinum"},
		{"linux/arm64 box · 4 CPU", ""}, // /proc/cpuinfo has no model name on arm64 Linux
		{"", ""},
	} {
		if got := cpuModel(c.host); got != c.want {
			t.Errorf("cpuModel(%q) = %q, want %q", c.host, got, c.want)
		}
	}
}

func TestDifferentCPU(t *testing.T) {
	epyc := &Result{Host: "linux/amd64 a · AMD EPYC 7763 64-Core Processor · 4 CPU"}
	epycHost := &Result{Host: "linux/amd64 b · AMD EPYC 7763 64-Core Processor · 4 CPU"}
	xeon := &Result{Host: "linux/amd64 c · Intel(R) Xeon(R) 6973P-C · 4 CPU"}
	upper := &Result{Host: "linux/amd64 d · INTEL(R) XEON(R) 6973P-C · 4 CPU"}
	unknown := &Result{Host: "linux/arm64 e · 4 CPU"}
	for _, c := range []struct {
		name      string
		cur, prev *Result
		want      bool
	}{
		{"same model, other host", epyc, epycHost, false},
		{"different model", epyc, xeon, true},
		{"case only", xeon, upper, false},
		{"unknown current", unknown, xeon, false},
		{"unknown baseline", xeon, unknown, false},
		{"no baseline", xeon, nil, false},
	} {
		if got := differentCPU(c.cur, c.prev); got != c.want {
			t.Errorf("%s: differentCPU = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDelta(t *testing.T) {
	prev := &Metric{Status: "ok", P50: 100, LowerIsBetter: true}
	for _, c := range []struct {
		name    string
		p50     float64
		cpuDiff bool
		want    string
	}{
		{"better", 50, false, " (🟢-50%)"},
		{"worse", 150, false, " (🔺+50%)"},
		{"within 5%", 102, false, " (≈)"},
		{"better, other CPU", 50, true, " (-50%，CPU 不同)"},
		{"worse, other CPU", 150, true, " (+50%，CPU 不同)"},
		{"small, other CPU", 102, true, " (+2%，CPU 不同)"},
		{"unchanged, other CPU", 100, true, " (0%，CPU 不同)"},
	} {
		m := &Metric{Status: "ok", P50: c.p50, LowerIsBetter: true}
		if got := delta(m, prev, c.cpuDiff); got != c.want {
			t.Errorf("%s: delta = %q, want %q", c.name, got, c.want)
		}
	}
	// no verdict can be drawn without two measurements
	if got := delta(&Metric{Status: "na"}, prev, true); got != "" {
		t.Errorf("na metric: delta = %q, want none", got)
	}
	if got := delta(&Metric{Status: "ok", P50: 1}, &Metric{Status: "ok"}, true); got != "" {
		t.Errorf("zero baseline: delta = %q, want none", got)
	}
}

func result(backend, host string, p50 float64) *Result {
	m := Metric{Group: "4-start", Key: "e2e", Name: "启动", Unit: "ms", Status: "ok", N: 1, P50: p50, LowerIsBetter: true}
	return &Result{Backend: backend, Platform: "linux/amd64", Host: host, Metrics: []Metric{m}}
}

func TestRenderCPUAware(t *testing.T) {
	const epyc = "linux/amd64 a · AMD EPYC 7763 64-Core Processor · 4 CPU"
	const xeon = "linux/amd64 b · Intel(R) Xeon(R) 6973P-C · 4 CPU"
	results := []*Result{result("kvm", xeon, 50), result("native", epyc, 50)}
	base := map[string]*Result{"kvm": result("kvm", epyc, 100), "native": result("native", epyc, 100)}
	out := render("t", "", results, base)
	if !strings.Contains(out, "| 启动 | ms | 50.0 (-50%，CPU 不同) | 50.0 (🟢-50%) |") {
		t.Errorf("kvm (other CPU) must be plain text and native (same CPU) keep its verdict:\n%s", out)
	}
	if !strings.Contains(out, "kvm（基线 AMD EPYC 7763 64-Core Processor → 本次 Intel(R) Xeon(R) 6973P-C）") {
		t.Errorf("the environment note must name the backend and both models:\n%s", out)
	}
	if strings.Contains(out, "native（基线") {
		t.Errorf("native ran on the baseline's CPU and must not be listed:\n%s", out)
	}

	// the same CPU everywhere: no note, no marker
	same := render("t", "", []*Result{result("kvm", epyc, 50)}, map[string]*Result{"kvm": result("kvm", epyc, 100)})
	if strings.Contains(same, "CPU 不同") || !strings.Contains(same, "50.0 (🟢-50%)") {
		t.Errorf("same-CPU comparison must be unchanged:\n%s", same)
	}
}
