package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// compare judges a change against its base from benchmark results of both
// builds that were run alternately on one runner (the bench-ab job of the
// bench workflow writes results-<label>-{base,head}-<round>.json):
//
//	go run ./hack/bench compare -dir results -label kvm
//	go run ./hack/bench compare -base b1.json,b2.json -head h1.json,h2.json
//
// Round i of the base is paired with round i of the head. A metric is
// reduced to its p50 per round; the headline numbers are the medians of
// those p50s across the rounds. A gated metric fails when the head is worse
// than the threshold in every round, so one slow run cannot fail it. The
// exit code is 1 if a gated metric failed, or if a metric the base passed
// failed in the head; it is 0 otherwise, also for an inconclusive comparison.

const (
	// defaultGate lists the metrics a change is held to: the start, switch
	// and boot paths and the snapshot restore. Patterns are path.Match
	// patterns on group/key; =percent replaces -fail-on for one pattern.
	defaultGate = "4-start/e2e*,4-start/create*,5-switch/*wall*,3-vm/boot.cold,9-snapshot/restore*,2-warm/cold=20"

	// samePct: a change below this many percent is no change.
	samePct = 5.0
	// noisePct: a round is noisy when the host CPU calibrator differs by
	// more than this many percent between the sides.
	noisePct = 5.0
)

// calibrator is the metric that tells how fast the runner was during a run
// (single-core sha256, see phaseEnv). Equal values on both sides of a round
// mean that the sides saw the same machine.
var calibrator = metricID{"0-env", "cpu"}

// Verdicts of a metric.
const (
	vBetter = "better"
	vWorse  = "worse"
	vSame   = "same"
	vNA     = "n/a"   // missing, or not ok, on either side in every round
	vNoisy  = "noisy" // only noisy rounds could be compared
	vFail   = "FAIL"  // see failKind
)

// metricID names a metric the way the gate patterns do: group/key.
type metricID struct{ group, key string }

func (id metricID) String() string { return id.group + "/" + id.key }

type resultIndex map[metricID]*Metric

func indexResult(r *Result) resultIndex {
	idx := make(resultIndex, len(r.Metrics))
	for i := range r.Metrics {
		id := metricID{r.Metrics[i].Group, r.Metrics[i].Key}
		if _, dup := idx[id]; !dup {
			idx[id] = &r.Metrics[i]
		}
	}
	return idx
}

// measured reports whether m carries a value to compare.
func measured(m *Metric) bool { return m != nil && m.Status == "ok" && m.N > 0 }

// roundPair is round n of both sides.
type roundPair struct {
	n          int
	base, head *Result
	bi, hi     resultIndex
	cal        calibration
}

// calibration compares the host CPU calibrator of the two sides of a round.
type calibration struct {
	base, head float64
	unit       string
	known      bool    // both sides measured it
	drift      float64 // head relative to base, in percent
	noisy      bool
}

func newPair(n int, base, head *Result) *roundPair {
	p := &roundPair{n: n, base: base, head: head, bi: indexResult(base), hi: indexResult(head)}
	if b, h := p.bi[calibrator], p.hi[calibrator]; measured(b) && measured(h) {
		p.cal = calibration{base: b.P50, head: h.P50, unit: b.Unit, known: true, drift: pctChange(b.P50, h.P50)}
		p.cal.noisy = math.Abs(p.cal.drift) > noisePct
	}
	return p
}

func (p *roundPair) state() string {
	switch {
	case !p.cal.known:
		return "no calibrator"
	case p.cal.noisy:
		return "noisy"
	}
	return "ok"
}

// gateRule selects gated metrics by pattern and says how much worse the
// head may be.
type gateRule struct {
	pattern   string
	threshold float64 // percent
}

