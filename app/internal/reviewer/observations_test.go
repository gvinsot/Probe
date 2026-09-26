package reviewer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// The provider receives the observation recipe and its limits in the system
// prompt, right after the base prompt, with or without intent criteria.
func TestSystemPromptCarriesTheObservationRecipe(t *testing.T) {
	for _, criteria := range [][]model.IntentCriterion{nil, {{ID: "AC-1", Text: "Totals never go negative", Line: 1}}} {
		server, bodies := scripted(t)
		r := &model.Report{IntentCriteria: criteria}
		if err := Run(context.Background(), Options{Endpoint: server.URL, Model: "test"}, r, &fakeHarness{}); err != nil {
			t.Fatal(err)
		}
		system := (*bodies)[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.HasPrefix(system, systemPrompt+observationPrompt) {
			t.Fatal("the observation prompt does not follow the base prompt")
		}
		for _, fragment := range []string{
			`t.Attr("swiftproof.<input>"`,
			"(task.meta as any).swiftproof",
			"Jest cannot record observations",
			"differential_observation",
			"not which revision is correct",
			"A NOT_DIVERGED record supports no hypothesis status",
		} {
			if !strings.Contains(system, fragment) {
				t.Errorf("system prompt lacks %q", fragment)
			}
		}
		tools, _ := json.Marshal((*bodies)[0]["tools"])
		if !strings.Contains(string(tools), "task.meta.swiftproof") {
			t.Error("the run_generated_test description does not reach the provider")
		}
	}
}

// The prompt never presents a divergence as a defect, or equal values as
// equivalence or approval.
func TestObservationPromptWording(t *testing.T) {
	lower := strings.ToLower(observationPrompt)
	for _, word := range []string{"regression", "bug", "equivalen", "safe", "verified", "tested", "approv"} {
		if strings.Contains(lower, word) {
			t.Errorf("prompt uses %q", word)
		}
	}
}
