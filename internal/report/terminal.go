package report

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/mcpfault/mcpfault/internal/lint"
)

// Style controls terminal colors.
type Style struct{ Color bool }

func (s Style) wrap(code, v string) string {
	if !s.Color {
		return v
	}
	return "\x1b[" + code + "m" + v + "\x1b[0m"
}
func (s Style) Red(v string) string    { return s.wrap("31", v) }
func (s Style) Green(v string) string  { return s.wrap("32", v) }
func (s Style) Yellow(v string) string { return s.wrap("33", v) }
func (s Style) Cyan(v string) string   { return s.wrap("36", v) }
func (s Style) Dim(v string) string    { return s.wrap("2", v) }
func (s Style) Bold(v string) string   { return s.wrap("1", v) }

func visible(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case r == '\x1b':
			esc = true
		case esc && r == 'm':
			esc = false
		case !esc:
			n++
		}
	}
	return n
}

func pad(s string, width int) string {
	if n := visible(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s + " "
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Print renders a finished (or running) report.
func Print(w io.Writer, r *Report, st Style, cwd string) {
	rel := func(p string) string {
		if x, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(x, "..") {
			return x
		}
		return p
	}
	fmt.Fprintf(w, "\n%s · %s\n", st.Bold("mcpfault"), st.Bold(r.Scenario))
	if r.Description != "" {
		fmt.Fprintln(w, st.Dim("  "+r.Description))
	}
	fmt.Fprintf(w, "  Agent  %s\n", r.Agent)
	fmt.Fprintf(w, "  Runs   %d   %s\n", len(r.Runs), st.Dim("results: "+rel(r.Dir)))

	for _, s := range r.Servers {
		if !s.Connected {
			fmt.Fprintf(w, "  %s server %q never connected through mcpfault. Point your agent at: %s\n", st.Red("✗"), s.Name, s.Endpoint)
		}
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "  %s %s\n", st.Yellow("!"), warning)
	}

	if len(r.Faults) > 0 {
		fmt.Fprintf(w, "\n  %s\n", st.Bold("Faults"))
		for _, f := range r.Faults {
			fmt.Fprintf(w, "    %s\n", f)
		}
	}

	width := 50
	for _, inv := range r.Invariants {
		n := utf8.RuneCountInString(inv.Name) + 3
		if inv.Severity == "warn" {
			n += 7
		}
		if n > width {
			width = n
		}
	}
	fmt.Fprintf(w, "\n  %s%s%s\n", st.Bold("Invariants"), strings.Repeat(" ", width-8), st.Dim("violated"))
	for _, inv := range r.Invariants {
		bad := inv.Failed > 0
		mark, rate := st.Green("✓"), st.Dim(fmt.Sprintf("%d/%d", inv.Failed, inv.Evaluated))
		switch {
		case inv.Evaluated == 0:
			mark = st.Dim("–")
		case bad && inv.Severity == "warn":
			mark, rate = st.Yellow("!"), st.Yellow(fmt.Sprintf("%d/%d", inv.Failed, inv.Evaluated))
		case bad:
			mark, rate = st.Red("✗"), st.Red(fmt.Sprintf("%d/%d", inv.Failed, inv.Evaluated))
		}
		label := inv.Name
		if inv.Severity == "warn" {
			label += st.Dim(" (warn)")
		}
		pct := ""
		if bad && inv.Evaluated > 0 {
			pct = st.Dim(fmt.Sprintf("%d%%", inv.Failed*100/inv.Evaluated))
		}
		fmt.Fprintf(w, "    %s %s%s%s\n", mark, pad(label, width), pad(rate, 8), pct)
	}
	errored := 0
	for _, run := range r.Runs {
		if run.Status == "error" {
			errored++
		}
	}
	if errored > 0 {
		fmt.Fprintf(w, "    %s %s%s\n", st.Red("✗"), pad("runs that errored (setup, timeout, agent crash)", width), st.Red(fmt.Sprintf("%d/%d", errored, len(r.Runs))))
	}

	for _, rec := range r.Recovery {
		fmt.Fprintf(w, "\n  %s\n", st.Bold(fmt.Sprintf("After %s ran but the agent wasn't told (%d×), the agent:", rec.Tool, rec.Total)))
		keys := make([]string, 0, len(rec.Outcomes))
		for k := range rec.Outcomes {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return rec.Outcomes[keys[i]] > rec.Outcomes[keys[j]] })
		for _, k := range keys {
			label := k
			if k == RetriedBlind || k == Crashed {
				label = st.Red(k)
			}
			fmt.Fprintf(w, "    %s%d\n", pad(label, 52), rec.Outcomes[k])
		}
		switch {
		case rec.ContractLevel == lint.LevelGap:
			fmt.Fprintln(w, st.Dim("    → The tool has no idempotency key and no lookup tool, so no agent can retry it safely."))
			fmt.Fprintln(w, st.Dim("      This is a tool-contract gap: escalating is the best an agent can do until the API changes."))
		case rec.Outcomes[RetriedBlind] > 0 && rec.ContractLevel != "":
			fmt.Fprintln(w, st.Dim("    → The tool offers a safe path (see Tool contract) and the agent didn't use it."))
			fmt.Fprintln(w, st.Dim("      This is an agent bug, not a tool-contract gap."))
		}
	}

	var shown []lint.Finding
	for _, f := range r.Contract {
		if f.Level != lint.LevelOK {
			shown = append(shown, f)
		}
	}
	if len(shown) > 0 {
		fmt.Fprintf(w, "\n  %s%s\n", st.Bold("Tool contract"), st.Dim("  (heuristic, from tools/list)"))
		for _, f := range shown {
			mark := st.Cyan("i")
			switch f.Level {
			case lint.LevelGap:
				mark = st.Red("✗")
			case lint.LevelWarn:
				mark = st.Yellow("!")
			}
			fmt.Fprintf(w, "    %s %s/%s: %s\n", mark, f.Server, f.Tool, f.Message)
		}
	}

	for _, run := range r.Runs {
		if run.Status == "passed" {
			continue
		}
		printRun(w, run, st, rel)
		break
	}

	fmt.Fprintln(w)
	if r.Passed {
		fmt.Fprintf(w, "%s  all invariants held in %d/%d runs\n\n", st.Green(st.Bold("PASS")), len(r.Runs), len(r.Runs))
		return
	}
	var parts []string
	n := 0
	for _, inv := range r.Invariants {
		if inv.Severity == "error" && inv.Failed > 0 {
			n++
		}
	}
	if n > 0 {
		parts = append(parts, fmt.Sprintf("%d invariant%s violated", n, plural(n)))
	}
	if errored > 0 {
		parts = append(parts, fmt.Sprintf("%d run%s errored", errored, plural(errored)))
	}
	fmt.Fprintf(w, "%s  %s\n\n", st.Red(st.Bold("FAIL")), strings.Join(parts, ", "))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func printRun(w io.Writer, run *RunResult, st Style, rel func(string) string) {
	fmt.Fprintf(w, "\n  %s%s\n", st.Bold(fmt.Sprintf("First failing run: run %d", run.Index)), st.Dim("  "+rel(run.Dir)))
	if run.Error != "" {
		fmt.Fprintf(w, "    %s\n", st.Red(run.Error))
	}
	if len(run.Calls) > 0 {
		fmt.Fprintln(w, st.Dim(fmt.Sprintf("     #  %s%s%s", pad("tool call", 74), pad("server", 11), "agent saw")))
		for _, c := range run.Calls {
			server := "—"
			switch {
			case !c.Forwarded:
				server = "not sent"
			case c.Committed != nil && *c.Committed:
				server = "executed"
			case c.Committed != nil:
				server = "rejected"
			}
			saw := "—"
			if c.Delivered != nil {
				saw = c.Delivered.Kind
			}
			tag := ""
			if c.Fault != nil {
				tag = st.Yellow("  ← " + c.Fault.Type)
			}
			call := c.Server + "/" + c.Tool + " " + compactArgs(c.Args)
			n := int64(c.Seq)
			if n == 0 {
				n = c.ID
			}
			fmt.Fprintf(w, "    %2d  %s%s%s%s\n", n, pad(trunc(call, 72), 74), pad(server, 11), saw, tag)
		}
	}
	if out := strings.TrimSpace(run.Output); out != "" {
		lines := strings.Split(out, "\n")
		fmt.Fprintln(w, st.Dim("    agent output: "+trunc(lines[len(lines)-1], 110)))
	}
	if run.Status == "error" && strings.TrimSpace(run.Stderr) != "" {
		lines := strings.Split(strings.TrimSpace(run.Stderr), "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		fmt.Fprintln(w, st.Yellow("    agent stderr:"))
		for _, l := range lines {
			fmt.Fprintln(w, st.Dim("      "+trunc(l, 130)))
		}
	}
	for _, inv := range run.Invariants {
		if inv.Pass {
			continue
		}
		mark := st.Red("✗")
		if inv.Severity == "warn" {
			mark = st.Yellow("!")
		}
		detail := ""
		if inv.Detail != "" {
			detail = st.Dim(" — " + trunc(inv.Detail, 120))
		}
		fmt.Fprintf(w, "    %s %s%s\n", mark, inv.Name, detail)
	}
}

func compactArgs(raw json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := m[k]
		if s, ok := v.(string); ok && !strings.ContainsAny(s, " \t") {
			parts = append(parts, k+"="+s)
			continue
		}
		b, _ := json.Marshal(v)
		parts = append(parts, k+"="+string(b))
	}
	return strings.Join(parts, " ")
}

// PrintLint renders lint findings.
func PrintLint(w io.Writer, findings []lint.Finding, st Style) {
	if len(findings) == 0 {
		fmt.Fprintln(w, "No write tools detected.")
		return
	}
	for _, f := range findings {
		mark := st.Green("✓ ok  ")
		switch f.Level {
		case lint.LevelGap:
			mark = st.Red("✗ gap ")
		case lint.LevelWarn:
			mark = st.Yellow("! warn")
		case lint.LevelInfo:
			mark = st.Cyan("i info")
		}
		fmt.Fprintf(w, "%s  %s/%s: %s\n", mark, f.Server, f.Tool, f.Message)
	}
}
