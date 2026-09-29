package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// explainOpenAI calls the chat completions endpoint of OpenAI, or of a
// compatible server when a base URL is set (vLLM, Ollama… run on premises).
func explainOpenAI(ctx context.Context, req Request) (string, error) {
	base := req.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	body, err := json.Marshal(map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
	})
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("OpenAI endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("OpenAI endpoint answered %s with an unexpected body", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return "", errors.New("OpenAI rejected the API key")
		}
		msg := resp.Status
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		return "", fmt.Errorf("OpenAI: %s", msg)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the model returned no answer")
	}
	if r := out.Choices[0].Message.Refusal; r != "" {
		return "", fmt.Errorf("the model declined: %s", r)
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("the model returned no text")
	}
	return text, nil
}
