package reviewer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

func explainAnthropic(ctx context.Context, req Request) (string, error) {
	opts := []option.RequestOption{option.WithAPIKey(req.APIKey)}
	if req.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(req.BaseURL))
	}
	client := anthropic.NewClient(opts...)
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: 16000,
		System:    []anthropic.BetaTextBlockParam{{Text: req.System}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(req.User)),
		},
	}
	if supportsEffort(req.Model) {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffortMedium}
	}
	if supportsFallback(req.Model) {
		// A request declined by a safety classifier is served again by the
		// model the API picks for that category, within the same call.
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}
	resp, err := client.Beta.Messages.New(ctx, params)
	if err != nil {
		var apiErr *anthropic.Error
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case 401, 403:
				return "", fmt.Errorf("Anthropic rejected the API key (%d)", apiErr.StatusCode)
			case 404:
				return "", fmt.Errorf("model %q is not available for this API key", req.Model)
			case 429:
				return "", errors.New("Anthropic rate limit reached, try again in a moment")
			}
		}
		return "", fmt.Errorf("Anthropic: %w", err)
	}
	if resp.StopReason == anthropic.BetaStopReasonRefusal {
		return "", errors.New("the model declined to explain this report")
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	if strings.TrimSpace(text.String()) == "" {
		return "", errors.New("the model returned no text")
	}
	return strings.TrimSpace(text.String()), nil
}

// supportsEffort lists the model families that accept output_config.effort.
func supportsEffort(model string) bool {
	for _, p := range []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-mythos-5", "claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// supportsFallback lists the models accepting the "default" fallback mode.
func supportsFallback(model string) bool {
	switch model {
	case "claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-sonnet-5-5":
		return true
	}
	return false
}
