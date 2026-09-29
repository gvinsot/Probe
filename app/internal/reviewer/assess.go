package reviewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gvinsot/Probe/app/internal/model"
)

// AssessTool records the model's plain-language reading of linter signals.
// It is local: it runs nothing and records a model judgment, never evidence.
const AssessTool = "assess_signals"

// Bounds of one assess_signals call and of each assessment.
const (
	maxAssessmentsPerCall  = 12
	maxAssessmentTitle     = 160
	maxAssessmentText      = 1200
	maxAssessmentRationale = 2000
	maxAssessmentEvidence  = 8
)

// assessPrompt is appended to the system prompt when the run has linter
// signals.
const assessPrompt = `
Linter signals: the input lists deterministic linter signals (field "signals", each with an "id"). Their summaries are terse and generic. Read each one in the context of this change and record your reading with assess_signals, at most 12 signals per call, as early as you can:
- title: a short, specific, plain-language title saying what the signal means for THIS change (for example "Retry loop no longer stops after 3 attempts" rather than "control flow changed");
- explanation: one to three plain sentences for a reviewer who has not read the code: what changed, and what could go wrong or why nothing can;
- judgment: "risk" when the signal points at a plausible problem, "no_risk" when the source shows it is harmless (a rename, a comment, a test-only or formatting change, a dependency bump with no behavioral effect), otherwise "uncertain";
- "no_risk" requires a rationale and the evidence_id of a read_file observation of the relevant lines; without them it is recorded as "uncertain".
Assessments are model judgment, never evidence, and change no hypothesis status. A concrete defect you find, whether or not a signal points at it, must still be submitted with submit_hypothesis.`

// assessTool is the assess_signals definition.
func assessTool() map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	item := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"signal_id":    str("The id of a signal from the input"),
			"title":        str("Short plain-language title specific to this change"),
			"explanation":  str("One to three plain sentences for a reviewer"),
			"judgment":     map[string]any{"type": "string", "enum": []string{model.AssessmentRisk, model.AssessmentNoRisk, model.AssessmentUncertain}},
			"rationale":    str("Why; required for no_risk"),
			"evidence_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "read_file evidence IDs; required for no_risk"},
		},
		"required": []string{"signal_id", "title", "explanation", "judgment"},
	}
	return map[string]any{"type": "function", "function": map[string]any{
		"name":        AssessTool,
		"description": "Record your plain-language reading of up to 12 linter signals. A later assessment of the same signal replaces the earlier one. no_risk is kept only with a rationale citing a read_file evidence ID.",
		"parameters": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties":           map[string]any{"assessments": map[string]any{"type": "array", "minItems": 1, "maxItems": maxAssessmentsPerCall, "items": item}},
			"required":             []string{"assessments"},
		},
	}}
}

// assess records the valid assessments of one call and reports each rejected
// one to the model, so that it can correct it. Finalize validates the cited
// evidence once the harness records are synchronized.
func assess(r *model.Report, data []byte) (json.RawMessage, error) {
	var call struct {
		Assessments []model.SignalAssessment `json:"assessments"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&call); err != nil {
		return nil, errors.New("assessments must match the structured schema")
	}
	if len(call.Assessments) == 0 || len(call.Assessments) > maxAssessmentsPerCall {
		return nil, fmt.Errorf("submit between 1 and %d assessments per call", maxAssessmentsPerCall)
	}
	signals := make(map[string]bool, len(r.Signals))
	for _, s := range r.Signals {
		signals[s.ID] = true
	}
	var recorded []string
	rejected := map[string]string{}
	for i, a := range call.Assessments {
		a.Title, a.Explanation, a.Rationale = strings.TrimSpace(a.Title), strings.TrimSpace(a.Explanation), strings.TrimSpace(a.Rationale)
		a.Judgment = strings.ToLower(strings.TrimSpace(a.Judgment))
		if a.EvidenceIDs == nil {
			a.EvidenceIDs = []string{}
		}
		key := a.SignalID
		if key == "" || len(key) > 200 {
			key = fmt.Sprintf("assessment %d", i+1)
		}
		if reason := assessmentProblem(a, signals); reason != "" {
			rejected[key] = reason
			continue
		}
		replaced := false
		for j := range r.SignalAssessments {
			if r.SignalAssessments[j].SignalID == a.SignalID {
				r.SignalAssessments[j], replaced = a, true
				break
			}
		}
		if !replaced {
			r.SignalAssessments = append(r.SignalAssessments, a)
		}
		recorded = append(recorded, a.SignalID)
	}
	return json.Marshal(map[string]any{"recorded": recorded, "rejected": rejected, "status": "recorded_pending_evidence_validation"})
}

// assessmentProblem returns why an assessment cannot be recorded, or "".
func assessmentProblem(a model.SignalAssessment, signals map[string]bool) string {
	switch {
	case !signals[a.SignalID]:
		return "unknown signal_id"
	case a.Title == "" || len(a.Title) > maxAssessmentTitle:
		return fmt.Sprintf("title is required and must be at most %d bytes", maxAssessmentTitle)
	case a.Explanation == "" || len(a.Explanation) > maxAssessmentText:
		return fmt.Sprintf("explanation is required and must be at most %d bytes", maxAssessmentText)
	case len(a.Rationale) > maxAssessmentRationale:
		return fmt.Sprintf("rationale must be at most %d bytes", maxAssessmentRationale)
	case len(a.EvidenceIDs) > maxAssessmentEvidence:
		return fmt.Sprintf("at most %d evidence IDs", maxAssessmentEvidence)
	}
	switch a.Judgment {
	case model.AssessmentRisk, model.AssessmentNoRisk, model.AssessmentUncertain:
	default:
		return "judgment must be risk, no_risk or uncertain"
	}
	for _, id := range a.EvidenceIDs {
		if id == "" || len(id) > 200 {
			return "invalid evidence ID"
		}
	}
	if a.Judgment == model.AssessmentNoRisk && (a.Rationale == "" || len(a.EvidenceIDs) == 0) {
		return "no_risk requires a rationale and a read_file evidence ID; use uncertain otherwise"
	}
	return ""
}
