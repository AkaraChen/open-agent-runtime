package main

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---- synthetic results ----

// metric is one measured value (a single sample, so p50 is the value).
func metric(id, unit string, lower bool, v float64) Metric {
	group, key, _ := strings.Cut(id, "/")
	m := Metric{Group: group, Key: key, Name: key, Unit: unit, Samples: []float64{v}, Status: "ok", LowerIsBetter: lower}
	summarize(&m)
	return m
}

func tm(id string, v float64) Metric  { return metric(id, "ms", true, v) }
func sec(id string, v float64) Metric { return metric(id, "s", true, v) }

// result is one side of a round; the host CPU calibrator comes first.
func result(commit string, cpu float64, metrics ...Metric) *Result {
	r := &Result{Backend: "kvm", Platform: "linux/amd64", Host: "linux/amd64 runner · cpu · 4 CPU", Commit: commit, Profile: "quick"}
	r.Metrics = append([]Metric{tm("0-env/cpu", cpu)}, metrics...)
	return r
}

// rd is one round of 4-start/e2e.
type rd struct {
	base, head float64
	cpuHead    float64 // host calibrator of the head, 0: the same as the base's (40 ms)
}

func e2eRounds(rs ...rd) []*roundPair {
	var pairs []*roundPair
	for i, r := range rs {
		cpu := r.cpuHead
		if cpu == 0 {
			cpu = 40
		}
		pairs = append(pairs, newPair(i+1, result("aaaaaaa", 40, tm("4-start/e2e", r.base)), result("bbbbbbb", cpu, tm("4-start/e2e", r.head))))
	}
	return pairs
}

func mustGate(t *testing.T, spec string) []gateRule {
	t.Helper()
	rules, err := parseGate(spec, 10)
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

func compareDefault(t *testing.T, pairs []*roundPair) *comparison {
	t.Helper()
	return analyze("kvm", pairs, mustGate(t, defaultGate), 3)
}

func metricOf(t *testing.T, c *comparison, id string) *metricCmp {
	t.Helper()
	for _, m := range c.metrics {
		if m.id.String() == id {
			return m
		}
	}
	t.Fatalf("no metric %s", id)
	return nil
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ---- the gate ----

func TestRegressionInEveryRoundFails(t *testing.T) {
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 48}, rd{base: 41, head: 50}, rd{base: 39, head: 47}))
	m := metricOf(t, c, "4-start/e2e")
	if m.failed != failRegression || m.verdict != vFail {
		t.Fatalf("failed=%v verdict=%s, want a failed regression", m.failed, m.verdict)
	}
	if !near(m.baseMed, 40) || !near(m.headMed, 48) || !near(m.pct, 20) {
		t.Fatalf("headline %v -> %v (%v%%), want the medians 40 -> 48 (+20%%)", m.baseMed, m.headMed, m.pct)
	}
	if c.status() != "FAIL" || len(c.failures()) != 1 {
		t.Fatalf("status %s with %d failures", c.status(), len(c.failures()))
	}
	for _, want := range []string{"**FAIL**", "Failed:", "`4-start/e2e`: 40.0 → 48.0 ms, +20.0%", "limit 10%"} {
		if md := c.markdown(); !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestRegressionInOneRoundDoesNotFail(t *testing.T) {
	// three rounds, one of them slow: the headline is the median, no change
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 48}, rd{base: 40, head: 40.5}, rd{base: 40, head: 41}))
	m := metricOf(t, c, "4-start/e2e")
	if m.failed != notFailed || m.verdict != vSame {
		t.Errorf("failed=%v verdict=%s, want no failure and same", m.failed, m.verdict)
	}
	// two rounds: the headline (mean of two) is worse by 12.5%, but round 2
	// is only 5% worse, so the gate holds
	c = compareDefault(t, e2eRounds(rd{base: 40, head: 48}, rd{base: 40, head: 42}))
	m = metricOf(t, c, "4-start/e2e")
	if m.failed != notFailed || m.verdict != vWorse || !near(m.pct, 12.5) {
		t.Errorf("failed=%v verdict=%s pct=%v, want worse by 12.5%% without a failure", m.failed, m.verdict, m.pct)
	}
	if c.status() != "PASS" {
		t.Errorf("status %s, want PASS", c.status())
	}
}

