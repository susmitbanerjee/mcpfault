// Package report holds the result model of a scenario session and renders it
// for terminals, JSON, and JUnit XML.
package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/lint"
)

// Report is everything known about one scenario session.
type Report struct {
	ID          string          `json:"id"`
	Scenario    string          `json:"scenario"`
	File        string          `json:"file"`
	Description string          `json:"description,omitempty"`
	Agent       string          `json:"agent"`
	Status      string          `json:"status"` // running | passed | failed
	Started     time.Time       `json:"started"`
	Finished    *time.Time      `json:"finished,omitempty"`
	PlannedRuns int             `json:"planned_runs"`
	Faults      []string        `json:"faults"`
	Servers     []ServerInfo    `json:"servers"`
	Invariants  []InvariantStat `json:"invariants"`
	Recovery    []Recovery      `json:"recovery,omitempty"`
	Contract    []lint.Finding  `json:"contract,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
	Runs        []*RunResult    `json:"runs"`
	Passed      bool            `json:"passed"`
	Dir         string          `json:"dir"`
}

// ServerInfo describes how the agent was expected to reach a server, and whether it did.
type ServerInfo struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint"`
	Connected bool   `json:"connected"`
	Calls     int    `json:"calls"`
}

// InvariantStat aggregates one invariant across runs.
type InvariantStat struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Severity    string `json:"severity"`
	Failed      int    `json:"failed"`
	Evaluated   int    `json:"evaluated"`
	FailingRuns []int  `json:"failing_runs,omitempty"`
}

// RunResult is one execution of the agent.
type RunResult struct {
	Index      int               `json:"index"`
	Status     string            `json:"status"` // passed | failed | error
	Error      string            `json:"error,omitempty"`
	ExitCode   *int              `json:"exit_code,omitempty"`
	DurationMS int64             `json:"duration_ms"`
	Output     string            `json:"output,omitempty"`
	Stderr     string            `json:"stderr,omitempty"`
	Invariants []InvariantResult `json:"invariants"`
	Calls      []engine.Call     `json:"calls"`
	Dir        string            `json:"dir"`
}

// InvariantResult is one invariant's verdict for one run.
type InvariantResult struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
	Pass     bool   `json:"pass"`
	Detail   string `json:"detail,omitempty"`
}

// Recovery summarizes what agents did after a call succeeded on the server but looked failed.
type Recovery struct {
	Tool          string         `json:"tool"`
	Fault         string         `json:"fault"`
	ContractLevel string         `json:"contract_level,omitempty"`
	Total         int            `json:"total"`
	Outcomes      map[string]int `json:"outcomes"`
}

// Recovery outcome labels.
const (
	RetriedBlind   = "retried without checking state or reusing a key"
	RetriedSameKey = "retried with the same idempotency key"
	CheckedRetried = "checked state, then retried"
	CheckedStopped = "checked state, did not retry"
	DidNotRetry    = "did not retry"
	Crashed        = "crashed or timed out before recovering"
)

// Finalize computes the aggregates from the run results.
func (r *Report) Finalize(invNames []InvariantStat) {
	r.Invariants = make([]InvariantStat, len(invNames))
	for i, base := range invNames {
		st := base
		st.Failed, st.Evaluated, st.FailingRuns = 0, 0, nil
		for _, run := range r.Runs {
			if i >= len(run.Invariants) {
				continue
			}
			st.Evaluated++
			if !run.Invariants[i].Pass {
				st.Failed++
				st.FailingRuns = append(st.FailingRuns, run.Index)
			}
		}
		r.Invariants[i] = st
	}
	r.Recovery = analyzeRecovery(r.Runs, r.Contract)
	r.Passed = true
	for _, run := range r.Runs {
		if run.Status == "error" {
			r.Passed = false
		}
	}
	for _, st := range r.Invariants {
		if st.Severity == "error" && st.Failed > 0 {
			r.Passed = false
		}
	}
	for i := range r.Servers {
		n := 0
		for _, run := range r.Runs {
			for _, c := range run.Calls {
				if c.Server == r.Servers[i].Name {
					n++
				}
			}
		}
		r.Servers[i].Calls = n
	}
}

var ambiguous = map[string]bool{"lost_response": true, "delay": true, "disconnect": true}