// parseGate parses "pattern[=percent],..."; a pattern without a percentage
// gets the -fail-on value.
func parseGate(spec string, failOn float64) ([]gateRule, error) {
	var rules []gateRule
	for _, f := range splitList(spec) {
		r := gateRule{pattern: f, threshold: failOn}
		if i := strings.LastIndex(f, "="); i >= 0 {
			v, err := parsePercent(f[i+1:])
			if err != nil {
				return nil, fmt.Errorf("compare: gate %q: %w", f, err)
			}
			r.pattern, r.threshold = strings.TrimSpace(f[:i]), v
		}
		if _, err := path.Match(r.pattern, "group/key"); err != nil {
			return nil, fmt.Errorf("compare: gate %q: %w", f, err)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// gateFor returns the first rule matching id.
func gateFor(rules []gateRule, id metricID) (gateRule, bool) {
	for _, r := range rules {
		if ok, _ := path.Match(r.pattern, id.String()); ok {
			return r, true
		}
	}
	return gateRule{}, false
}

func parsePercent(s string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("invalid percentage %q", s)
	}
	return v, nil
}

// percentFlag is a percentage written with or without the % sign.
type percentFlag float64

func (p *percentFlag) String() string { return strconv.FormatFloat(float64(*p), 'g', -1, 64) }

func (p *percentFlag) Set(s string) error {
	v, err := parsePercent(s)
	if err != nil {
		return err
	}
	*p = percentFlag(v)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func compareMain(args []string) error { return compareCmd(os.Stdout, args) }

func compareCmd(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory with the A/B job's results-<label>-{base,head}-<round>.json")
	label := fs.String("label", "", "with -dir: the backend label of the files (native, kvm, vz-intel, qemu-arm64)")
	baseFiles := fs.String("base", "", "instead of -dir: comma separated base results, one per round")
	headFiles := fs.String("head", "", "instead of -dir: comma separated head results, in the same round order")
	failOn := percentFlag(10)
	fs.Var(&failOn, "fail-on", "a gated metric fails when it is worse than this many percent in every round")
	minMS := fs.Float64("min-ms", 3, "... and worse by at least this many ms (metrics in s: the same time; other units: no minimum)")
	gate := fs.String("gate", defaultGate, "comma separated group/key patterns of the gated metrics (path.Match; * stops at /), each with an optional =percent replacing -fail-on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("compare: unexpected argument %q", fs.Arg(0))
	}
	rules, err := parseGate(*gate, float64(failOn))
	if err != nil {
		return err
	}
	var pairs []*roundPair
	var notes []string
	switch {
	case *dir != "" && (*baseFiles != "" || *headFiles != ""):
		return errors.New("compare: use -dir with -label, or -base with -head, not both")
	case *dir != "":
		if *label == "" {
			return errors.New("compare: -dir needs -label")
		}
		pairs, notes, err = loadDir(*dir, *label)
	case *baseFiles != "" && *headFiles != "":
		pairs, err = loadFiles(splitList(*baseFiles), splitList(*headFiles))
	default:
		return errors.New("compare: need -dir with -label, or -base with -head")
	}
	if err != nil {
		return err
	}
	name := *label
	if name == "" {
		name = pairs[0].base.Backend
	}
	c := analyze(name, pairs, rules, *minMS)
	c.warnings = append(notes, c.warnings...)
	if _, err := io.WriteString(w, c.markdown()); err != nil {
		return err
	}
	if n := len(c.failures()); n > 0 {
		return fmt.Errorf("compare: %d %s failed", n, plural(n, "metric"))
	}
	return nil
}

// loadDir pairs the files of one label in dir by the round number in their
// names. Rounds that only one side has are skipped, and reported.
func loadDir(dir, label string) ([]*roundPair, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	files := map[string]map[int]string{"base": {}, "head": {}}
	for _, e := range entries {
		for side, rounds := range files {
			prefix := "results-" + label + "-" + side + "-"
			name := e.Name()
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".json")); err == nil && n > 0 {
				rounds[n] = filepath.Join(dir, name)
			}
		}
	}
	var rounds []int
	var notes []string
	for _, n := range sortedRounds(files["base"], files["head"]) {
		b, h := files["base"][n], files["head"][n]
		switch {
		case b != "" && h != "":
			rounds = append(rounds, n)
		case b != "":
			notes = append(notes, fmt.Sprintf("round %d has no head result and is skipped", n))
		default:
			notes = append(notes, fmt.Sprintf("round %d has no base result and is skipped", n))
		}
	}
	if len(rounds) == 0 {
		return nil, nil, fmt.Errorf("compare: no round of %q has both a base and a head result in %s (%d base, %d head files)",
			label, dir, len(files["base"]), len(files["head"]))
	}
	var bp, hp []string
	for _, n := range rounds {
		bp, hp = append(bp, files["base"][n]), append(hp, files["head"][n])
	}
	pairs, err := loadPairs(rounds, bp, hp)
	return pairs, notes, err
}

// sortedRounds returns the round numbers of both maps, ascending.
func sortedRounds(a, b map[int]string) []int {
	seen := map[int]bool{}
	var out []int
	for _, m := range []map[int]string{a, b} {
		for n := range m {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Ints(out)
	return out
}

// loadFiles pairs explicitly listed files by position.
func loadFiles(basePaths, headPaths []string) ([]*roundPair, error) {
	if len(basePaths) != len(headPaths) {
		return nil, fmt.Errorf("compare: %d base files but %d head files; the i-th of each is round i", len(basePaths), len(headPaths))
	}
	rounds := make([]int, len(basePaths))
	for i := range rounds {
		rounds[i] = i + 1
	}
	return loadPairs(rounds, basePaths, headPaths)
}

func loadPairs(rounds []int, basePaths, headPaths []string) ([]*roundPair, error) {
	base, err := loadResults(basePaths)
	if err != nil {
		return nil, err
	}
	head, err := loadResults(headPaths)
	if err != nil {
		return nil, err
	}
	pairs := make([]*roundPair, len(rounds))
	for i, n := range rounds {
		pairs[i] = newPair(n, base[i], head[i])
	}
	return pairs, nil
}

// roundDelta is one round of one metric.
type roundDelta struct {
	n          int
	baseOK     bool    // the base measured it
	headOK     bool    // the head measured it
	headFailed bool    // the head failed a check that the base passed
	noisy      bool    // the round's calibrator drifted: the round does not count
	base, head float64 // p50 of the round, if both sides measured it
	pct        float64 // head relative to base, in percent
}

func (r roundDelta) valid() bool { return r.baseOK && r.headOK }

// failKind is why a metric failed the comparison.
type failKind int

const (
	notFailed      failKind = iota
	failStatus              // the head failed a check that the base passed
	failRegression          // the head is worse than the gate allows
)

// metricCmp is the comparison of one metric over all rounds.
type metricCmp struct {
	id      metricID
	name    string
	unit    string
	lower   bool // lower values are better
	rounds  []roundDelta
	have    bool // baseMed, headMed and pct are set
	baseMed float64
	headMed float64
	pct     float64
	verdict string

	gated     bool
	threshold float64 // percent
	failed    failKind
	failedIn  []int // failStatus: the rounds
}

// worsePct and worseAbs say how much worse than the base the head is, in
// percent and in the metric's unit; negative values are improvements.
func (m *metricCmp) worsePct(pct float64) float64 {
	if m.lower {
		return pct
	}
	return -pct
}

func (m *metricCmp) worseAbs(base, head float64) float64 {
	if m.lower {
		return head - base
	}
	return base - head
}

// comparison is the outcome of compare.
type comparison struct {
	label        string
	pairs        []*roundPair
	rules        []gateRule
	minMS        float64
	metrics      []*metricCmp
	noisyRounds  int
	inconclusive bool     // too many noisy rounds to judge performance
	unmatched    []string // gate patterns without a metric
	warnings     []string
}

func analyze(label string, pairs []*roundPair, rules []gateRule, minMS float64) *comparison {
	c := &comparison{label: label, pairs: pairs, rules: rules, minMS: minMS}
	for _, p := range pairs {
		if p.cal.noisy {
			c.noisyRounds++
		}
	}
	c.inconclusive = 2*c.noisyRounds > len(pairs)

	var ids []metricID
	seen := map[metricID]bool{}
	for _, p := range pairs {
		for _, r := range []*Result{p.base, p.head} {
			for i := range r.Metrics {
				id := metricID{r.Metrics[i].Group, r.Metrics[i].Key}
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	sort.SliceStable(ids, func(i, j int) bool { return ids[i].group < ids[j].group })
	matched := map[string]bool{}
	for _, id := range ids {
		c.metrics = append(c.metrics, c.compareMetric(id))
		if r, ok := gateFor(rules, id); ok {
			matched[r.pattern] = true
		}
	}
	for _, r := range rules {
		if !matched[r.pattern] {
			c.unmatched = append(c.unmatched, r.pattern)
		}
	}
	c.describe()
	return c
}

func (c *comparison) compareMetric(id metricID) *metricCmp {
	m := &metricCmp{id: id}
	for _, p := range c.pairs {
		b, h := p.bi[id], p.hi[id]
		if m.name == "" {
			for _, x := range []*Metric{b, h} {
				if x != nil {
					m.name, m.unit, m.lower = x.Name, x.Unit, x.LowerIsBetter
					break
				}
			}
		}
		// the calibrator group measures the runner itself: no round is
		// noisy for it
		rd := roundDelta{n: p.n, baseOK: measured(b), headOK: measured(h), noisy: p.cal.noisy && id.group != calibrator.group}
		if rd.valid() {
			rd.base, rd.head = b.P50, h.P50
			rd.pct = pctChange(rd.base, rd.head)
		}
		if b != nil && h != nil && b.Status == "ok" && h.Status == "fail" {
			rd.headFailed = true
			m.failedIn = append(m.failedIn, p.n)
		}
		m.rounds = append(m.rounds, rd)
	}

	// the headline comes from the rounds that count; if there are none, the
	// numbers are still shown, under the verdict noisy
	var valid, counted []roundDelta
	for _, r := range m.rounds {
		if r.valid() {
			valid = append(valid, r)
			if !r.noisy {
				counted = append(counted, r)
			}
		}
	}
	use := counted
	if len(use) == 0 {
		use = valid
	}
	if len(use) > 0 {
		var bs, hs []float64
		for _, r := range use {
			bs, hs = append(bs, r.base), append(hs, r.head)
		}
		m.baseMed, m.headMed = medianOf(bs), medianOf(hs)
		m.pct = pctChange(m.baseMed, m.headMed)
		m.have = true
	}
	switch {
	case len(valid) == 0:
		m.verdict = vNA
	case len(counted) == 0:
		m.verdict = vNoisy
	case math.Abs(m.pct) < samePct:
		m.verdict = vSame
	case m.worsePct(m.pct) > 0:
		m.verdict = vWorse
	default:
		m.verdict = vBetter
	}

	if rule, ok := gateFor(c.rules, id); ok {
		m.gated, m.threshold = true, rule.threshold
	}
	switch {
	case len(m.failedIn) > 0:
		// a failed check is not a measurement: it counts in any round, for
		// every metric, gated or not
		m.failed = failStatus
	case m.gated && !c.inconclusive && m.regressed(counted, c.minMS):
		m.failed = failRegression
	}
	if m.failed != notFailed {
		m.verdict = vFail
	}
	return m
}

// regressed reports whether the head is worse than the threshold, and by at
// least the absolute minimum, in every one of the rounds (and there is one).
//
// PERF: this trades sensitivity for stability. Demanding the regression in
// every round, by a percentage and by a few milliseconds, misses small
// regressions and ones that only show in some rounds; in return a change
// does not fail on the noise of a shared runner. The rounds passed in are
// those whose host CPU calibrator agrees between the sides: the others say
// something about the runner, not about the change.
func (m *metricCmp) regressed(rounds []roundDelta, minMS float64) bool {
	floor := absFloor(m.unit, minMS)
	for _, r := range rounds {
		if m.worsePct(r.pct) <= m.threshold || m.worseAbs(r.base, r.head) < floor {
			return false
		}
	}
	return len(rounds) > 0
}

// absFloor is the least absolute change that counts for a metric in unit:
// minMS for times, nothing for the rest.
func absFloor(unit string, minMS float64) float64 {
	switch unit {
	case "ms":
		return minMS
	case "s":
		return minMS / 1000
	}
	return 0
}

// failures are the metrics that failed the comparison, in report order.
func (c *comparison) failures() []*metricCmp {
	var out []*metricCmp
	for _, m := range c.metrics {
		if m.failed != notFailed {
			out = append(out, m)
		}
	}
	return out
}

// describe adds warnings about results that should not be compared as if
// they were interleaved runs of one backend on one runner, and about what
// could not be compared.
func (c *comparison) describe() {
	mixed := func(what string, get func(*Result) string) {
		var vals []string
		for _, p := range c.pairs {
			for _, r := range []*Result{p.base, p.head} {
				if v := get(r); !slices.Contains(vals, v) {
					vals = append(vals, v)
				}
			}
		}
		if len(vals) > 1 {
			c.warnings = append(c.warnings, fmt.Sprintf("the results differ in %s: %s", what, strings.Join(vals, ", ")))
		}
	}
	mixed("backend", func(r *Result) string { return r.Backend })
	mixed("platform", func(r *Result) string { return r.Platform })
	mixed("profile", func(r *Result) string { return r.Profile })
	if n := len(c.hosts()); n > 1 {
		c.warnings = append(c.warnings, fmt.Sprintf("the results come from %d different hosts, so the differences include the machines", n))
	}
	for _, p := range c.pairs {
		for _, s := range []struct {
			side string
			r    *Result
		}{{"base", p.base}, {"head", p.head}} {
			if len(s.r.Metrics) == 0 {
				c.warnings = append(c.warnings, fmt.Sprintf("%s round %d has no metrics", s.side, p.n))
			}
			for _, n := range s.r.Notes {
				c.warnings = append(c.warnings, fmt.Sprintf("%s round %d: %s", s.side, p.n, truncate(n, 200)))
			}
		}
	}
	// a gate cannot hold what one side did not measure (a phase that died
	// in the head leaves its metrics out): say so, the verdict is n/a
	var gaps []string
	for _, m := range c.metrics {
		if !m.gated {
			continue
		}
		var noHead, noBase []int
		for _, r := range m.rounds {
			switch {
			case r.baseOK && !r.headOK && !r.headFailed:
				noHead = append(noHead, r.n)
			case !r.baseOK && r.headOK:
				noBase = append(noBase, r.n)
			}
		}
		if len(noHead) > 0 {
			gaps = append(gaps, fmt.Sprintf("`%s` (head: %s %s)", m.id, plural(len(noHead), "round"), joinInts(noHead)))
		}
		if len(noBase) > 0 {
			gaps = append(gaps, fmt.Sprintf("`%s` (base: %s %s)", m.id, plural(len(noBase), "round"), joinInts(noBase)))
		}
	}
	if len(gaps) > 0 {
		c.warnings = append(c.warnings, "gated metrics measured by one side only: "+strings.Join(gaps, ", "))
	}
	if len(c.pairs) == 1 {
		c.warnings = append(c.warnings, "a single round: a regression is not confirmed by a second one")
	}
}

// ---- output ----

func (c *comparison) status() string {
	switch {
	case len(c.failures()) > 0:
		return "FAIL"
	case c.inconclusive:
		return "INCONCLUSIVE"
	}
	return "PASS"
}

// commits lists the distinct commits of one side.
func (c *comparison) commits(side func(*roundPair) *Result) string {
	var out []string
	for _, p := range c.pairs {
		if v := side(p).Commit; v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return "`" + strings.Join(out, "`, `") + "`"
}

// hosts lists the distinct hosts of all results.
func (c *comparison) hosts() []string {
	var out []string
	for _, p := range c.pairs {
		for _, r := range []*Result{p.base, p.head} {
			if !slices.Contains(out, r.Host) {
				out = append(out, r.Host)
			}
		}
	}
	return out
}

func (c *comparison) markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "## bench A/B: %s\n\n", c.label)
	fmt.Fprintf(&sb, "**%s** · %d %s · base %s · head %s\n\n", c.status(), len(c.pairs), plural(len(c.pairs), "round"),
		c.commits(func(p *roundPair) *Result { return p.base }), c.commits(func(p *roundPair) *Result { return p.head }))
	fmt.Fprintf(&sb, "Host: `%s`\n\n", strings.Join(c.hosts(), "`, `"))

	if fails := c.failures(); len(fails) > 0 {
		sb.WriteString("Failed:\n\n")
		for _, m := range fails {
			c.failure(&sb, m)
		}
		sb.WriteString("\n")
	}
	if c.inconclusive {
		fmt.Fprintf(&sb, "Inconclusive: %d of %d %s noisy, so no gate on performance was evaluated.\n\n", c.noisyRounds, len(c.pairs), plural(len(c.pairs), "round"))
	}

	fmt.Fprintf(&sb, "| round | host CPU (`%s`) base → head | drift | state |\n|---|---|---|---|\n", calibrator)
	for _, p := range c.pairs {
		if p.cal.known {
			fmt.Fprintf(&sb, "| %d | %s → %s %s | %s%% | %s |\n", p.n, fmtNum(p.cal.base), fmtNum(p.cal.head), p.cal.unit, fmtPct(p.cal.drift), p.state())
		} else {
			fmt.Fprintf(&sb, "| %d | — | — | %s |\n", p.n, p.state())
		}
	}
	sb.WriteString("\n")
	if len(c.warnings) > 0 {
		sb.WriteString("Warnings:\n\n")
		for _, w := range c.warnings {
			fmt.Fprintf(&sb, "- %s\n", w)
		}
		sb.WriteString("\n")
	}

	var gated []*metricCmp
	for _, m := range c.metrics {
		if m.gated {
			gated = append(gated, m)
		}
	}
	sb.WriteString("### Gated metrics\n\n")
	if len(c.rules) == 0 {
		sb.WriteString("Nothing is gated.\n")
	} else {
		fmt.Fprintf(&sb, "Gate: worse than %s and by at least %s ms in every round. Delta: head vs base, medians over the rounds; in the per-round list, (x) is a noisy round (host CPU drift over %s%%) and does not count. Under %s%% is same.\n\n",
			c.thresholds(), num(c.minMS), num(noisePct), num(samePct))
		c.table(&sb, gated)
		if len(c.unmatched) > 0 {
			fmt.Fprintf(&sb, "\nNo metric matched: `%s`\n", strings.Join(c.unmatched, "`, `"))
		}
	}

	fmt.Fprintf(&sb, "\n<details><summary>All metrics (%d)</summary>\n\n", len(c.metrics))
	c.table(&sb, c.metrics)
	sb.WriteString("\n</details>\n")
	return sb.String()
}

// failure writes the list item for a failed metric.
func (c *comparison) failure(sb *strings.Builder, m *metricCmp) {
	switch m.failed {
	case failStatus:
		fmt.Fprintf(sb, "- `%s`: failed in the head but not in the base (%s %s)\n", m.id, plural(len(m.failedIn), "round"), joinInts(m.failedIn))
	case failRegression:
		fmt.Fprintf(sb, "- `%s`: %s → %s %s, %s", m.id, fmtVal(m.unit, m.baseMed), fmtVal(m.unit, m.headMed), m.unit, pctCell(m.pct))
		if len(m.rounds) > 1 {
			fmt.Fprintf(sb, " (per round %s)", c.perRound(m))
		}
		fmt.Fprintf(sb, ", limit %s%%\n", num(m.threshold))
	}
}

// thresholds describes the limits of the gate rules: the most common one,
// then the others, e.g. "10% (20% for `2-warm/cold`)".
func (c *comparison) thresholds() string {
	counts := map[float64]int{}
	for _, r := range c.rules {
		counts[r.threshold]++
	}
	common, most := 0.0, 0
	for t, n := range counts {
		if n > most || (n == most && t < common) {
			common, most = t, n
		}
	}
	var other []string
	for _, r := range c.rules {
		if r.threshold != common {
			other = append(other, fmt.Sprintf("%s%% for `%s`", num(r.threshold), r.pattern))
		}
	}
	s := num(common) + "%"
	if len(other) > 0 {
		s += " (" + strings.Join(other, ", ") + ")"
	}
	return s
}

func (c *comparison) table(sb *strings.Builder, ms []*metricCmp) {
	sb.WriteString("| metric | name | unit | base | head | delta | per round | verdict |\n|---|---|---|---|---|---|---|---|\n")
	for _, m := range ms {
		base, head, delta := "—", "—", "—"
		if m.have {
			base, head, delta = fmtVal(m.unit, m.baseMed), fmtVal(m.unit, m.headMed), pctCell(m.pct)
		}
		verdict := m.verdict
		if m.verdict == vFail {
			verdict = "**FAIL**"
		}
		fmt.Fprintf(sb, "| `%s` | %s | %s | %s | %s | %s | %s | %s |\n",
			m.id, esc(m.name), esc(m.unit), base, head, delta, c.perRound(m), verdict)
	}
}

// perRound lists the change of every round in percent: "+4.1 / (+9.8)",
// with parentheses for a noisy round.
func (c *comparison) perRound(m *metricCmp) string {
	var parts []string
	for _, r := range m.rounds {
		switch {
		case r.headFailed:
			parts = append(parts, "fail")
		case !r.valid():
			parts = append(parts, "n/a")
		case r.noisy:
			parts = append(parts, "("+fmtPct(r.pct)+")")
		default:
			parts = append(parts, fmtPct(r.pct))
		}
	}
	return strings.Join(parts, " / ")
}

func esc(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

func joinInts(v []int) string {
	var parts []string
	for _, n := range v {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// num formats a setting (a threshold) without trailing zeros.
func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// fmtVal formats a value; checks (unit 通过) are 1 for pass.
func fmtVal(unit string, v float64) string {
	if unit == "通过" {
		if v == 1 {
			return "pass"
		}
		return "fail"
	}
	return fmtNum(v)
}

// fmtPct formats a percentage with a sign and without the % sign.
func fmtPct(p float64) string {
	switch {
	case math.IsInf(p, 1):
		return "+inf"
	case math.IsInf(p, -1):
		return "-inf"
	case math.Abs(p) < 0.05:
		return "0.0"
	case math.Abs(p) < 100:
		return fmt.Sprintf("%+.1f", p)
	}
	return fmt.Sprintf("%+.0f", p)
}

// pctCell is a percentage for a table cell: fmtPct with a % sign, except
// for the infinite change from zero.
func pctCell(p float64) string {
	if math.IsInf(p, 0) {
		return fmtPct(p)
	}
	return fmtPct(p) + "%"
}

// pctChange is the change from base to head in percent (±Inf from zero).
func pctChange(base, head float64) float64 {
	switch {
	case head == base:
		return 0
	case base == 0 && head > 0:
		return math.Inf(1)
	case base == 0:
		return math.Inf(-1)
	}
	return (head - base) / math.Abs(base) * 100
}

// medianOf is the median; for an even count it is the mean of the two
// middle values (a two-round A/B has no middle one). median in run.go takes
// the upper one, which is fine for its dozens of samples.
func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	if n := len(s); n%2 == 0 {
		return (s[n/2-1] + s[n/2]) / 2
	}
	return s[len(s)/2]
}