func TestImprovement(t *testing.T) {
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 30}, rd{base: 40, head: 32}))
	m := metricOf(t, c, "4-start/e2e")
	if m.verdict != vBetter || m.failed != notFailed || !near(m.pct, -22.5) {
		t.Errorf("verdict=%s failed=%v pct=%v, want better by 22.5%%", m.verdict, m.failed, m.pct)
	}
	if c.status() != "PASS" || !strings.Contains(c.markdown(), "-22.5%") {
		t.Errorf("status %s:\n%s", c.status(), c.markdown())
	}
}

func TestHigherIsBetter(t *testing.T) {
	run := func(base, head float64) *metricCmp {
		pairs := []*roundPair{
			newPair(1, result("a", 40, metric("2-warm/cold.mbps", "MiB/s", false, base)), result("b", 40, metric("2-warm/cold.mbps", "MiB/s", false, head))),
			newPair(2, result("a", 40, metric("2-warm/cold.mbps", "MiB/s", false, base)), result("b", 40, metric("2-warm/cold.mbps", "MiB/s", false, head))),
		}
		return analyze("kvm", pairs, mustGate(t, "2-warm/cold.mbps"), 3).metrics[1]
	}
	// 400 -> 500 MiB/s is faster
	if m := run(400, 500); m.verdict != vBetter || m.failed != notFailed {
		t.Errorf("faster: verdict=%s failed=%v", m.verdict, m.failed)
	}
	// 400 -> 300 MiB/s is 25% worse; throughput has no absolute minimum
	if m := run(400, 300); m.verdict != vFail || m.failed != failRegression {
		t.Errorf("slower: verdict=%s failed=%v", m.verdict, m.failed)
	}
}

func TestMissingAndNotOKMetricsAreNA(t *testing.T) {
	na := Metric{Group: "4-start", Key: "ready", Name: "ready", Status: "na"}
	base := result("a", 40, tm("4-start/e2e", 40), tm("4-start/create", 20), tm("4-start/ready", 30))
	head := result("b", 40, tm("4-start/e2e", 41), na, tm("5-switch/overlap.wall", 80))
	c := compareDefault(t, []*roundPair{newPair(1, base, head), newPair(2, base, head)})
	for id, want := range map[string]string{
		"4-start/e2e":           vSame,
		"4-start/create":        vNA, // missing in the head
		"4-start/ready":         vNA, // not available in the head
		"5-switch/overlap.wall": vNA, // missing in the base
		"0-env/cpu":             vSame,
	} {
		if m := metricOf(t, c, id); m.verdict != want {
			t.Errorf("%s: verdict %s, want %s", id, m.verdict, want)
		}
	}
	if len(c.failures()) != 0 || c.status() != "PASS" {
		t.Errorf("missing metrics must not fail the gate: %s, %d failures", c.status(), len(c.failures()))
	}
	md := c.markdown()
	// the gated ones that one side lacks are called out; patterns without any metric too
	for _, want := range []string{"gated metrics measured by one side only: `4-start/create` (head: rounds 1, 2), `5-switch/overlap.wall` (base: rounds 1, 2)", "No metric matched: `3-vm/boot.cold`"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestHeadFailureFails(t *testing.T) {
	check := func(status string) Metric {
		m := metric("8-correctness/determinism", "通过", false, 1)
		if status == "fail" {
			m = metric("8-correctness/determinism", "通过", false, 0)
			m.Status = "fail"
		}
		return m
	}
	round := func(n int, base, head string) *roundPair {
		return newPair(n, result("a", 40, check(base)), result("b", 40, check(head)))
	}
	// the check is not gated and not worse on the numbers of round 2: one
	// failed round is enough
	c := compareDefault(t, []*roundPair{round(1, "ok", "fail"), round(2, "ok", "ok")})
	m := metricOf(t, c, "8-correctness/determinism")
	if m.failed != failStatus || len(m.failedIn) != 1 || m.failedIn[0] != 1 || m.verdict != vFail {
		t.Fatalf("failed=%v in %v verdict=%s", m.failed, m.failedIn, m.verdict)
	}
	if c.status() != "FAIL" || !strings.Contains(c.markdown(), "failed in the head but not in the base (round 1)") {
		t.Errorf("status %s:\n%s", c.status(), c.markdown())
	}
	if got := c.perRound(m); got != "fail / 0.0" {
		t.Errorf("per round %q", got)
	}
	// failing on both sides, or only in the base, is not a regression
	for _, rs := range [][2]string{{"fail", "fail"}, {"fail", "ok"}} {
		c := compareDefault(t, []*roundPair{round(1, rs[0], rs[1]), round(2, rs[0], rs[1])})
		if m := metricOf(t, c, "8-correctness/determinism"); m.failed != notFailed || m.verdict != vNA {
			t.Errorf("base %s, head %s: failed=%v verdict=%s", rs[0], rs[1], m.failed, m.verdict)
		}
	}
}

func TestNoisyRoundIsNotCounted(t *testing.T) {
	// the calibrator of round 2 differs by 15%: the regression has to hold
	// in rounds 1 and 3, and round 2 does not matter although it is fine
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 48}, rd{base: 40, head: 41, cpuHead: 46}, rd{base: 40, head: 47}))
	if c.pairs[0].cal.noisy || !c.pairs[1].cal.noisy || c.pairs[2].cal.noisy || c.noisyRounds != 1 || c.inconclusive {
		t.Fatalf("noisy rounds: %v %v %v (%d), inconclusive %v", c.pairs[0].cal.noisy, c.pairs[1].cal.noisy, c.pairs[2].cal.noisy, c.noisyRounds, c.inconclusive)
	}
	m := metricOf(t, c, "4-start/e2e")
	if m.failed != failRegression {
		t.Errorf("failed=%v, want the regression of the two counted rounds", m.failed)
	}
	if got := c.perRound(m); got != "+20.0 / (+2.5) / +17.5" {
		t.Errorf("per round %q", got)
	}
	if !near(m.baseMed, 40) || !near(m.headMed, 47.5) {
		t.Errorf("headline %v -> %v, want the counted rounds only: 40 -> 47.5", m.baseMed, m.headMed)
	}
	md := c.markdown()
	for _, want := range []string{"| 2 | 40.0 → 46.0 ms | +15.0% | noisy |", "| 1 | 40.0 → 40.0 ms | 0.0% | ok |"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	// the calibrator itself is judged on all rounds: none of them is noisy
	// for it
	if got := c.perRound(metricOf(t, c, "0-env/cpu")); got != "0.0 / +15.0 / 0.0" {
		t.Errorf("calibrator per round %q", got)
	}
}