func analyzeRecovery(runs []*RunResult, contract []lint.Finding) []Recovery {
	byTool := map[string]*Recovery{}
	var order []string
	for _, run := range runs {
		for i, c := range run.Calls {
			if c.Fault == nil || !ambiguous[c.Fault.Type] || c.Committed == nil || !*c.Committed {
				continue
			}
			var finding *lint.Finding
			for j := range contract {
				if contract[j].Server == c.Server && contract[j].Tool == c.Tool {
					finding = &contract[j]
				}
			}
			known := finding != nil
			if !known {
				finding = inferFinding(c, run.Calls)
			}
			later := run.Calls[i+1:]
			retryAt := -1
			for j, l := range later {
				if l.Server == c.Server && l.Tool == c.Tool {
					retryAt = j
					break
				}
			}
			before := later
			if retryAt >= 0 {
				before = later[:retryAt]
			}
			reconciled := false
			if finding != nil {
				for _, b := range before {
					for _, lk := range finding.LookupTools {
						if b.Server == c.Server && b.Tool == lk {
							reconciled = true
						}
					}
				}
			}
			sameKey := false
			if retryAt >= 0 && finding != nil {
				var a, b map[string]any
				_ = json.Unmarshal(c.Args, &a)
				_ = json.Unmarshal(later[retryAt].Args, &b)
				for _, p := range finding.IdempotencyParams {
					if a[p] != nil && a[p] == b[p] {
						sameKey = true
					}
				}
			}
			var outcome string
			switch {
			case retryAt < 0 && run.Status == "error":
				outcome = Crashed
			case retryAt < 0 && reconciled:
				outcome = CheckedStopped
			case retryAt < 0:
				outcome = DidNotRetry
			case sameKey:
				outcome = RetriedSameKey
			case reconciled:
				outcome = CheckedRetried
			default:
				outcome = RetriedBlind
			}
			key := c.Server + "/" + c.Tool
			rec := byTool[key]
			if rec == nil {
				rec = &Recovery{Tool: key, Fault: c.Fault.Label, Outcomes: map[string]int{}}
				if known {
					rec.ContractLevel = finding.Level
				}
				byTool[key] = rec
				order = append(order, key)
			}
			rec.Total++
			rec.Outcomes[outcome]++
		}
	}
	out := make([]Recovery, 0, len(order))
	for _, k := range order {
		out = append(out, *byTool[k])
	}
	return out
}

// inferFinding guesses a tool's idempotency parameters and lookup tools from the calls
// themselves, for agents that never called tools/list.
func inferFinding(c engine.Call, calls []engine.Call) *lint.Finding {
	f := &lint.Finding{Server: c.Server, Tool: c.Tool}
	var args map[string]any
	_ = json.Unmarshal(c.Args, &args)
	for k := range args {
		if lint.IsIdempotencyParam(k) {
			f.IdempotencyParams = append(f.IdempotencyParams, k)
		}
	}
	seen := map[string]bool{}
	var names []string
	for _, other := range calls {
		if other.Server == c.Server && !seen[other.Tool] {
			seen[other.Tool] = true
			names = append(names, other.Tool)
		}
	}
	f.LookupTools = lint.Lookups(c.Tool, names)
	return f
}

// Save writes report.json into the report's directory.
func (r *Report) Save() error {
	if r.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(r.Dir, "report.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(r.Dir, "report.json"))
}

// Summary is the lightweight listing entry for a stored session.
type Summary struct {
	ID          string     `json:"id"`
	Scenario    string     `json:"scenario"`
	Agent       string     `json:"agent"`
	Status      string     `json:"status"`
	Started     time.Time  `json:"started"`
	Finished    *time.Time `json:"finished,omitempty"`
	Runs        int        `json:"runs"`
	PlannedRuns int        `json:"planned_runs"`
	Violations  int        `json:"violations"`
}

// Load reads a stored report.
func Load(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	return &r, json.Unmarshal(b, &r)
}

// List returns summaries of the sessions stored under dir, newest first.
func List(dir string) []Summary {
	entries, _ := os.ReadDir(dir)
	var out []Summary
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(filepath.Join(dir, e.Name(), "report.json"))
		if err != nil {
			continue
		}
		v := 0
		for _, st := range r.Invariants {
			if st.Severity == "error" && st.Failed > 0 {
				v++
			}
		}
		out = append(out, Summary{ID: r.ID, Scenario: r.Scenario, Agent: r.Agent, Status: r.Status, Started: r.Started, Finished: r.Finished, Runs: len(r.Runs), PlannedRuns: r.PlannedRuns, Violations: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}
