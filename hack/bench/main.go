// Command bench measures oar end to end and writes machine readable results
// plus a markdown report.
//
//	go run ./hack/bench run -backend native -out results-native.json [-profile quick|full] [-- vm start flags]
//	go run ./hack/bench report -title "Step 0" -out report.md [-baseline dir] results-*.json
//	go run ./hack/bench compare -dir results -label kvm [-fail-on 10] [-min-ms 3] [-gate patterns]
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"
)

// Metric is one measured quantity. Samples hold raw observations; summary
// fields are derived.
type Metric struct {
	Group   string    `json:"group"`
	Key     string    `json:"key"`
	Name    string    `json:"name"`
	Unit    string    `json:"unit"`
	Samples []float64 `json:"samples,omitempty"`
	N       int       `json:"n"`
	P50     float64   `json:"p50"`
	P95     float64   `json:"p95"`
	P99     float64   `json:"p99"`
	Max     float64   `json:"max"`
	Min     float64   `json:"min"`
	Status  string    `json:"status"` // ok, na, fail
	Note    string    `json:"note,omitempty"`
	// LowerIsBetter decides the direction of deltas in reports.
	LowerIsBetter bool `json:"lowerIsBetter"`
}

// Result is the output of one benchmark run on one backend.
type Result struct {
	Backend  string    `json:"backend"`
	Platform string    `json:"platform"`
	Host     string    `json:"host"`
	Commit   string    `json:"commit"`
	Profile  string    `json:"profile"`
	Started  time.Time `json:"started"`
	Duration float64   `json:"durationSeconds"`
	Metrics  []Metric  `json:"metrics"`
	Notes    []string  `json:"notes,omitempty"`
}

func summarize(m *Metric) {
	m.N = len(m.Samples)
	if m.N == 0 {
		return
	}
	s := append([]float64(nil), m.Samples...)
	sort.Float64s(s)
	q := func(p float64) float64 {
		idx := int(math.Ceil(p*float64(len(s)))) - 1
		if idx < 0 {
			idx = 0
		}
		return s[idx]
	}
	m.Min, m.Max = s[0], s[len(s)-1]
	m.P50, m.P95, m.P99 = q(0.50), q(0.95), q(0.99)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: bench run|report|compare ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runMain(os.Args[2:])
	case "report":
		err = reportMain(os.Args[2:])
	case "compare":
		err = compareMain(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func ms(d time.Duration) float64 { return float64(d) / 1e6 }