func TestNoisyRoundDoesNotMoveTheHeadline(t *testing.T) {
	// the one round that regressed is the noisy one: counted, the mean of
	// the two rounds would be 15% worse
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 40.5}, rd{base: 40, head: 52, cpuHead: 46}))
	m := metricOf(t, c, "4-start/e2e")
	if m.failed != notFailed || m.verdict != vSame || !near(m.headMed, 40.5) {
		t.Errorf("failed=%v verdict=%s head=%v", m.failed, m.verdict, m.headMed)
	}
	if c.status() != "PASS" {
		t.Errorf("status %s", c.status())
	}
	// a regression that holds in every counted round fails although a noisy
	// round says otherwise
	c = compareDefault(t, e2eRounds(rd{base: 40, head: 48}, rd{base: 40, head: 36, cpuHead: 46}, rd{base: 40, head: 48}))
	if m := metricOf(t, c, "4-start/e2e"); m.failed != failRegression {
		t.Errorf("failed=%v", m.failed)
	}
}

func TestMostlyNoisyIsInconclusive(t *testing.T) {
	// two of three rounds are noisy: the third regresses, but that is not
	// enough to call it
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 48, cpuHead: 46}, rd{base: 40, head: 48, cpuHead: 34}, rd{base: 40, head: 48}))
	if !c.inconclusive || c.status() != "INCONCLUSIVE" || len(c.failures()) != 0 {
		t.Errorf("inconclusive=%v status=%s failures=%d", c.inconclusive, c.status(), len(c.failures()))
	}
	if md := c.markdown(); !strings.Contains(md, "Inconclusive: 2 of 3 rounds noisy") {
		t.Errorf("markdown:\n%s", md)
	}
	// exactly half is still a verdict
	c = compareDefault(t, e2eRounds(rd{base: 40, head: 48, cpuHead: 46}, rd{base: 40, head: 48}))
	if c.inconclusive || c.status() != "FAIL" {
		t.Errorf("one of two noisy: inconclusive=%v status=%s", c.inconclusive, c.status())
	}
	// a failed check is not performance: it still fails an inconclusive comparison
	bad := metric("8-correctness/determinism", "通过", false, 0)
	bad.Status = "fail"
	c = compareDefault(t, []*roundPair{newPair(1, result("a", 40, metric("8-correctness/determinism", "通过", false, 1)), result("b", 46, bad))})
	if !c.inconclusive || len(c.failures()) != 1 || c.status() != "FAIL" {
		t.Errorf("inconclusive=%v failures=%d status=%s", c.inconclusive, len(c.failures()), c.status())
	}
}

