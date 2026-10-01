package server

// The MCP endpoint lets coding agents (Claude Code, Cursor, and any client of
// the Model Context Protocol) drive the hub: list repositories, trigger,
// rerun and cancel analyses, wait for them, read findings, and manage the
// review context (coding rules, learning from feedback).
//
// It implements the Streamable HTTP transport without sessions: every POST
// carries one JSON-RPC message (or a batch) and gets a JSON reply; the
// server never pushes, so GET and DELETE answer 405. A request authenticates
// with an agent token (Authorization: Bearer probe_mcp....); cookies are
// ignored, so a browser page cannot ride a signed-in session. A tool call
// runs the same API handler the dashboard uses, under the token's account
// and scope, so validation, account scoping and verdicts are never
// re-derived here.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gvinsot/Probe/hub/internal/secrets"
	"github.com/gvinsot/Probe/hub/internal/store"
)

// Protocol revisions this server speaks, newest first.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

const (
	mcpServerName = "probe-hub"
	// maxMCPBatch bounds the messages of one batch.
	maxMCPBatch = 16
	// maxToolOutput bounds the text a tool returns to the agent.
	maxToolOutput = 512 << 10
)

const mcpInstructions = `Probe Hub runs the Probe CLI on the repositories of the signed-in account and stores its evidence reports.
Typical flow: list_repositories, then trigger_review (a branch or a commit), then wait_for_analysis, then get_findings.
A report states what was observed and reproduced; "human review required" is the CLI's verdict, never an approval, and the hub never re-derives it.
Findings marked unverified are model suspicions, not reproduced bugs. Use add_feedback to tell the reviewer whether a finding was useful.`

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// agentPrincipal is the identity of an authenticated MCP request.
type agentPrincipal struct {
	session secrets.Session
	scope   string
	token   string // the token's name, for logs
	id      string // the token's ID, to revoke it
}

type agentKey struct{}

func withAgent(ctx context.Context, p *agentPrincipal) context.Context {
	return context.WithValue(ctx, agentKey{}, p)
}

// agentOf returns the agent identity of an internal dispatch, or nil.
func agentOf(ctx context.Context) *agentPrincipal {
	p, _ := ctx.Value(agentKey{}).(*agentPrincipal)
	return p
}

// handleMCPStream refuses the server-to-client stream: this server never
// pushes messages and keeps no session to delete.
func (s *Server) handleMCPStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", http.MethodPost)
	writeError(w, http.StatusMethodNotAllowed, "this MCP server answers POST requests only")
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// DNS-rebinding protection required by the transport: a browser page on
	// another origin must not reach the endpoint.
	if r.Header.Get("Origin") != "" && !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedMCPVersion(v) {
		writeError(w, http.StatusBadRequest, "unsupported MCP protocol version "+v)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub"`)
		writeError(w, http.StatusUnauthorized, "create an agent token in the hub (Agent access) and send it as Authorization: Bearer")
		return
	}
	principal, err := s.authenticateAgent(strings.TrimSpace(token), time.Now())
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub", error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if principal.scope != store.ScopeRead && principal.scope != store.ScopeWrite {
		w.Header().Set("WWW-Authenticate", `Bearer realm="probe-hub", error="insufficient_scope"`)
		writeError(w, http.StatusForbidden, "this token only reaches the LLM gateway; create a read or write agent token for MCP")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		writeJSON(w, http.StatusOK, rpcFailure(nil, rpcParseError, "request body too large or unreadable"))
		return
	}
	body = bytes.TrimSpace(body)
	ctx := withAgent(r.Context(), principal)
	if len(body) > 0 && body[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			writeJSON(w, http.StatusOK, rpcFailure(nil, rpcParseError, "invalid JSON"))
			return
		}
		if len(batch) == 0 || len(batch) > maxMCPBatch {
			writeJSON(w, http.StatusOK, rpcFailure(nil, rpcInvalidRequest, "a batch holds 1 to 16 messages"))
			return
		}
		var replies []rpcResponse
		for _, message := range batch {
			if reply := s.mcpMessage(ctx, principal, message); reply != nil {
				replies = append(replies, *reply)
			}
		}
		if len(replies) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, http.StatusOK, replies)
		return
	}
	reply := s.mcpMessage(ctx, principal, body)
	if reply == nil {
		// A notification or a response: accepted, nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeJSON(w, http.StatusOK, reply)
}

