// Package invariants evaluates scenario invariants against what happened in a run.
package invariants

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mcpfault/mcpfault/internal/config"
	"github.com/mcpfault/mcpfault/internal/engine"
	"github.com/mcpfault/mcpfault/internal/jsonx"
)

// Outcome is the result of one invariant in one run.
type Outcome struct {
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Record turns a call into the generic JSON shape `where` and `distinct` paths address:
// id, server, tool, args.*, fault.type, forwarded, committed, result.*, delivered.kind ...
func Record(c engine.Call) map[string]any {
	b, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if args, ok := m["args"]; ok {
		m["args"] = jsonx.Normalize(args)
	}
	return m
}

// EvaluateLog counts matching calls (or distinct values) and checks the bounds.
func EvaluateLog(inv *config.Invariant, calls []engine.Call) (Outcome, error) {
	var selected []map[string]any
	var ids []int64
	for _, c := range calls {
		if c.Tool != inv.Tool || (inv.Server != "" && c.Server != inv.Server) {
			continue
		}
		if inv.Committed != nil && (c.Committed != nil && *c.Committed) != *inv.Committed {
			continue
		}
		rec := Record(c)
		ok, err := Where(inv.Where, rec)
		if err != nil {
			return Outcome{}, err
		}
		if ok {
			selected = append(selected, rec)
			if c.Seq > 0 {
				ids = append(ids, int64(c.Seq))
			} else {
				ids = append(ids, c.ID)
			}
		}
	}
	observed := len(selected)
	what := "matching call"
	if inv.Distinct != "" {
		seen := map[string]bool{}
		for _, rec := range selected {
			if v, ok := jsonx.Get(rec, inv.Distinct); ok && v != nil {
				seen[jsonx.Key(v)] = true
			}
		}
		observed, what = len(seen), "distinct "+inv.Distinct
	} else if observed != 1 {
		what += "s"
	}
	pass := (inv.Equals == nil || observed == *inv.Equals) &&
		(inv.Min == nil || observed >= *inv.Min) &&
		(inv.Max == nil || observed <= *inv.Max)
	detail := fmt.Sprintf("%d %s", observed, what)
	if len(ids) > 0 {
		refs := make([]string, 0, len(ids))
		for i, id := range ids {
			if i == 8 {
				refs = append(refs, "…")
				break
			}
			refs = append(refs, fmt.Sprintf("#%d", id))
		}
		detail += " (calls " + strings.Join(refs, ", ") + ")"
	}
	return Outcome{Pass: pass, Detail: detail}, nil
}

// Where reports whether a record satisfies every condition. A condition is either a
// literal (equality) or a map of operators: eq, ne, in, gt, gte, lt, lte, exists, matches.
func Where(where map[string]any, rec map[string]any) (bool, error) {
	for path, cond := range where {
		actual, present := jsonx.Get(rec, path)
		ops, isOps := cond.(map[string]any)
		if !isOps || !looksLikeOps(ops) {
			if !present || !jsonx.Equal(actual, cond) {
				return false, nil
			}
			continue
		}
		for op, v := range ops {
			ok, err := apply(op, actual, present, v)
			if err != nil {
				return false, fmt.Errorf("where %s: %w", path, err)
			}
			if !ok {
				return false, nil
			}
		}
	}
	return true, nil
}

var operators = map[string]bool{"eq": true, "ne": true, "in": true, "gt": true, "gte": true, "lt": true, "lte": true, "exists": true, "matches": true}

func looksLikeOps(m map[string]any) bool {
	for k := range m {
		if !operators[k] {
			return false
		}
	}
	return len(m) > 0
}

func apply(op string, actual any, present bool, v any) (bool, error) {
	switch op {
	case "eq":
		return present && jsonx.Equal(actual, v), nil
	case "ne":
		return !present || !jsonx.Equal(actual, v), nil
	case "in":
		list, ok := v.([]any)
		if !ok {
			return false, fmt.Errorf("`in` needs a list")
		}
		for _, x := range list {
			if present && jsonx.Equal(actual, x) {
				return true, nil
			}
		}
		return false, nil
	case "exists":
		want, _ := v.(bool)
		return present == want, nil
	case "matches":
		re, err := regexp.Compile(fmt.Sprint(v))
		if err != nil {
			return false, err
		}
		return present && re.MatchString(fmt.Sprint(actual)), nil
	case "gt", "gte", "lt", "lte":
		a, ok1 := jsonx.ToFloat(actual)
		b, ok2 := jsonx.ToFloat(v)
		if !present || !ok1 || !ok2 {
			return false, nil
		}
		switch op {
		case "gt":
			return a > b, nil
		case "gte":
			return a >= b, nil
		case "lt":
			return a < b, nil
		default:
			return a <= b, nil
		}
	}
	return false, fmt.Errorf("unknown operator %q", op)
}

// EvaluateOutput applies an output invariant to the agent's output.
func EvaluateOutput(inv *config.Invariant, output string) Outcome {
	if inv.Matches != nil && !inv.Matches.MatchString(output) {
		return Outcome{Pass: false, Detail: "output did not match /" + inv.Matches.String()[4:] + "/"}
	}
	if inv.NotMatch != nil {
		if m := inv.NotMatch.FindString(output); m != "" {
			return Outcome{Pass: false, Detail: fmt.Sprintf("output contained %q", m)}
		}
	}
	return Outcome{Pass: true}
}