func TestOnlyNoisyRoundsGiveVerdictNoisy(t *testing.T) {
	c := compareDefault(t, e2eRounds(rd{base: 40, head: 48, cpuHead: 46}))
	m := metricOf(t, c, "4-start/e2e")
	if m.verdict != vNoisy || !m.have || !near(m.pct, 20) || m.failed != notFailed {
		t.Errorf("verdict=%s have=%v pct=%v failed=%v", m.verdict, m.have, m.pct, m.failed)
	}
	if got := c.perRound(m); got != "(+20.0)" {
		t.Errorf("per round %q", got)
	}
}

func TestAbsoluteMinimum(t *testing.T) {
	// +30% in both rounds, but only 1.5 ms
	c := compareDefault(t, e2eRounds(rd{base: 5, head: 6.5}, rd{base: 5, head: 6.5}))
	if m := metricOf(t, c, "4-start/e2e"); m.failed != notFailed || m.verdict != vWorse {
		t.Errorf("1.5 ms: failed=%v verdict=%s", m.failed, m.verdict)
	}
	// +80%, 4 ms
	c = compareDefault(t, e2eRounds(rd{base: 5, head: 9}, rd{base: 5, head: 9}))
	if m := metricOf(t, c, "4-start/e2e"); m.failed != failRegression {
		t.Errorf("4 ms: failed=%v", m.failed)
	}
	// the same minimum in seconds: 2.5 ms is not enough, 4 ms is
	secs := func(base, head float64) *metricCmp {
		var pairs []*roundPair
		for n := 1; n <= 2; n++ {
			pairs = append(pairs, newPair(n, result("a", 40, sec("x/y", base)), result("b", 40, sec("x/y", head))))
		}
		return analyze("kvm", pairs, mustGate(t, "x/y"), 3).metrics[1]
	}
	if m := secs(0.010, 0.0125); m.failed != notFailed {
		t.Errorf("2.5 ms in s: failed=%v", m.failed)
	}
	if m := secs(0.010, 0.0140); m.failed != failRegression {
		t.Errorf("4 ms in s: failed=%v", m.failed)
	}
}

func TestPerPatternThreshold(t *testing.T) {
	cold := func(base, head float64) *comparison {
		var pairs []*roundPair
		for n := 1; n <= 2; n++ {
			pairs = append(pairs, newPair(n, result("a", 40, sec("2-warm/cold", base)), result("b", 40, sec("2-warm/cold", head))))
		}
		return compareDefault(t, pairs)
	}
	// cold warm may be 20% worse, other gated metrics 10%
	if m := metricOf(t, cold(1.5, 1.725), "2-warm/cold"); m.failed != notFailed || m.verdict != vWorse || m.threshold != 20 {
		t.Errorf("+15%%: failed=%v verdict=%s threshold=%v", m.failed, m.verdict, m.threshold)
	}
	if m := metricOf(t, cold(1.5, 1.875), "2-warm/cold"); m.failed != failRegression {
		t.Errorf("+25%%: failed=%v", m.failed)
	}
	if md := cold(1.5, 1.875).markdown(); !strings.Contains(md, "Gate: worse than 10% (20% for `2-warm/cold`) and by at least 3 ms in every round.") {
		t.Errorf("markdown:\n%s", md)
	}
}

