// Package lint checks whether an MCP server's write tools can be retried safely
// after an ambiguous failure (a timeout after the server already did the work).
package lint

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Tool is the part of an MCP tool definition lint looks at.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	} `json:"inputSchema"`
	Annotations *struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
	} `json:"annotations"`
}

// Finding levels.
const (
	LevelOK   = "ok"   // required idempotency key
	LevelInfo = "info" // optional key, or declared idempotent
	LevelWarn = "warn" // no key, but a lookup tool exists
	LevelGap  = "gap"  // no key and no lookup: nothing can retry safely
)

// Finding is the verdict for one write tool.
type Finding struct {
	Server             string   `json:"server"`
	Tool               string   `json:"tool"`
	Level              string   `json:"level"`
	Message            string   `json:"message"`
	IdempotencyParams  []string `json:"idempotency_params,omitempty"`
	LookupTools        []string `json:"lookup_tools,omitempty"`
	DeclaredIdempotent bool     `json:"declared_idempotent,omitempty"`
}

var (
	writeVerbs = set("create add insert update upsert set delete remove send post pay charge refund transfer submit book cancel issue record write execute run trigger approve place make patch put modify publish deploy grant revoke invite capture void move rename archive merge close open assign")
	readVerbs  = set("get list find search lookup read fetch query describe show check status view retrieve count")
	idemParam  = regexp.MustCompile(`(?i)idempot|dedup|request_?id|client_?(request|ref|reference|token)|operation_?id|nonce|external_?id|unique_?key`)
)

func set(words string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(words) {
		m[w] = true
	}
	return m
}

func tokens(name string) []string {
	var b strings.Builder
	prevLower := false
	for _, r := range name {
		if unicode.IsUpper(r) && prevLower {
			b.WriteByte('_')
		}
		prevLower = unicode.IsLower(r) || unicode.IsDigit(r)
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.FieldsFunc(b.String(), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func stem(t string) string {
	switch {
	case len(t) > 4 && strings.HasSuffix(t, "es") && !strings.HasSuffix(t, "ses"):
		return t[:len(t)-1]
	case len(t) > 3 && strings.HasSuffix(t, "s"):
		return t[:len(t)-1]
	}
	return t
}

// Classify returns "read", "write" or "unknown".
func Classify(t Tool) string {
	if t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint {
		return "read"
	}
	tk := tokens(t.Name)
	if len(tk) > 0 && readVerbs[tk[0]] {
		return "read"
	}
	for _, x := range tk {
		if writeVerbs[x] {
			return "write"
		}
	}
	if t.Annotations != nil && t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
		return "write"
	}
	return "unknown"
}

// IsIdempotencyParam reports whether a parameter name looks like an idempotency key.
func IsIdempotencyParam(name string) bool { return idemParam.MatchString(name) }

// Lookups returns the read tools among names that share a noun with tool,
// e.g. list_payments for create_payment.
func Lookups(tool string, names []string) []string {
	nouns := map[string]bool{}
	for _, x := range tokens(tool) {
		if !writeVerbs[x] {
			nouns[stem(x)] = true
		}
	}
	var out []string
	for _, name := range names {
		if name == tool || Classify(Tool{Name: name}) != "read" {
			continue
		}
		for _, x := range tokens(name) {
			if nouns[stem(x)] {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// Check lints one server's tools.
func Check(server string, tools []Tool) []Finding {
	var out []Finding
	for _, t := range tools {
		if Classify(t) != "write" {
			continue
		}
		var params []string
		for p := range t.InputSchema.Properties {
			if idemParam.MatchString(p) {
				params = append(params, p)
			}
		}
		sort.Strings(params)
		required := map[string]bool{}
		for _, r := range t.InputSchema.Required {
			required[r] = true
		}
		nouns := map[string]bool{}
		for _, x := range tokens(t.Name) {
			if !writeVerbs[x] {
				nouns[stem(x)] = true
			}
		}
		var lookups []string
		for _, o := range tools {
			if o.Name == t.Name || Classify(o) != "read" {
				continue
			}
			for _, x := range tokens(o.Name) {
				if nouns[stem(x)] {
					lookups = append(lookups, o.Name)
					break
				}
			}
		}
		declared := t.Annotations != nil && t.Annotations.IdempotentHint != nil && *t.Annotations.IdempotentHint
		f := Finding{Server: server, Tool: t.Name, IdempotencyParams: params, LookupTools: lookups, DeclaredIdempotent: declared}
		switch {
		case len(params) > 0:
			optional := true
			for _, p := range params {
				if required[p] {
					optional = false
				}
			}
			if optional {
				f.Level, f.Message = LevelInfo, "accepts "+strings.Join(params, "/")+" but it is optional, so retries are only safe if the agent sends it"
			} else {
				f.Level, f.Message = LevelOK, "requires "+strings.Join(params, "/")+", so retries can be made safe"
			}
		case declared:
			f.Level, f.Message = LevelInfo, "declares idempotentHint but has no idempotency key; prove it with a lost_response fault"
		case len(lookups) > 0:
			f.Level, f.Message = LevelWarn, "no idempotency key; after an ambiguous failure the agent must check "+strings.Join(lookups, " / ")+" before retrying"
		default:
			f.Level, f.Message = LevelGap, "no idempotency key and no lookup tool; after a timeout no agent can tell whether this ran, so it cannot be retried safely"
		}
		out = append(out, f)
	}
	return out
}

// CheckRaw lints a raw tools array from tools/list.
func CheckRaw(server string, raw json.RawMessage) []Finding {
	var tools []Tool
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	return Check(server, tools)
}
