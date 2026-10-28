package harness

import (
	stdcontext "context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/domain"
)

// MCPServer describes the per-run loopback MCP server handed to a backend.
type MCPServer struct {
	Name    string
	URL     string
	Headers map[string]string
}

// Context is the immutable conversation context bound to one harness run.
type Context struct {
	Backend         string
	Model           string
	PublicModel     string
	Effort          string
	ClientKey       string
	SystemPrompt    string
	ModelSpecified  bool
	EffortSpecified bool
	SystemSpecified bool
	Prompt          string
	Attachments     []json.RawMessage
	Key             json.RawMessage
}

// RunHooks are the per-run wiring passed to the backend runner.
type RunHooks struct {
	MCPConfig   string
	MCPServer   MCPServer
	WorkDir     string
	Instruction string
	Sink        domain.Sink
}

// RunFunc runs the official CLI for one harness run.
type RunFunc func(ctx stdcontext.Context, context Context, hooks RunHooks) (domain.Output, error)

// Lease is the compute-slot pool shared with ordinary turns. A run holds a
// lease only while the CLI is computing, never while it waits for results.
type Lease interface {
	Acquire(ctx stdcontext.Context) error
	Release()
}

// ToolCall is one call handed to the API client for local execution.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// SegmentOutput is one sealed response segment of a harness run.
type SegmentOutput struct {
	Text      string
	Thinking  string
	ToolCalls []ToolCall
	Usage     *domain.Usage
}

type subscription struct {
	ctx  stdcontext.Context
	sink domain.Sink
}

type segment struct {
	id     string
	policy Policy

	mu        sync.Mutex
	events    []domain.Event
	subs      map[*subscription]struct{}
	calls     []ToolCall
	sealed    bool
	delivered bool
	done      chan struct{}
	out       SegmentOutput
	err       error
}

func (s *segment) appendEvent(ev domain.Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return false
	}
	s.events = append(s.events, ev)
	for sub := range s.subs {
		if sub.ctx.Err() == nil {
			sub.sink.Emit(ev.Kind, ev.Value)
		}
	}
	return true
}

func (s *segment) queue(events []domain.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, events...)
}

func (s *segment) seal(calls []ToolCall, usage *domain.Usage) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return false
	}
	s.sealed = true
	s.calls = calls
	var text, thinking strings.Builder
	for _, ev := range s.events {
		switch ev.Kind {
		case domain.EventText:
			text.WriteString(ev.Value)
		case domain.EventThinking:
			thinking.WriteString(ev.Value)
		}
	}
	s.out = SegmentOutput{Text: text.String(), Thinking: thinking.String(), ToolCalls: calls, Usage: usage}
	close(s.done)
	return true
}

func (s *segment) reject(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return
	}
	s.sealed = true
	s.err = err
	close(s.done)
}

func (s *segment) isSealed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sealed
}

func (s *segment) isDelivered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.delivered
}

func (s *segment) currentText() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev.Kind == domain.EventText {
			return true
		}
	}
	return false
}

// Turn is the API handler's handle on one response segment.
type Turn struct {
	ID    string
	RunID string
	Model string
	seg   *segment
}

// Wait follows the segment until it seals or the client goes away. The run
// itself continues either way.
func (t *Turn) Wait(ctx stdcontext.Context, sink domain.Sink) (SegmentOutput, error) {
	sub := &subscription{ctx: ctx, sink: sink}
	t.seg.mu.Lock()
	for _, ev := range t.seg.events {
		if ctx.Err() == nil {
			sink.Emit(ev.Kind, ev.Value)
		}
	}
	t.seg.subs[sub] = struct{}{}
	t.seg.mu.Unlock()
	delivered := false
	defer func() {
		t.seg.mu.Lock()
		delete(t.seg.subs, sub)
		if delivered {
			t.seg.delivered = true
		}
		t.seg.mu.Unlock()
	}()
	select {
	case <-t.seg.done:
		t.seg.mu.Lock()
		out, err := t.seg.out, t.seg.err
		t.seg.mu.Unlock()
		delivered = ctx.Err() == nil
		return out, err
	case <-ctx.Done():
		return SegmentOutput{}, ctx.Err()
	}
}

type mcpResult struct {
	content []json.RawMessage
	isError bool
}

type pendingCall struct {
	id     string
	name   string
	args   json.RawMessage
	result chan mcpResult
}

func (c *pendingCall) respond(r mcpResult) {
	select {
	case c.result <- r:
	default:
	}
}

