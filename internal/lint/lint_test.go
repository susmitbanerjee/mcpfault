package lint

import (
	"encoding/json"
	"testing"
)

func tools(t *testing.T, raw string) []Tool {
	t.Helper()
	var out []Tool
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestClassify(t *testing.T) {
	ro := true
	cases := map[string]string{"create_payment": "write", "sendEmail": "write", "list_payments": "read", "getInvoice": "read", "summarize": "unknown", "move_file": "write"}
	for name, want := range cases {
		if got := Classify(Tool{Name: name}); got != want {
			t.Errorf("%s: got %s want %s", name, got, want)
		}
	}
	tool := Tool{Name: "record_thing"}
	tool.Annotations = &struct {
		ReadOnlyHint    *bool `json:"readOnlyHint"`
		DestructiveHint *bool `json:"destructiveHint"`
		IdempotentHint  *bool `json:"idempotentHint"`
	}{ReadOnlyHint: &ro}
	if Classify(tool) != "read" {
		t.Error("readOnlyHint should win")
	}
}

func TestLevels(t *testing.T) {
	cases := []struct {
		raw   string
		level string
	}{
		{`[{"name":"create_payment","inputSchema":{"properties":{"amount":{}}}}]`, LevelGap},
		{`[{"name":"create_payment","inputSchema":{"properties":{"amount":{}}}},{"name":"list_payments"}]`, LevelWarn},
		{`[{"name":"create_payment","inputSchema":{"properties":{"idempotency_key":{}}}}]`, LevelInfo},
		{`[{"name":"create_payment","inputSchema":{"properties":{"requestId":{}},"required":["requestId"]}}]`, LevelOK},
		{`[{"name":"create_payment","annotations":{"idempotentHint":true}}]`, LevelInfo},
	}
	for _, c := range cases {
		f := Check("s", tools(t, c.raw))
		if len(f) != 1 || f[0].Level != c.level {
			t.Errorf("%s: got %+v want %s", c.raw, f, c.level)
		}
	}
}

func TestReadToolsAreIgnored(t *testing.T) {
	if f := Check("s", tools(t, `[{"name":"get_invoice"},{"name":"search_docs"}]`)); len(f) != 0 {
		t.Fatalf("%+v", f)
	}
}
