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
