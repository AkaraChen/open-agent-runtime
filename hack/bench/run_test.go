package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The builder and the agent report these fields (other branches); the JSON
// names are the contract.
func TestNewFieldsDecode(t *testing.T) {
	var bo buildOut
	if err := json.Unmarshal([]byte(`{"stats":{"total":2e9,"eval":6.4e9,"nixSource":6.1e9,"components":4e9,"componentBuild":3.9e9}}`), &bo); err != nil {
		t.Fatal(err)
	}
	if bo.Stats.NixSource != 6.1e9 || bo.Stats.ComponentBuild != 3.9e9 || bo.Stats.Eval != 6.4e9 {
		t.Errorf("build stats: %+v", bo.Stats)
	}

	var r runOut
	if err := json.Unmarshal([]byte(`{"result":{"timings":{"create":19500000,"total":42200000,"runcExecs":3,"cpuUser":12000000,"cpuSys":8000000}}}`), &r); err != nil {
		t.Fatal(err)
	}
	tm := r.Result.Timings
	if tm.RuncExecs != 3 || tm.CPUUser != 12e6 || tm.CPUSys != 8e6 || tm.Create != 19.5e6 || tm.Total != 42.2e6 {
		t.Errorf("timings: %+v", tm)
	}
}

func TestExecCPU(t *testing.T) {
	var with, without runOut
	json.Unmarshal([]byte(`{"result":{"timings":{"runcExecs":3,"cpuUser":12000000,"cpuSys":8000000}}}`), &with)
	json.Unmarshal([]byte(`{"result":{"timings":{"total":42200000}}}`), &without)

	b := &bench{res: &Result{}}
	var ec execCPU
	ec.observe(&with)
	ec.observe(&with)
	ec.record(b, "4-start", "execs", "execs", "cpu", "cpu")
	var old execCPU // an agent that reports neither: n/a, not a row of zeros
	old.observe(&without)
	old.record(b, "5-switch", "x.execs", "execs", "x.cpu", "cpu")

	want := []struct {
		key, status string
		p50         float64
	}{{"execs", "ok", 3}, {"cpu", "ok", 20}, {"x.execs", "na", 0}, {"x.cpu", "na", 0}}
	if len(b.res.Metrics) != len(want) {
		t.Fatalf("metrics: %+v", b.res.Metrics)
	}
	for i, w := range want {
		m := b.res.Metrics[i]
		if m.Key != w.key || m.Status != w.status || m.P50 != w.p50 {
			t.Errorf("metric %d = {%s %s %v}, want %+v", i, m.Key, m.Status, m.P50, w)
		}
	}
	if m := b.res.Metrics[0]; m.N != 2 || m.Unit != "个" || !m.LowerIsBetter {
		t.Errorf("execs metric: %+v", m)
	}
	if m := b.res.Metrics[1]; m.Unit != "ms" || !strings.Contains(m.Note, "user 12.0 + sys 8.0 ms") {
		t.Errorf("cpu metric: %+v", m)
	}
}

func TestBuildSplit(t *testing.T) {
	var o buildOut
	o.Stats.Total, o.Stats.Base, o.Stats.Eval, o.Stats.Fetch, o.Stats.Components = 60e6, 5e6, 0, 3e6, 57e6

	// a builder that does not report the finer split: the text of the
	// earlier benchmark reports (13-baseline, kvm, no-op rebuild)
	want := "构建器 60 ms（基础镜像 5、求值 0、下载 3、组件 57），其余（CLI 进程、配置、锁文件）4 ms"
	if got := buildSplit(&o, 64*time.Millisecond); got != want {
		t.Errorf("without the finer split:\n got %s\nwant %s", got, want)
	}

	// with it: nixpkgs source inside the evaluation, the user's build
	// command and the import inside the components
	o.Stats.Eval, o.Stats.NixSource = 6410e6, 6100e6
	o.Stats.Components, o.Stats.ComponentBuild = 4020e6, 3950e6
	want = "构建器 60 ms（基础镜像 5、求值 6410（其中 nixpkgs 源码获取 6100）、下载 3、组件 4020（其中用户 build 命令 3950、导入 70）），其余（CLI 进程、配置、锁文件）4 ms"
	if got := buildSplit(&o, 64*time.Millisecond); got != want {
		t.Errorf("with the finer split:\n got %s\nwant %s", got, want)
	}

	// the import time never goes negative when the two timers disagree
	o.Stats.ComponentBuild = 5000e6
	if got := buildSplit(&o, 64*time.Millisecond); !strings.Contains(got, "导入 0）") {
		t.Errorf("import time must clamp at zero: %s", got)
	}
}

func TestReuseResult(t *testing.T) {
	b := &bench{res: &Result{}}
	r := reuseResult{
		steps: []reuseStep{
			{view: 130, wall: 300},
			{view: 12, wall: 180},
			{view: 11, wall: 175},
			{back: true, view: 4, wall: 90},
		},
		steals: 3, rebuilds: 1,
	}
	r.record(b, "2-warm")
	want := []struct {
		key, unit string
		n         int
		p50       float64
		lower     bool
	}{
		{"reuse.view.first", "ms", 1, 130, true},
		{"reuse.view.next", "ms", 2, 11, true},
		{"reuse.view.back", "ms", 1, 4, true},
		{"reuse.steals", "个", 1, 3, false},
		{"reuse.rebuilds", "个", 1, 1, true},
	}
	if len(b.res.Metrics) != len(want) {
		t.Fatalf("metrics: %+v", b.res.Metrics)
	}
	for i, w := range want {
		m := b.res.Metrics[i]
		if m.Group != "2-warm" || m.Key != w.key || m.Unit != w.unit || m.N != w.n || m.P50 != w.p50 || m.LowerIsBetter != w.lower || m.Status != "ok" {
			t.Errorf("metric %d = %+v, want %+v", i, m, w)
		}
	}
	if m := b.res.Metrics[1]; !strings.Contains(m.Note, "12.0 / 11.0") || !strings.Contains(m.Note, "180.0 / 175.0") {
		t.Errorf("per-reload times missing from the note: %s", m.Note)
	}

	// a run that stopped after the first reload has no later ones, and one
	// that never got to reload is n/a, not a row of zeros
	b = &bench{res: &Result{}}
	(&reuseResult{steps: []reuseStep{{view: 130, wall: 300}}}).record(b, "2-warm")
	if len(b.res.Metrics) != 3 || b.res.Metrics[0].Key != "reuse.view.first" || b.res.Metrics[1].Key != "reuse.steals" {
		t.Errorf("one reload: %+v", b.res.Metrics)
	}
	b = &bench{res: &Result{}}
	(&reuseResult{}).record(b, "2-warm")
	if len(b.res.Metrics) != 1 || b.res.Metrics[0].Status != "na" {
		t.Errorf("no reload: %+v", b.res.Metrics)
	}
}