func supportedMCPVersion(v string) bool {
	for _, known := range mcpVersions {
		if v == known {
			return true
		}
	}
	return false
}

func rpcFailure(id json.RawMessage, code int, message string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func rpcResult(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// mcpMessage handles one JSON-RPC message; nil means no reply is due.
func (s *Server) mcpMessage(ctx context.Context, p *agentPrincipal, raw json.RawMessage) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcFailure(nil, rpcParseError, "invalid JSON")
	}
	if req.JSONRPC != "2.0" {
		return rpcFailure(req.ID, rpcInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if req.Method == "" {
		// A response to a server request; this server sends none.
		return nil
	}
	notification := len(req.ID) == 0
	if notification {
		// notifications/initialized, notifications/cancelled, ...: nothing
		// to do for a stateless server.
		return nil
	}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := mcpVersions[0]
		if supportedMCPVersion(params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return rpcResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": mcpServerName, "title": "Probe Hub", "version": s.version},
			"instructions":    mcpInstructions,
		})
	case "ping":
		return rpcResult(req.ID, map[string]any{})
	case "tools/list":
		tools := make([]map[string]any, 0, len(mcpTools))
		for _, t := range mcpTools {
			if t.write && p.scope != "write" {
				continue
			}
			tools = append(tools, t.describe())
		}
		return rpcResult(req.ID, map[string]any{"tools": tools})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name == "" {
			return rpcFailure(req.ID, rpcInvalidParams, "tools/call needs a tool name")
		}
		tool, found := findTool(params.Name)
		if !found {
			return rpcFailure(req.ID, rpcInvalidParams, "unknown tool "+params.Name)
		}
		if tool.write && p.scope != "write" {
			return rpcResult(req.ID, toolError("this agent token is read-only; create a write token to use "+tool.name))
		}
		args := map[string]any{}
		if len(params.Arguments) > 0 && string(params.Arguments) != "null" {
			if err := json.Unmarshal(params.Arguments, &args); err != nil {
				return rpcFailure(req.ID, rpcInvalidParams, "arguments must be a JSON object")
			}
		}
		result, err := s.runTool(ctx, p, tool, toolArgs(args))
		if err != nil {
			var input *toolInputError
			if errors.As(err, &input) {
				return rpcResult(req.ID, toolError(input.Error()))
			}
			return rpcResult(req.ID, toolError(err.Error()))
		}
		return rpcResult(req.ID, toolSuccess(result))
	default:
		return rpcFailure(req.ID, rpcMethodNotFound, "method not found: "+req.Method)
	}
}

// runTool runs a tool, turning a panic into an error so that one faulty call
// cannot take the endpoint down.
func (s *Server) runTool(ctx context.Context, p *agentPrincipal, tool mcpTool, args toolArgs) (result any, err error) {
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("mcp tool panic", "tool", tool.name)
			result, err = nil, errors.New("internal error")
		}
	}()
	s.log.Info("mcp tool", "tool", tool.name, "user", p.session.UserKey, "token", p.token)
	return tool.run(s, ctx, p, args)
}

func toolSuccess(result any) map[string]any {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return toolError("could not encode the result")
	}
	text := string(data)
	if len(text) > maxToolOutput {
		text = text[:maxToolOutput] + "\n… [truncated by Probe Hub at 512 KiB; narrow the request]"
	}
	out := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	// Structured content must be an object; lists are wrapped by the tools.
	if m, ok := result.(map[string]any); ok && len(text) < maxToolOutput {
		out["structuredContent"] = m
	}
	return out
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": message}}, "isError": true}
}