type receipt struct {
	valueHash string
	segment   *segment
}

type run struct {
	id       string
	key      string
	protocol string
	tools    []Tool
	digest   string
	context  Context

	ctx    stdcontext.Context
	cancel stdcontext.CancelCauseFunc

	mu           sync.Mutex
	sequence     int
	pending      map[string]*pendingCall
	callIds      map[string]bool
	queue        []*pendingCall
	queuedEvents []domain.Event
	receipts     map[string]receipt
	current      *segment
	waiting      *segment
	first        *segment
	done         bool
	finishedAt   time.Time
	outputBytes  int
	invalidCalls int
	resuming     bool
	hasLease     bool
	batchTimer   *time.Timer
	waitTimer    *time.Timer

	dir      string
	server   *http.Server
	listener net.Listener
	workDone chan struct{}
}

func (r *run) isDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.done
}

func (r *run) currentPolicy() Policy {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current.policy
}

// Gateway owns every in-flight harness run.
type Gateway struct {
	RunCLI         RunFunc
	Lease          Lease
	Timeout        time.Duration
	Cache          time.Duration
	MaxRuns        int
	MaxOutputBytes int
	Logf           func(format string, args ...any)

	mu      sync.Mutex
	runs    map[string]*run
	calls   map[string]*run
	initial map[string]*run
}

// NewGateway creates a gateway with production defaults for zero fields.
func NewGateway(runCLI RunFunc, lease Lease) *Gateway {
	return &Gateway{
		RunCLI: runCLI, Lease: lease,
		Timeout: 300 * time.Second, Cache: 600 * time.Second,
		MaxRuns: 16, MaxOutputBytes: 4_000_000,
		runs: map[string]*run{}, calls: map[string]*run{}, initial: map[string]*run{},
	}
}

// Stats summarizes live harness runs for the health endpoint.
func (g *Gateway) Stats() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	active, pending := 0, 0
	for _, r := range g.runs {
		r.mu.Lock()
		if !r.done {
			active++
		}
		pending += len(r.pending)
		r.mu.Unlock()
	}
	return map[string]any{"active": active, "pending_tools": pending}
}

func (g *Gateway) forgetLocked(r *run) {
	delete(g.runs, r.id)
	if g.initial[r.key] == r {
		delete(g.initial, r.key)
	}
	r.mu.Lock()
	ids := make([]string, 0, len(r.callIds))
	for id := range r.callIds {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		delete(g.calls, id)
	}
}

func (g *Gateway) sweepLocked() {
	now := time.Now()
	for _, r := range g.runs {
		r.mu.Lock()
		done, at := r.done, r.finishedAt
		r.mu.Unlock()
		if done && now.Sub(at) > g.Cache {
			g.forgetLocked(r)
		}
	}
	for len(g.runs) >= g.MaxRuns {
		var oldest *run
		for _, r := range g.runs {
			r.mu.Lock()
			done, at := r.done, r.finishedAt
			r.mu.Unlock()
			if done && (oldest == nil || at.Before(oldest.finishedAt)) {
				oldest = r
			}
		}
		if oldest == nil {
			break
		}
		g.forgetLocked(oldest)
	}
}

func newSegment(r *run, spec *Spec) *segment {
	seg := &segment{
		id: fmt.Sprintf("%s_%d", r.id, r.sequence), policy: spec.Policy,
		subs: map[*subscription]struct{}{}, done: make(chan struct{}),
	}
	r.sequence++
	return seg
}