func TestParseGate(t *testing.T) {
	rules := mustGate(t, defaultGate)
	if len(rules) != 6 {
		t.Fatalf("%d rules in the default gate: %v", len(rules), rules)
	}
	for id, want := range map[string]float64{ // -1: not gated
		"4-start/e2e":            10,
		"4-start/create":         10,
		"5-switch/overlap.wall":  10,
		"5-switch/ingress.wall":  10,
		"5-switch/slowstop.wall": 10,
		"3-vm/boot.cold":         10,
		"9-snapshot/restore":     10,
		"2-warm/cold":            20,
		"4-start/compose":        -1,
		"4-start/direct.run":     -1,
		"5-switch/overlap.guest": -1,
		"3-vm/boot.warm":         -1,
		"9-snapshot/cold":        -1,
		"2-warm/cold.mbps":       -1,
		"2-warm/incr":            -1,
		"6-runtime/e2e":          -1,
	} {
		group, key, _ := strings.Cut(id, "/")
		r, ok := gateFor(rules, metricID{group, key})
		switch {
		case want < 0 && ok:
			t.Errorf("%s is gated by %q", id, r.pattern)
		case want >= 0 && (!ok || r.threshold != want):
			t.Errorf("%s: gated=%v threshold=%v, want %v", id, ok, r.threshold, want)
		}
	}

	rules, err := parseGate(" a/b , c/d=5% ,, e/*=0 ", 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []gateRule{{"a/b", 7}, {"c/d", 5}, {"e/*", 0}}
	if len(rules) != len(want) {
		t.Fatalf("%v", rules)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rule %d: %v, want %v", i, rules[i], want[i])
		}
	}
	if rules, err := parseGate("", 10); err != nil || len(rules) != 0 {
		t.Errorf("empty gate: %v, %v", rules, err)
	}
	for _, bad := range []string{"a/b=x", "a/b=-1", "a/b=", "a/[=1", "["} {
		if _, err := parseGate(bad, 10); err == nil {
			t.Errorf("gate %q was accepted", bad)
		}
	}
}

// ---- files ----

