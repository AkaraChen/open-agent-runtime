package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var groupTitles = map[string]string{
	"0-env":         "0. 环境基准（判断 runner 噪声用，与 oar 无关）",
	"1-build":       "1. 构建",
	"2-warm":        "2. 分发与预热",
	"3-vm":          "3. 虚拟机层",
	"4-start":       "4. 组合与启动",
	"5-switch":      "5. 切换（reload）",
	"6-runtime":     "6. 运行时开销",
	"7-density":     "7. 密度与扩展性",
	"8-correctness": "8. 正确性与隔离",
	"9-snapshot":    "9. 快照",
}

func loadResults(paths []string) ([]*Result, error) {
	var out []*Result
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r Result
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, &r)
	}
	return out, nil
}

func reportMain(args []string) error {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	title := fs.String("title", "Benchmark", "report title")
	out := fs.String("out", "report.md", "output markdown")
	baseline := fs.String("baseline", "", "directory with previous results-*.json")
	intro := fs.String("intro", "", "markdown file prepended after the title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	results, err := loadResults(fs.Args())
	if err != nil {
		return err
	}
	base := map[string]*Result{}
	if *baseline != "" {
		paths, _ := filepath.Glob(filepath.Join(*baseline, "results-*.json"))
		prev, err := loadResults(paths)
		if err != nil {
			return err
		}
		for _, r := range prev {
			base[r.Backend] = r
		}
	}
	md := render(*title, *intro, results, base)
	return os.WriteFile(*out, []byte(md), 0o644)
}

func fmtNum(v float64) string {
	switch {
	case v == 0:
		return "0"
	case math.Abs(v) >= 100:
		return fmt.Sprintf("%.0f", v)
	case math.Abs(v) >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func cell(m *Metric) string {
	switch m.Status {
	case "na":
		return "—"
	case "fail":
		if m.N == 0 {
			return "❌"
		}
	}
	if m.Unit == "通过" {
		if m.P50 == 1 {
			return "✅"
		}
		return "❌"
	}
	s := fmtNum(m.P50)
	if m.N > 1 {
		s = fmt.Sprintf("%s / %s", fmtNum(m.P50), fmtNum(m.P99))
	}
	if m.Status == "fail" {
		s += " ❌"
	}
	return s
}

// delta formats the change of m against prev. cpuDiff says the two runs were
// on different CPU models: the change then says more about the runner than
// about oar, so it is a plain percentage with no verdict.
func delta(m, prev *Metric, cpuDiff bool) string {
	if prev == nil || prev.Status != "ok" || m.Status != "ok" || m.Unit == "通过" || prev.P50 == 0 {
		return ""
	}
	d := (m.P50 - prev.P50) / math.Abs(prev.P50) * 100
	if cpuDiff {
		return fmt.Sprintf(" (%s，CPU 不同)", pct(d))
	}
	if math.Abs(d) < 5 {
		return " (≈)"
	}
	better := d < 0
	if !m.LowerIsBetter {
		better = !better
	}
	mark := "🔺"
	if better {
		mark = "🟢"
	}
	return fmt.Sprintf(" (%s%+.0f%%)", mark, d)
}

// pct formats a percentage change with a sign and no decimals.
func pct(d float64) string {
	if r := math.Round(d); r != 0 {
		return fmt.Sprintf("%+.0f%%", r)
	}
	return "0%"
}

// cpuModel is the CPU model in a Result.Host, which hostDesc writes as
// "<goos>/<goarch> <hostname> · <cpu model> · <N> CPU". It is empty when the
// host reported none (no "model name" in /proc/cpuinfo, as on arm64 Linux).
func cpuModel(host string) string {
	parts := strings.Split(host, " · ")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(strings.Fields(strings.Join(parts[1:len(parts)-1], " · ")), " ")
}

// differentCPU reports whether two results were measured on different CPU
// models. A result without a model counts as unknown, not as different.
func differentCPU(cur, prev *Result) bool {
	if cur == nil || prev == nil {
		return false
	}
	a, b := cpuModel(cur.Host), cpuModel(prev.Host)
	return a != "" && b != "" && !strings.EqualFold(a, b)
}

func find(r *Result, group, key string) *Metric {
	if r == nil {
		return nil
	}
	for i := range r.Metrics {
		if r.Metrics[i].Group == group && r.Metrics[i].Key == key {
			return &r.Metrics[i]
		}
	}
	return nil
}

func render(title, intro string, results []*Result, base map[string]*Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", title)
	if intro != "" {
		if b, err := os.ReadFile(intro); err == nil {
			sb.Write(b)
			sb.WriteString("\n\n")
		}
	}
	sb.WriteString("## 测试环境\n\n| 后端 | 平台 | 宿主机 | commit | 档位 | 耗时 |\n|---|---|---|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %.0f s |\n", r.Backend, r.Platform, r.Host, r.Commit, r.Profile, r.Duration)
	}
	// backends whose baseline ran on another CPU model
	cpuDiff := map[string]bool{}
	var cpuNotes []string
	for _, r := range results {
		if differentCPU(r, base[r.Backend]) {
			cpuDiff[r.Backend] = true
			cpuNotes = append(cpuNotes, fmt.Sprintf("%s（基线 %s → 本次 %s）", r.Backend, cpuModel(base[r.Backend].Host), cpuModel(r.Host)))
		}
	}
	if len(cpuNotes) > 0 {
		fmt.Fprintf(&sb, "\n> 与基线的 CPU 型号不同：%s。这些后端的变化只写百分比并标注“CPU 不同”，不标 🟢/🔺/≈：换了宿主机，差异不能算作 oar 的改善或退化。\n", strings.Join(cpuNotes, "；"))
	}
	sb.WriteString("\n读表方法：单值指标直接给出数值；多次采样的指标写成 `p50 / p99`。括号里是相对上一步基线的变化：🟢 表示改善，🔺 表示变差，≈ 表示变化不到 5%。— 表示该后端不适用，❌ 表示失败。\n\n")

	// metric order and names come from the most complete result; metrics
	// only present elsewhere are appended to their group
	type key struct{ g, k string }
	primary := results[0]
	for _, r := range results {
		if len(r.Metrics) > len(primary.Metrics) {
			primary = r
		}
	}
	var order []key
	seen := map[key]bool{}
	names := map[key]*Metric{}
	addFrom := func(r *Result) {
		for i := range r.Metrics {
			m := &r.Metrics[i]
			k := key{m.Group, m.Key}
			if !seen[k] {
				seen[k] = true
				order = append(order, k)
			}
			if prev, ok := names[k]; !ok || (prev.Status != "ok" && m.Status == "ok") {
				names[k] = m
			}
		}
	}
	addFrom(primary)
	for _, r := range results {
		addFrom(r)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].g < order[j].g })
	group := ""
	for _, k := range order {
		if k.g != group {
			group = k.g
			fmt.Fprintf(&sb, "\n## %s\n\n| 指标 | 单位 |", groupTitles[group])
			for _, r := range results {
				fmt.Fprintf(&sb, " %s |", r.Backend)
			}
			sb.WriteString(" 说明 |\n|---|---|")
			for range results {
				sb.WriteString("---|")
			}
			sb.WriteString("---|\n")
		}
		meta := names[k]
		fmt.Fprintf(&sb, "| %s | %s |", meta.Name, meta.Unit)
		notes := map[string]string{}
		for _, r := range results {
			m := find(r, k.g, k.k)
			if m == nil {
				sb.WriteString(" — |")
				continue
			}
			fmt.Fprintf(&sb, " %s%s |", cell(m), delta(m, find(base[r.Backend], k.g, k.k), cpuDiff[r.Backend]))
			if m.Note != "" {
				notes[r.Backend] = m.Note
			}
		}
		fmt.Fprintf(&sb, " %s |\n", joinNotes(results, notes))
	}
	for _, r := range results {
		for _, n := range r.Notes {
			fmt.Fprintf(&sb, "\n> ⚠️ %s：%s\n", r.Backend, n)
		}
	}
	return sb.String()
}

// joinNotes prints a note once when every backend agrees, otherwise per
// backend.
func joinNotes(results []*Result, notes map[string]string) string {
	if len(notes) == 0 {
		return ""
	}
	uniq := map[string]bool{}
	for _, n := range notes {
		uniq[n] = true
	}
	if len(uniq) == 1 && len(notes) == len(results) {
		for n := range uniq {
			return n
		}
	}
	var parts []string
	for _, r := range results {
		if n, ok := notes[r.Backend]; ok {
			parts = append(parts, r.Backend+"："+n)
		}
	}
	return strings.Join(dedupe(parts), "；")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