// Prepare validates a request and attaches it to a run. All continuation
// validation happens before even one MCP result is released.
func (g *Gateway) Prepare(spec *Spec, context Context) (*Turn, error) {
	g.mu.Lock()
	g.sweepLocked()
	var r *run
	var current *segment
	var startContinue []Result
	if len(spec.Results) > 0 {
		r = g.calls[spec.Results[0].ID]
		if r == nil {
			g.mu.Unlock()
			return nil, typedToolError(410, "Tool call state is unavailable; do not repeat side effects automatically", "tool_state_expired")
		}
		for _, result := range spec.Results {
			if g.calls[result.ID] != r {
				g.mu.Unlock()
				return nil, typedToolError(410, "Tool call state is unavailable; do not repeat side effects automatically", "tool_state_expired")
			}
		}
		fail := func(err error) (*Turn, error) { g.mu.Unlock(); return nil, err }
		if r.protocol != spec.Protocol {
			return fail(typedToolError(409, "Do not change API protocol during a tool cycle", "tool_state_conflict"))
		}
		if context.ClientKey != "" && r.context.ClientKey != "" && context.ClientKey != r.context.ClientKey {
			return fail(typedToolError(409, "Tool results belong to a different conversation", "tool_state_conflict"))
		}
		if context.ModelSpecified && (context.Model != r.context.Model || context.Backend != r.context.Backend) {
			return fail(typedToolError(409, "Cannot change models while tool calls are pending", "tool_state_conflict"))
		}
		if context.EffortSpecified && context.Effort != r.context.Effort {
			return fail(typedToolError(409, "Cannot change reasoning effort during a tool cycle", "tool_state_conflict"))
		}
		if context.SystemSpecified && context.SystemPrompt != r.context.SystemPrompt {
			return fail(typedToolError(409, "Cannot change system instructions during a tool cycle", "tool_state_conflict"))
		}
		if spec.HasTools && toolDigest(spec.Tools) != r.digest {
			return fail(typedToolError(409, "Cannot change tool definitions during a tool cycle", "tool_state_conflict"))
		}
		if err := ValidateChoice(spec, r.tools); err != nil {
			return fail(err)
		}
		ordered := append([]Result{}, spec.Results...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
		ids := make([]string, 0, len(ordered))
		for _, result := range ordered {
			ids = append(ids, result.ID)
		}
		receiptKey := digestJSON(ids)
		valueHash := digestJSON(map[string]any{"results": ordered, "choice": spec.Choice, "name": spec.Name, "parallel": spec.Parallel})
		r.mu.Lock()
		if previous, ok := r.receipts[receiptKey]; ok {
			if previous.valueHash != valueHash {
				r.mu.Unlock()
				return fail(typedToolError(409, "Conflicting retry of a tool result", "tool_state_conflict"))
			}
			current = previous.segment
			r.mu.Unlock()
		} else {
			if r.done {
				r.mu.Unlock()
				return fail(typedToolError(410, "Tool call state has expired", "tool_state_expired"))
			}
			var expected []string
			if r.waiting != nil {
				r.waiting.mu.Lock()
				for _, c := range r.waiting.calls {
					expected = append(expected, c.ID)
				}
				r.waiting.mu.Unlock()
				sort.Strings(expected)
			}
			if digestJSON(expected) != digestJSON(ids) {
				r.mu.Unlock()
				return fail(toolError(400, "Supply exactly one result for every tool call in the last response"))
			}
			current = newSegment(r, spec)
			r.current = current
			r.waiting = nil
			if r.waitTimer != nil {
				r.waitTimer.Stop()
			}
			r.receipts[receiptKey] = receipt{valueHash: valueHash, segment: current}
			queued := r.queuedEvents
			r.queuedEvents = nil
			current.queue(queued)
			r.resuming = true
			startContinue = ordered
			r.mu.Unlock()
		}
		g.mu.Unlock()
		if startContinue != nil {
			go func() {
				if err := g.continueRun(r, startContinue); err != nil {
					g.fail(r, err)
				}
			}()
		}
	} else {
		tools := spec.Tools
		if err := ValidateChoice(spec, tools); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		key := digestJSON(map[string]any{
			"protocol": spec.Protocol, "context": context.Key, "tools": tools,
			"choice": spec.Choice, "name": spec.Name, "parallel": spec.Parallel,
		})
		if existing := g.initial[key]; existing != nil && (!existing.isDone() || !existing.first.isDelivered()) {
			r, current = existing, existing.first
			g.mu.Unlock()
		} else {
			if len(g.runs) >= g.MaxRuns {
				g.mu.Unlock()
				return nil, typedToolError(429, "Too many pending harness runs", "rate_limit_error")
			}
			compiled, err := CompileTools(tools)
			if err != nil {
				g.mu.Unlock()
				return nil, err
			}
			ctx, cancel := stdcontext.WithCancelCause(stdcontext.Background())
			r = &run{
				id: strings.ReplaceAll(domain.NewUUID(), "-", ""), key: key, protocol: spec.Protocol,
				tools: compiled, digest: toolDigest(tools), context: context,
				ctx: ctx, cancel: cancel,
				pending: map[string]*pendingCall{}, callIds: map[string]bool{}, receipts: map[string]receipt{},
				workDone: make(chan struct{}),
			}
			current = newSegment(r, spec)
			r.current, r.first = current, current
			g.runs[r.id] = r
			g.initial[key] = r
			g.mu.Unlock()
			go func() {
				defer close(r.workDone)
				if err := g.start(r); err != nil {
					g.fail(r, err)
				}
				g.cleanup(r)
			}()
		}
	}
	return &Turn{ID: current.id, RunID: r.id, Model: r.context.PublicModel, seg: current}, nil
}

func (g *Gateway) emit(r *run, ev domain.Event) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.outputBytes += len(ev.Value)
	if r.outputBytes > g.MaxOutputBytes {
		r.mu.Unlock()
		g.fail(r, typedToolError(502, "Tool cycle output limit exceeded", "tool_output_limit"))
		return
	}
	current := r.current
	r.mu.Unlock()
	if current.appendEvent(ev) {
		return
	}
	r.mu.Lock()
	r.queuedEvents = append(r.queuedEvents, ev)
	r.mu.Unlock()
}