// writeRound writes the files of one round of one label.
func writeRound(t *testing.T, dir, label string, n int, base, head float64) {
	t.Helper()
	for side, v := range map[string]float64{"base": base, "head": head} {
		r := result(side+strconv.Itoa(n), 40, tm("4-start/e2e", v))
		if err := writeJSON(filepath.Join(dir, "results-"+label+"-"+side+"-"+strconv.Itoa(n)+".json"), r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadDirPairsRoundsByNumber(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{1, 2, 10} { // 10 sorts before 2 as text
		writeRound(t, dir, "kvm", n, 40, 40)
	}
	writeRound(t, dir, "native", 1, 40, 40)
	writeRound(t, dir, "vz-intel", 1, 40, 40)
	// a round of one side only, logs of failed phases, other files
	if err := writeJSON(filepath.Join(dir, "results-kvm-base-3.json"), result("lonely", 40)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"results-kvm-base-1.switch.console.log", "results-kvm-head-x.json", "resources.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pairs, notes, err := loadDir(dir, "kvm")
	if err != nil {
		t.Fatal(err)
	}
	var rounds []int
	for _, p := range pairs {
		rounds = append(rounds, p.n)
		if want := strconv.Itoa(p.n); p.base.Commit != "base"+want || p.head.Commit != "head"+want {
			t.Errorf("round %d pairs %s with %s", p.n, p.base.Commit, p.head.Commit)
		}
	}
	if len(rounds) != 3 || rounds[0] != 1 || rounds[1] != 2 || rounds[2] != 10 {
		t.Errorf("rounds %v, want [1 2 10]", rounds)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "round 3 has no head result") {
		t.Errorf("notes %q", notes)
	}
	if _, _, err := loadDir(dir, "qemu-arm64"); err == nil {
		t.Error("a label without results was accepted")
	}
	if _, _, err := loadDir(filepath.Join(dir, "missing"), "kvm"); err == nil {
		t.Error("a missing directory was accepted")
	}
}

func TestCompareCmd(t *testing.T) {
	dir := t.TempDir()
	writeRound(t, dir, "kvm", 1, 40, 48)
	writeRound(t, dir, "kvm", 2, 41, 50)

	var out bytes.Buffer
	err := compareCmd(&out, []string{"-dir", dir, "-label", "kvm"})
	if err == nil || !strings.Contains(err.Error(), "1 metric failed") {
		t.Fatalf("a consistent regression returned %v", err)
	}
	for _, want := range []string{"## bench A/B: kvm", "**FAIL** · 2 rounds · base `base1`, `base2` · head `head1`, `head2`", "### Gated metrics", "<details><summary>All metrics (2)</summary>", "| `4-start/e2e` |"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	// the same results with a looser limit, in percent with or without the sign
	for _, limit := range []string{"25", "25%"} {
		out.Reset()
		if err := compareCmd(&out, []string{"-dir", dir, "-label", "kvm", "-fail-on", limit}); err != nil {
			t.Errorf("-fail-on %s: %v", limit, err)
		}
	}
	// without gates, and with a gate on something else
	for _, gate := range []string{"", "6-runtime/*"} {
		out.Reset()
		if err := compareCmd(&out, []string{"-dir", dir, "-label", "kvm", "-gate", gate}); err != nil {
			t.Errorf("-gate %q: %v", gate, err)
		}
	}
	// the minimum change in ms
	out.Reset()
	if err := compareCmd(&out, []string{"-dir", dir, "-label", "kvm", "-min-ms", "20"}); err != nil {
		t.Errorf("-min-ms 20: %v", err)
	}

	// explicit files, round by position
	out.Reset()
	f := func(side string, n int) string {
		return filepath.Join(dir, "results-kvm-"+side+"-"+strconv.Itoa(n)+".json")
	}
	if err := compareCmd(&out, []string{"-base", f("base", 1) + "," + f("base", 2), "-head", f("head", 1) + "," + f("head", 2)}); err == nil {
		t.Error("explicit files: the regression passed")
	}
	if !strings.Contains(out.String(), "## bench A/B: kvm") { // the label comes from the results
		t.Errorf("output:\n%s", out.String())
	}
	// identical results are all the same
	out.Reset()
	if err := compareCmd(&out, []string{"-base", f("base", 1), "-head", f("base", 1)}); err != nil {
		t.Errorf("identical results: %v", err)
	}
	if md := out.String(); !strings.Contains(md, "**PASS**") || strings.Contains(md, "| worse |") || strings.Contains(md, "| better |") {
		t.Errorf("identical results:\n%s", md)
	}

	for _, args := range [][]string{
		nil,
		{"-dir", dir},
		{"-dir", dir, "-label", "kvm", "-base", f("base", 1)},
		{"-base", f("base", 1)},
		{"-base", f("base", 1) + "," + f("base", 2), "-head", f("head", 1)},
		{"-dir", dir, "-label", "kvm", "-fail-on", "lots"},
		{"-dir", dir, "-label", "kvm", "-gate", "a/b=lots"},
		{"-dir", dir, "-label", "kvm", "extra"},
		{"-dir", filepath.Join(dir, "missing"), "-label", "kvm"},
	} {
		if err := compareCmd(&bytes.Buffer{}, args); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}

// ---- helpers ----

func TestMedianOf(t *testing.T) {
	for _, tc := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{3, 1, 2}, 2}, {[]float64{1, 2}, 1.5}, {[]float64{4, 1, 3, 2}, 2.5}} {
		if got := medianOf(tc.in); got != tc.want {
			t.Errorf("medianOf(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestPctChange(t *testing.T) {
	for _, tc := range []struct {
		base, head float64
		want       float64
		text       string
	}{{100, 110, 10, "+10.0"}, {100, 50, -50, "-50.0"}, {50, 150, 200, "+200"}, {0, 0, 0, "0.0"}, {40, 41, 2.5, "+2.5"}, {40, 40.01, 0.025, "0.0"}, {0, 5, math.Inf(1), "+inf"}, {0, -5, math.Inf(-1), "-inf"}} {
		got := pctChange(tc.base, tc.head)
		if got != tc.want && !near(got, tc.want) {
			t.Errorf("pctChange(%v, %v) = %v, want %v", tc.base, tc.head, got, tc.want)
		}
		if text := fmtPct(got); text != tc.text {
			t.Errorf("fmtPct(%v) = %q, want %q", got, text, tc.text)
		}
		wantCell := tc.text + "%"
		if math.IsInf(got, 0) {
			wantCell = tc.text
		}
		if cell := pctCell(got); cell != wantCell {
			t.Errorf("pctCell(%v) = %q, want %q", got, cell, wantCell)
		}
	}
}

func TestPipesInNamesDoNotBreakTables(t *testing.T) {
	m := tm("4-start/e2e", 40)
	m.Name = "a | b"
	base := result("a", 40, m)
	c := compareDefault(t, []*roundPair{newPair(1, base, base)})
	if md := c.markdown(); !strings.Contains(md, `| a \| b |`) {
		t.Errorf("markdown:\n%s", md)
	}
}
