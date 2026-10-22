package main

import (
	"strings"
	"testing"
	"time"
)

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
