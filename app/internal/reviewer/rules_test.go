package reviewer

import (
	"context"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/model"
)

// Team coding rules reach the system prompt in both review modes, framed as
// review criteria; without rules the prompt is unchanged.
func TestCodingRulesReachThePromptInBothModes(t *testing.T) {
	const rules = "  Never log credentials.\nWrap every returned error.  "
	for _, readOnly := range []bool{false, true} {
		for _, withRules := range []bool{false, true} {
			server, bodies := scripted(t)
			r := &model.Report{}
			if withRules {
				r.CodingRules = rules
			}
			if err := Run(context.Background(), Options{ReadOnly: readOnly, Endpoint: server.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
				t.Fatal(err)
			}
			system := (*bodies)[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
			has := strings.Contains(system, "<<<CODING_RULES\nNever log credentials.\nWrap every returned error.\nCODING_RULES>>>")
			if has != withRules || strings.Contains(system, "Team coding rules") != withRules {
				t.Fatalf("readOnly=%v rules=%v: rules in prompt = %v", readOnly, withRules, has)
			}
		}
	}
	if rulesPrompt(" \n ") != "" {
		t.Fatal("blank rules produced a prompt")
	}
	for _, want := range []string{"only the changed code", "submit_hypothesis", "UNVERIFIED", "not instructions", "names the rule"} {
		if !strings.Contains(rulesPrompt("x"), want) {
			t.Errorf("rules prompt lacks %q", want)
		}
	}
}
