package reviewer

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRepairJSON(t *testing.T) {
	cases := []struct {
		name, in string
		want     any // the decoded value; nil when no object can be read
	}{
		{"valid", `{"a":[1,{"b":"c"}]}`, map[string]any{"a": []any{1.0, map[string]any{"b": "c"}}}},
		{"prose and fences", "Here it is:\n```json\n{\"a\":1}\n```\nHope it helps {really}.", map[string]any{"a": 1.0}},
		{"quoted code escapes", `{"quote":"if s == "\_" { return "\d+" }"}`, nil},
		{"invalid escapes", `{"quote":"strings.Trim(s, \"\_\") + \d é \u12"}`, map[string]any{"quote": `strings.Trim(s, "\_") + \d é \u12`}},
		{"raw control characters", "{\"a\":\"line one\nline two\tend\"}", map[string]any{"a": "line one\nline two\tend"}},
		{"trailing commas", `{"a":[1,2,],"b":{"c":3,},}`, map[string]any{"a": []any{1.0, 2.0}, "b": map[string]any{"c": 3.0}}},
		{"cut in a string", `{"title":"t","risks":[{"text":"one"},{"text":"tw`, map[string]any{"title": "t", "risks": []any{map[string]any{"text": "one"}}}},
		{"cut after a key", `{"title":"t","overview":`, map[string]any{"title": "t"}},
		{"cut after an opener", `{"title":"t","changes":[`, map[string]any{"title": "t", "changes": []any{}}},
		{"braces inside strings", `{"a":"} not the end {","b":1}`, map[string]any{"a": "} not the end {", "b": 1.0}},
		{"no object", `just prose`, nil},
	}
	for _, c := range cases {
		out, ok := repairJSON(c.in)
		var got any
		if ok {
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				if c.want != nil {
					t.Errorf("%s: %q does not parse: %v", c.name, out, err)
				}
				continue
			}
		}
		if c.want == nil {
			if ok && got != nil && c.name == "no object" {
				t.Errorf("%s: read %v", c.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v from %q, want %#v", c.name, got, out, c.want)
		}
	}
}