func (g *Gateway) seal(r *run, calls []ToolCall, usage *domain.Usage) {
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if current.seal(calls, usage) && len(calls) > 0 {
		g.releaseLease(r)
	}
}

func (g *Gateway) scheduleCalls(r *run) {
	r.mu.Lock()
	if r.done || r.resuming || r.current.isSealed() || len(r.queue) == 0 || r.batchTimer != nil {
		r.mu.Unlock()
		return
	}
	policy := r.current.policy
	kept := r.queue[:0]
	var dropped []*pendingCall
	for _, call := range r.queue {
		if policy.Choice != "none" && (policy.Choice != "named" || call.name == policy.Name) {
			kept = append(kept, call)
		} else {
			delete(r.pending, call.id)
			dropped = append(dropped, call)
		}
	}
	r.queue = kept
	for _, call := range dropped {
		call.respond(mcpResult{isError: true, content: []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": "Tool is not available for this response"})}})
	}
	if len(r.queue) == 0 {
		r.mu.Unlock()
		return
	}
	r.batchTimer = time.AfterFunc(15*time.Millisecond, func() {
		r.mu.Lock()
		r.batchTimer = nil
		if r.done || r.current.isSealed() || len(r.queue) == 0 {
			r.mu.Unlock()
			return
		}
		limit := len(r.queue)
		if !r.current.policy.Parallel {
			limit = 1
		}
		batch := append([]*pendingCall{}, r.queue[:limit]...)
		r.queue = append([]*pendingCall{}, r.queue[limit:]...)
		r.waiting = r.current
		r.waitTimer = time.AfterFunc(g.Timeout, func() {
			g.fail(r, typedToolError(504, "Timed out waiting for tool results; do not repeat side effects automatically", "tool_result_timeout"))
		})
		r.mu.Unlock()
		calls := make([]ToolCall, 0, len(batch))
		for _, c := range batch {
			calls = append(calls, ToolCall{ID: c.id, Name: c.name, Arguments: c.args})
		}
		g.seal(r, calls, nil)
	})
	r.mu.Unlock()
}

func (g *Gateway) takeLease(r *run) error {
	r.mu.Lock()
	if r.hasLease {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	if g.Lease != nil {
		if err := g.Lease.Acquire(r.ctx); err != nil {
			if cause := stdcontext.Cause(r.ctx); cause != nil {
				return cause
			}
			return err
		}
	}
	r.mu.Lock()
	if r.done || r.ctx.Err() != nil {
		r.mu.Unlock()
		if g.Lease != nil {
			g.Lease.Release()
		}
		if cause := stdcontext.Cause(r.ctx); cause != nil {
			return cause
		}
		return typedToolError(503, "Tool cycle ended", "server_error")
	}
	r.hasLease = true
	r.mu.Unlock()
	return nil
}

func (g *Gateway) releaseLease(r *run) {
	r.mu.Lock()
	had := r.hasLease
	r.hasLease = false
	r.mu.Unlock()
	if had && g.Lease != nil {
		g.Lease.Release()
	}
}

func (g *Gateway) start(r *run) error {
	dir, err := os.MkdirTemp("", "komputer-tools-")
	if err != nil {
		return err
	}
	r.dir = dir
	token := domain.NewUUID() + domain.NewUUID()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	r.listener = listener
	r.server = &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			g.rpc(r, "Bearer "+token, w, req)
		}),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() { _ = r.server.Serve(listener) }()
	mcpServer := MCPServer{
		Name:    "komputer_use",
		URL:     "http://" + listener.Addr().String() + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + token},
	}
	mcpConfig := filepath.Join(dir, "tools.json")
	configJSON := mustRaw(map[string]any{"mcpServers": map[string]any{"komputer_use": map[string]any{
		"type": "http", "url": mcpServer.URL, "headers": mcpServer.Headers,
	}}})
	if err := os.WriteFile(mcpConfig, configJSON, 0o600); err != nil {
		return err
	}
	if err := r.ctx.Err(); err != nil {
		if cause := stdcontext.Cause(r.ctx); cause != nil {
			return cause
		}
		return err
	}
	if err := g.takeLease(r); err != nil {
		return err
	}
	policy := r.first.policy
	instruction := ""
	switch policy.Choice {
	case "none":
		instruction = "Do not call any tools."
	case "required":
		instruction = "Use at least one available tool before your first answer."
	case "named":
		for _, t := range r.tools {
			if t.Name == policy.Name {
				instruction = fmt.Sprintf("For your first tool call, use %s (%s).", t.Alias, t.Name)
			}
		}
	}
	out, err := g.RunCLI(r.ctx, r.context, RunHooks{
		MCPConfig: mcpConfig, MCPServer: mcpServer, WorkDir: dir, Instruction: instruction,
		Sink: func(ev domain.Event) { g.emit(r, ev) },
	})
	if r.isDone() {
		return nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	pending := len(r.pending)
	policy = r.current.policy
	r.mu.Unlock()
	if pending > 0 {
		return typedToolError(502, "The tool cycle ended before accepting results", "upstream_error")
	}
	if policy.Choice == "required" || policy.Choice == "named" {
		return typedToolError(502, "No tool call was produced for the required tool choice", "tool_choice_error")
	}
	r.mu.Lock()
	current := r.current
	r.mu.Unlock()
	if !current.currentText() && out.Text != "" {
		g.emit(r, domain.Event{Kind: domain.EventText, Value: out.Text})
	}
	g.seal(r, nil, out.Usage)
	r.mu.Lock()
	r.done = true
	r.finishedAt = time.Now()
	r.mu.Unlock()
	return nil
}

func writeRPCJSON(w http.ResponseWriter, status int, value any) {
	data, _ := json.Marshal(value)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (g *Gateway) rpc(r *run, token string, w http.ResponseWriter, req *http.Request) {
	auth := req.Header.Get("Authorization")
	if len(auth) != len(token) || subtle.ConstantTimeCompare([]byte(auth), []byte(token)) != 1 {
		writeRPCJSON(w, http.StatusForbidden, map[string]any{"error": "Forbidden"})
		return
	}
	expectedHost := r.listener.Addr().String()
	origin := req.Header.Get("Origin")
	if req.Host != expectedHost || (origin != "" && origin != "http://"+expectedHost) {
		writeRPCJSON(w, http.StatusForbidden, map[string]any{"error": "Forbidden origin"})
		return
	}
	if req.URL.Path != "/mcp" {
		writeRPCJSON(w, http.StatusNotFound, map[string]any{"error": "Not found"})
		return
	}
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1_000_001))
	if err != nil || len(body) > 1_000_000 {
		writeRPCJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "MCP request too large"})
		return
	}
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.JSONRPC != "2.0" {
		writeRPCJSON(w, http.StatusBadRequest, map[string]any{"error": "Invalid JSON-RPC request"})
		return
	}
	if len(msg.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	reply := func(result any) {
		writeRPCJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}
	rpcError := func(code int, message string) {
		writeRPCJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": code, "message": message}})
	}
	switch msg.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		version := "2025-06-18"
		for _, v := range []string{"2024-11-05", "2025-03-26", "2025-06-18"} {
			if params.ProtocolVersion == v {
				version = v
			}
		}
		reply(map[string]any{
			"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "komputer", "version": "0.10.0"},
		})
		return
	case "ping":
		reply(map[string]any{})
		return
	case "tools/list":
		tools := make([]any, 0, len(r.tools))
		for _, t := range r.tools {
			tools = append(tools, map[string]any{
				"name": t.Alias, "title": t.Name,
				"description": t.Name + ": " + t.Description, "inputSchema": t.Schema,
			})
		}
		reply(map[string]any{"tools": tools})
		return
	case "tools/call":
		// handled below
	default:
		rpcError(-32601, "Method not found")
		return
	}
	if r.isDone() {
		rpcError(-32000, "Tool cycle is no longer active")
		return
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(msg.Params, &params)
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	r.mu.Lock()
	policy := r.current.policy
	r.mu.Unlock()
	var tool *Tool
	for i := range r.tools {
		if r.tools[i].Alias == params.Name {
			tool = &r.tools[i]
			break
		}
	}
	var problem string
	switch {
	case tool == nil:
		problem = "Unknown tool"
	case policy.Choice == "none":
		problem = "Tools are disabled for this response; answer without calling tools"
	case policy.Choice == "named" && tool.Name != policy.Name:
		alias := ""
		for _, t := range r.tools {
			if t.Name == policy.Name {
				alias = t.Alias
			}
		}
		problem = fmt.Sprintf("Only %s may be called for this response", alias)
	case !tool.Validate(args):
		problem = "Arguments do not match the tool input schema; correct the arguments"
	}
	if problem != "" {
		reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": problem}}})
		r.mu.Lock()
		r.invalidCalls++
		tooMany := r.invalidCalls >= 8
		r.mu.Unlock()
		if tooMany {
			g.fail(r, typedToolError(502, "Too many invalid tool calls", "tool_choice_error"))
		}
		return
	}
	prefix := "call"
	if r.protocol == "anthropic" {
		prefix = "toolu"
	}
	call := &pendingCall{
		id:   prefix + "_" + strings.ReplaceAll(domain.NewUUID(), "-", ""),
		name: tool.Name, args: append(json.RawMessage{}, args...),
		result: make(chan mcpResult, 1),
	}
	r.mu.Lock()
	r.invalidCalls = 0
	r.pending[call.id] = call
	r.callIds[call.id] = true
	r.queue = append(r.queue, call)
	r.mu.Unlock()
	g.mu.Lock()
	g.calls[call.id] = r
	g.mu.Unlock()
	g.scheduleCalls(r)
	select {
	case result := <-call.result:
		out := map[string]any{"content": result.content}
		if result.isError {
			out["isError"] = true
		}
		reply(out)
	case <-req.Context().Done():
		r.mu.Lock()
		_, stillPending := r.pending[call.id]
		done := r.done
		r.mu.Unlock()
		if stillPending && !done {
			g.fail(r, typedToolError(502, "Tool transport disconnected", "upstream_error"))
		}
	}
}

