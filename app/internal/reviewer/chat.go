package reviewer

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gvinsot/Probe/app/internal/harness"
	"github.com/gvinsot/Probe/app/internal/model"
	"github.com/gvinsot/Probe/app/internal/redact"
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
	maxTokens int // max_completion_tokens; 0: 4096
}

// newChat builds the session's client. The response-header wait is the whole
// reviewer timeout: a non-streaming completion sends its headers only once the
// answer is generated, which a shared endpoint can take minutes to do.
func newChat(o Options, endpoint string) *chat {
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: o.Timeout, MaxIdleConns: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: 30 * time.Second}
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

// requestError describes a failed request without echoing the transport error,
// which can hold the endpoint URL: a timeout says how long the endpoint was
// waited for, anything else is a transport failure or a prohibited redirect.
func requestError(err error, elapsed, limit time.Duration) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("reviewer endpoint gave no response within %s (reviewer timeout %s); the model may be slow or overloaded", elapsed.Round(time.Second), limit)
	}
	return errors.New("reviewer request failed (transport error or prohibited redirect)")
}

// toolCallIDs gives every tool call of a session a distinct ID. Providers
// behind OpenAI-compatible gateways omit the ID or the type, or restart their
// numbering (call_0) at every turn; each result must still answer its own
// call, so an absent, oversized or already used ID is replaced by a local one
// before the message joins the conversation.
type toolCallIDs struct {
	used map[string]bool
	next int
}

// normalize rewrites the tool calls of m in place. A call whose type is set
// to anything but function is an error.
func (ids *toolCallIDs) normalize(m *message) error {
	if ids.used == nil {
		ids.used = map[string]bool{}
	}
	for i := range m.ToolCalls {
		call := &m.ToolCalls[i]
		if call.Type == "" {
			call.Type = "function"
		}
		if call.Type != "function" {
			return fmt.Errorf("tool call of unsupported type %q", redact.TruncateUTF8(call.Type, 40))
		}
		for call.ID == "" || len(call.ID) > 200 || ids.used[call.ID] {
			ids.next++
			call.ID = fmt.Sprintf("probe_call_%d", ids.next)
		}
		ids.used[call.ID] = true
	}
	return nil
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
	// A request without tools omits both tool fields: providers reject
	// parallel_tool_calls, and some an empty tools array, when no tool is
	// offered.
	var parallel *bool
	if len(tools) > 0 {
		parallel = new(bool)
	}
	body, err := json.Marshal(struct {
		Model               string           `json:"model"`
		Messages            []message        `json:"messages"`
		Tools               []map[string]any `json:"tools,omitempty"`
		MaxCompletionTokens int              `json:"max_completion_tokens"`
		ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
	}{c.o.Model, messages, tools, cmp.Or(c.maxTokens, 4096), parallel})
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
		return completionChoice{}, event, requestError(err, time.Since(started), c.o.Timeout)
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
