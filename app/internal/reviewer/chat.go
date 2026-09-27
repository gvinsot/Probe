package reviewer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gvinsot/SwiftProof/app/internal/harness"
	"github.com/gvinsot/SwiftProof/app/internal/model"
)

// errInputBudget reports a request larger than Options.MaxInputBytes; it is
// never sent.
var errInputBudget = errors.New("reviewer input budget exhausted")

// chat is one bounded Chat Completions session: no proxy, no redirect,
// credentials sent to the configured endpoint only, and every response size
// bounded. It is shared by the investigation (Run) and the planner (Plan).
type chat struct {
	o         Options
	endpoint  string
	transport *http.Transport
	client    *http.Client
}

func newChat(o Options, endpoint string) *chat {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}
	client := &http.Client{Transport: transport, Timeout: o.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("reviewer redirects are prohibited") }}
	return &chat{o: o, endpoint: endpoint, transport: transport, client: client}
}

func (c *chat) close() { c.transport.CloseIdleConnections() }

// clean redacts credential shapes and the API key from text sent to or read
// from the provider.
func (c *chat) clean(s string) string {
	s = harness.Redact(s)
	if c.o.APIKey != "" {
		s = strings.ReplaceAll(s, c.o.APIKey, "[REDACTED]")
	}
	return s
}

type completionChoice struct {
	Message      message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// complete sends one request and returns the single choice, with the audit
// event of the call (status OK or ERROR). errInputBudget is returned, with no
// event worth recording, when the request exceeds the input budget. The
// returned message is redacted and has the assistant role.
func (c *chat) complete(ctx context.Context, messages []message, tools []map[string]any, iteration int) (completionChoice, model.AuditEvent, error) {
	body, err := json.Marshal(struct {
		Model               string           `json:"model"`
		Messages            []message        `json:"messages"`
		Tools               []map[string]any `json:"tools"`
		MaxCompletionTokens int              `json:"max_completion_tokens"`
		ParallelToolCalls   bool             `json:"parallel_tool_calls"`
	}{c.o.Model, messages, tools, 4096, false})
	if err != nil {
		return completionChoice{}, model.AuditEvent{}, err
	}
	if len(body) > c.o.MaxInputBytes {
		return completionChoice{}, model.AuditEvent{}, errInputBudget
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return completionChoice{}, model.AuditEvent{}, errors.New("cannot construct reviewer request")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.o.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.o.APIKey)
	}
	started := time.Now()
	res, err := c.client.Do(req)
	event := model.AuditEvent{Time: started.UTC(), Tool: "reviewer_completion", Arguments: fmt.Sprintf("iteration=%d", iteration+1), Status: "ERROR", DurationMS: time.Since(started).Milliseconds()}
	if err != nil {
		return completionChoice{}, event, errors.New("reviewer request failed (transport, timeout, or prohibited redirect)")
	}
	data, readErr := io.ReadAll(io.LimitReader(res.Body, 1024*1024+1))
	res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return completionChoice{}, event, fmt.Errorf("reviewer endpoint returned HTTP %d", res.StatusCode)
	}
	if readErr != nil || len(data) > 1024*1024 {
		return completionChoice{}, event, errors.New("reviewer response exceeded size limit or could not be read")
	}
	var response struct {
		Choices []completionChoice `json:"choices"`
	}
	if err := json.Unmarshal(data, &response); err != nil || len(response.Choices) != 1 {
		return completionChoice{}, event, errors.New("reviewer returned an invalid completion")
	}
	event.Status = "OK"
	event.DurationMS = time.Since(started).Milliseconds()
	choice := response.Choices[0]
	choice.Message.Role = "assistant"
	choice.Message.ToolCallID = ""
	choice.Message.Content = c.clean(choice.Message.Content)
	return choice, event, nil
}