func (g *Gateway) fail(r *run, err error) {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true
	r.finishedAt = time.Now()
	if r.batchTimer != nil {
		r.batchTimer.Stop()
	}
	if r.waitTimer != nil {
		r.waitTimer.Stop()
	}
	current := r.current
	pending := make([]*pendingCall, 0, len(r.pending))
	for _, c := range r.pending {
		pending = append(pending, c)
	}
	r.pending = map[string]*pendingCall{}
	r.mu.Unlock()
	if current != nil {
		current.reject(err)
	}
	r.cancel(err)
	g.releaseLease(r)
	for _, c := range pending {
		c.respond(mcpResult{isError: true, content: []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": "Tool cycle cancelled"})}})
	}
}

func (g *Gateway) cleanup(r *run) {
	g.releaseLease(r)
	r.mu.Lock()
	if r.batchTimer != nil {
		r.batchTimer.Stop()
	}
	if r.waitTimer != nil {
		r.waitTimer.Stop()
	}
	r.mu.Unlock()
	if r.server != nil {
		_ = r.server.Close()
	}
	if r.dir != "" {
		for i := 0; i < 3; i++ {
			if err := os.RemoveAll(r.dir); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// Close fails every live run and waits for their CLI processes to stop.
func (g *Gateway) Close() {
	g.mu.Lock()
	runs := make([]*run, 0, len(g.runs))
	for _, r := range g.runs {
		runs = append(runs, r)
	}
	g.mu.Unlock()
	for _, r := range runs {
		g.fail(r, typedToolError(503, "Service is stopping", "server_error"))
	}
	for _, r := range runs {
		<-r.workDone
	}
}

// Cancel terminates one run by id and waits for its CLI process to stop.
func (g *Gateway) Cancel(id string) bool {
	g.mu.Lock()
	r := g.runs[id]
	g.mu.Unlock()
	if r == nil {
		return false
	}
	g.fail(r, typedToolError(409, "Tool cycle cancelled", "tool_cycle_cancelled"))
	<-r.workDone
	return true
}

func (g *Gateway) continueRun(r *run, results []Result) error {
	defer func() {
		r.mu.Lock()
		r.resuming = false
		r.mu.Unlock()
	}()
	if err := g.takeLease(r); err != nil {
		return err
	}
	for _, result := range results {
		r.mu.Lock()
		call := r.pending[result.ID]
		delete(r.pending, result.ID)
		r.mu.Unlock()
		if call != nil {
			call.respond(mcpResult{content: result.Content, isError: result.IsError})
		}
	}
	g.scheduleCalls(r)
	return nil
}
