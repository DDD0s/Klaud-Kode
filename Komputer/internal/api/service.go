package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/harness"
	"github.com/DDD0s/Klaud-Kode/Komputer/internal/session"
)

type plan struct {
	kind      string // "oneshot" or "session"
	task      bool
	rec       *session.Record
	generated bool
	backend   backend.Backend
	model     string
	effort    string
	tools     string
	useMCP    bool
	recovered string
}

func effortInput(rb *requestBody) backend.EffortInput {
	in := backend.EffortInput{
		Model:           rb.Model,
		ReasoningEffort: rb.String("reasoning_effort"),
		Effort:          rb.String("effort"),
	}
	if oc := rb.Object("output_config"); oc != nil {
		in.OutputEffort = rawString(oc["effort"])
	}
	if th := rb.Object("thinking"); th != nil {
		in.ThinkingEnabled = rawString(th["type"]) == "enabled"
		var budget float64
		if raw, ok := th["budget_tokens"]; ok {
			_ = json.Unmarshal(raw, &budget)
		}
		in.ThinkingBudget = int(budget)
	}
	return in
}

func (s *Server) resolveTarget(rb *requestBody) (backend.Backend, string, string) {
	b := s.backends.Route(rb.Model)
	return b, b.ResolveModel(rb.Model), b.ResolveEffort(effortInput(rb))
}

func modelList(ids []string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"id": id, "object": "model", "type": "model",
			"created": 1727000000, "created_at": "2024-09-22T00:00:00Z",
			"display_name": id, "owned_by": "komputer",
		})
	}
	return out
}

func resolveClientKey(r *http.Request, rb *requestBody) (string, string) {
	for _, h := range []string{
		"X-Conversation-Id", "X-Chat-Id", "X-OpenWebUI-Chat-Id", "X-LibreChat-Conversation-Id",
		"X-Open-Webui-Chat-Id", "X-Komputer-Session", "X-Komputer-Session-Id", "X-Session-Id",
	} {
		if k := domain.NormalizeKey(r.Header.Get(h)); k != "" {
			return k, "header"
		}
	}
	if rb != nil {
		meta := rb.Object("metadata")
		candidates := []string{
			rb.String("conversation_id"), rb.String("chat_id"), rb.String("thread_id"),
			rawString(meta["chat_id"]), rawString(meta["conversation_id"]), rawString(meta["thread_id"]),
			rb.String("session_id"), rb.String("komputer_session"), rb.String("komputer_conversation_id"),
		}
		for _, v := range candidates {
			if k := domain.NormalizeKey(v); k != "" {
				return k, "body"
			}
		}
		if k := domain.NormalizeKey(rb.String("user")); k != "" {
			return k, "user"
		}
		if k := domain.NormalizeKey(rawString(meta["user_id"])); k != "" {
			return k, "user"
		}
	}
	return "", ""
}

func (s *Server) planRequest(r *http.Request, rb *requestBody, messages []domain.Message, fallbackKey string) (*plan, error) {
	b, model, effort := s.resolveTarget(rb)
	if s.cfg.TaskDetect && strings.HasPrefix(strings.TrimSpace(domain.LatestUserText(messages)), "### Task:") {
		taskModel := b.TaskModel()
		if taskModel == "" {
			taskModel = model
		}
		return &plan{kind: "oneshot", task: true, backend: b, model: taskModel, tools: "", useMCP: false}, nil
	}
	ephemeral := truthyValue(r.Header.Get("X-Komputer-Ephemeral")) || rb.Bool("komputer_ephemeral")
	key, source := resolveClientKey(r, rb)
	if key == "" {
		key = fallbackKey
		if key != "" {
			source = "response"
		}
	}
	if ephemeral || (key == "" && s.cfg.NoKeyMode != "auto-session") {
		return &plan{kind: "oneshot", backend: b, model: model, effort: effort, tools: s.cfg.BuiltinTools, useMCP: true}, nil
	}
	var rec *session.Record
	if key != "" {
		rec = s.store.Find(key)
	}
	if rec != nil {
		s.logf("session hit client_key=%s via=%s", rec.ClientKey, source)
	} else {
		var err error
		rec, err = s.store.Create(key, "", b.Name(), b.NewSessionID(), false)
		if errors.Is(err, session.ErrTooManySessions) {
			return nil, typedErr(http.StatusTooManyRequests,
				fmt.Sprintf("max live sessions (%d) reached; delete a chat mapping or wait for idle timeout (%dm)", s.cfg.MaxSessions, int(s.cfg.IdleTimeout/time.Minute)),
				"rate_limit_error")
		}
		if err != nil {
			return nil, err
		}
		s.logf("session new client_key=%s via=%s %s=%s", rec.ClientKey, orDefault(source, "auto"), b.Name(), orDefault(rec.CLISessionID(), "(assigned by cli)"))
	}
	return &plan{
		kind: "session", rec: rec, generated: key == "", backend: b,
		model: model, effort: effort, tools: s.cfg.BuiltinTools, useMCP: true,
	}, nil
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truthyValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
func (s *Server) runCLI(b backend.Backend, turn backend.Turn, sink domain.Sink) (domain.Output, error) {
	return s.runCLIContext(context.Background(), b, turn, sink)
}

func (s *Server) buildSystemPrompt(messages []domain.Message, extraSystem string, includeBody bool) string {
	parts := []string{}
	if includeBody && s.cfg.BodySystem != "" {
		parts = append(parts, s.cfg.BodySystem)
	}
	if t := domain.CollectSystemText(messages, extraSystem); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, blankLine)
}

func (s *Server) runCLIContext(ctx context.Context, b backend.Backend, turn backend.Turn, sink domain.Sink) (domain.Output, error) {
	out, err := s.runner.Run(ctx, b, turn, sink)
	if err == nil {
		return out, nil
	}
	var apiErr *domain.APIError
	if errors.As(err, &apiErr) {
		return domain.Output{}, apiErr
	}
	if ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil {
			if errors.As(cause, &apiErr) {
				return domain.Output{}, apiErr
			}
			return domain.Output{}, cause
		}
		return domain.Output{}, ctx.Err()
	}
	var failure *backend.Failure
	if errors.As(err, &failure) {
		return domain.Output{}, ClassifyCLIError(failure.Message, b, failure.Type, failure.Activity)
	}
	return domain.Output{}, ClassifyCLIError(err.Error(), b, "", false)
}

func (s *Server) runHarnessCLI(ctx context.Context, hctx harness.Context, hooks harness.RunHooks) (domain.Output, error) {
	systemPrompt := hctx.SystemPrompt
	if hooks.Instruction != "" {
		systemPrompt = strings.Join([]string{systemPrompt, hooks.Instruction}, blankLine)
	}
	b := s.backends.Get(hctx.Backend)
	if b == nil {
		return domain.Output{}, typedErr(503, "Backend is unavailable", "server_error")
	}
	mcpServer := &backend.MCPServerSpec{
		Name: hooks.MCPServer.Name, URL: hooks.MCPServer.URL, Headers: hooks.MCPServer.Headers,
	}
	return s.runCLIContext(ctx, b, backend.Turn{
		Session: backend.SessionRef{Mode: backend.SessionNone},
		Prompt:  hctx.Prompt, Attachments: hctx.Attachments,
		SystemPrompt: systemPrompt, IncludeSystem: true, Model: hctx.Model, Effort: hctx.Effort,
		Tools: "", UseMCP: true, MCPConfig: hooks.MCPConfig,
		PrivateMCP: true, WorkDir: hooks.WorkDir, MCPServer: mcpServer,
	}, hooks.Sink)
}

type slotLease struct{ server *Server }

func (l slotLease) Acquire(ctx context.Context) error { return l.server.acquireSlotContext(ctx) }
func (l slotLease) Release()                          { l.server.releaseSlot() }

func (s *Server) acquireSlotContext(ctx context.Context) error {
	s.queued.Add(1)
	defer s.queued.Add(-1)
	select {
	case s.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return ctx.Err()
	}
}

func (s *Server) acquireSlot() {
	_ = s.acquireSlotContext(context.Background())
}

func (s *Server) releaseSlot() {
	select {
	case <-s.slots:
	default:
	}
}

type keyLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func newKeyLocks() *keyLocks { return &keyLocks{locks: map[string]chan struct{}{}} }

func (k *keyLocks) Acquire(key string) func() {
	k.mu.Lock()
	ch, ok := k.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		ch <- struct{}{}
		k.locks[key] = ch
	}
	k.mu.Unlock()
	<-ch
	return func() { ch <- struct{}{} }
}

// tunnelGuard probes and revives the Body health endpoint, single-flight
// through one mutex; successful and failed probes are briefly cached.
type tunnelGuard struct {
	server  *Server
	mu      sync.Mutex
	okAt    time.Time
	failAt  time.Time
	failErr error
}

const tunnelOKCache = 15 * time.Second

func (g *tunnelGuard) ensure() error {
	cfg := g.server.cfg
	// One lock for every path: probeDirect and status() both touch the cached
	// fields, and health checks run concurrently with turns.
	g.mu.Lock()
	defer g.mu.Unlock()
	// A Body that dialed in is either connected or it is not; nothing to probe,
	// nothing to revive.
	if cfg.BodyConnect {
		if g.server.bodyLink.Connected() {
			g.okAt = time.Now()
			g.failErr = nil
			return nil
		}
		err := fmt.Errorf("no body connected")
		g.okAt = time.Time{}
		g.failAt = time.Now()
		g.failErr = err
		return err
	}
	// A direct Body URL is reachable on its own: probe it, and only treat a
	// failure as an outage. No SSH is involved, so there is nothing to revive.
	if cfg.BodyURL != "" {
		return g.probeDirectLocked()
	}
	if cfg.SkipTunnel {
		return nil
	}
	now := time.Now()
	if !g.okAt.IsZero() && now.Sub(g.okAt) < tunnelOKCache {
		return nil
	}
	if g.failErr != nil && now.Sub(g.failAt) < cfg.TunnelFailCache {
		return g.failErr
	}
	err := g.probeLocked()
	if err != nil {
		g.okAt = time.Time{}
		g.failAt = time.Now()
		g.failErr = err
		return err
	}
	g.okAt = time.Now()
	g.failErr = nil
	return nil
}

// probeDirectLocked checks the Body at its configured address, caching
// success and failure so one dead Body does not cost a request every call.
// The caller holds g.mu.
func (g *tunnelGuard) probeDirectLocked() error {
	cfg := g.server.cfg
	now := time.Now()
	if !g.okAt.IsZero() && now.Sub(g.okAt) < tunnelOKCache {
		return nil
	}
	if g.failErr != nil && now.Sub(g.failAt) < cfg.TunnelFailCache {
		return g.failErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.BodyProbe, nil)
	if err == nil {
		var resp *http.Response
		if resp, err = http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				g.okAt = time.Now()
				g.failErr = nil
				return nil
			}
			err = fmt.Errorf("body health returned HTTP %d at %s", resp.StatusCode, cfg.BodyProbe)
		}
	}
	g.okAt = time.Time{}
	g.failAt = time.Now()
	g.failErr = err
	return err
}

func (g *tunnelGuard) probeLocked() error {
	probe := func() (int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.server.cfg.HealthURL, nil)
		if err != nil {
			return 0, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	if status, err := probe(); err == nil && status >= 200 && status < 300 {
		return nil
	}
	g.server.logf("tunnel unhealthy — running tunnel-up.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/bash", g.server.cfg.TunnelUp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tunnel-up failed: %w; output=%s", err, truncate(string(out), 400))
	}
	g.server.logf("tunnel-up ok: %s", truncate(strings.TrimSpace(string(out)), 200))
	status, err := probe()
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("tunnel still unhealthy after revive: HTTP %d", status)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (g *tunnelGuard) status() (*bool, *string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	var ok *bool
	var at *string
	// A directly probed Body reports its latest result even after the failure
	// cache expires, because health impacts an operator's reading not a turn.
	direct := g.server.cfg.BodyURL != ""
	switch {
	case g.failErr != nil && (direct || now.Sub(g.failAt) < g.server.cfg.TunnelFailCache):
		f := false
		ok = &f
		s := session.NowISO(g.failAt)
		at = &s
	case !g.okAt.IsZero() && (direct || now.Sub(g.okAt) < tunnelOKCache):
		t := true
		ok = &t
		s := session.NowISO(g.okAt)
		at = &s
	}
	return ok, at
}

func (s *Server) ensureTunnelOr503() error {
	if err := s.tunnel.ensure(); err != nil {
		apiErr := typedErr(http.StatusServiceUnavailable, publicErrors["body_unavailable"], "body_unavailable")
		apiErr.Detail = privateDetail(err)
		retry := 10
		if s.cfg.BodyConnect {
			// Nothing is cached in connect mode: a Body that redials a second
			// later must succeed on the very next attempt.
			retry = 2
		} else {
			s.tunnel.mu.Lock()
			if !s.tunnel.failAt.IsZero() {
				remain := s.cfg.TunnelFailCache - time.Since(s.tunnel.failAt)
				retry = int((remain + time.Second - 1) / time.Second)
				if retry < 1 {
					retry = 1
				}
			}
			s.tunnel.mu.Unlock()
		}
		apiErr.RetryAfter = retry
		return apiErr
	}
	return nil
}

func (s *Server) executeTurn(p *plan, messages []domain.Message, extraSystem string, sink domain.Sink) (domain.Output, error) {
	var attachments []json.RawMessage
	if latest := domain.LatestUserMessage(messages); latest != nil {
		attachments = domain.ExtractAttachments(latest.Content)
	}
	b := p.backend

	if p.kind == "oneshot" {
		if p.useMCP {
			if err := s.ensureTunnelOr503(); err != nil {
				return domain.Output{}, err
			}
		}
		s.acquireSlot()
		defer s.releaseSlot()
		kind := "stateless"
		if p.task {
			kind = "task"
		}
		s.logf("oneshot %s model=%s msgs=%d attachments=%d", kind, orDefault(p.model, "default"), len(messages), len(attachments))
		return s.runCLI(b, backend.Turn{
			Session:       backend.SessionRef{Mode: backend.SessionNone},
			IncludeSystem: true,
			Prompt:        domain.BuildTurnPrompt(messages, true, s.cfg.MaxHistoryChars),
			Attachments:   attachments,
			SystemPrompt:  s.buildSystemPrompt(messages, extraSystem, !p.task),
			Model:         p.model, Effort: p.effort, Tools: p.tools, UseMCP: p.useMCP,
		}, sink)
	}

	if err := s.ensureTunnelOr503(); err != nil {
		return domain.Output{}, err
	}
	rec := p.rec
	unlock := s.locks.Acquire(rec.ClientKey)
	defer unlock()
	s.acquireSlot()
	defer s.releaseSlot()
	t0 := time.Now()

	mode := "new"
	if rec.Started {
		mode = "resume"
	}
	if rec.BackendName() != b.Name() {
		s.logf("session backend switch client_key=%s %s -> %s", rec.ClientKey, rec.BackendName(), b.Name())
		s.store.AssignBackend(rec, b.Name(), b.NewSessionID())
		if rec.Started {
			rec.Reseeds++
		}
		rec.Started = false
		mode = "reseed"
	}
	if mode == "resume" && s.cfg.DetectDivergence && domain.HistoryDiverged(messages, rec.HistorySeen, rec.LastReplyTail) {
		mode = "reseed"
		s.store.SetCLISessionID(rec, b.NewSessionID())
		rec.Reseeds++
		rec.Started = false
	}
	prompt := domain.BuildTurnPrompt(messages, mode != "resume", s.cfg.MaxHistoryChars)
	systemPrompt := s.buildSystemPrompt(messages, extraSystem, true)
	systemSha := ""
	if b.InlinesSystem() {
		systemSha = domain.SHA256Hex(systemPrompt, 0)
	}
	s.logf("turn start client_key=%s %s=%s mode=%s turn=%d model=%s effort=%s attachments=%d promptChars=%d",
		rec.ClientKey, b.Name(), orDefault(rec.CLISessionID(), "(new)"), mode, rec.TurnCount+1,
		orDefault(p.model, "default"), orDefault(p.effort, "default"), len(attachments), len([]rune(prompt)))

	streamed := &strings.Builder{}
	onText := func(ev domain.Event) {
		if ev.Kind == domain.EventText {
			streamed.WriteString(ev.Value)
		}
		sink.Emit(ev.Kind, ev.Value)
	}
	run := func() (domain.Output, error) {
		sessionMode := backend.SessionNew
		if mode == "resume" {
			sessionMode = backend.SessionResume
		}
		return s.runCLI(b, backend.Turn{
			Session:       backend.SessionRef{Mode: sessionMode, ID: rec.CLISessionID()},
			IncludeSystem: mode != "resume" || rec.SystemSHA != systemSha,
			Prompt:        prompt,
			Attachments:   attachments,
			SystemPrompt:  systemPrompt,
			Model:         p.model, Effort: p.effort, Tools: p.tools, UseMCP: p.useMCP,
		}, onText)
	}

	out, err := run()
	if err != nil && mode == "resume" {
		var apiErr *domain.APIError
		if errors.As(err, &apiErr) && apiErr.MissingBeforeActivity {
			s.logf("session restore failed, reseeding client_key=%s: %s", rec.ClientKey, privateDetail(err))
			mode = "reseed"
			s.store.SetCLISessionID(rec, b.NewSessionID())
			rec.Started = false
			rec.HistorySeen = false
			rec.LastReplyTail = ""
			rec.Reseeds++
			if domain.PriorTranscript(messages, s.cfg.MaxHistoryChars) != "" {
				p.recovered = "client-history"
			} else {
				p.recovered = "empty"
			}
			prompt = domain.BuildTurnPrompt(messages, true, s.cfg.MaxHistoryChars)
			if p.recovered == "empty" {
				prompt = "Earlier conversation context is unavailable. Do not assume you remember it." + blankLine + prompt
			}
			_ = s.store.Save()
			streamed.Reset()
			out, err = run()
		}
	}
	if err != nil {
		if mode != "resume" {
			// A failed first turn may have left a half-created CLI session
			// under this id; a retry with the same id would be refused.
			s.store.SetCLISessionID(rec, b.NewSessionID())
			_ = s.store.Save()
		}
		return domain.Output{}, err
	}
	if b.AssignsSessionID() && out.SessionID != "" && out.SessionID != rec.CLISessionID() {
		s.store.SetCLISessionID(rec, out.SessionID)
	}
	if rec.Backend != "" {
		rec.SystemSHA = systemSha
	}
	rec.Started = rec.CLISessionID() != ""
	rec.TurnCount++
	rec.LastUsedAt = session.NowISO(time.Now())
	if domain.HasAssistantMessage(messages) {
		rec.HistorySeen = true
	}
	tailSource := out.Text
	if streamed.Len() > 0 {
		tailSource = streamed.String()
	}
	rec.LastReplyTail = domain.ReplyTail(tailSource)
	if err := s.store.Save(); err != nil {
		s.logf("warn: session save failed: %v", err)
	}
	s.logf("turn done client_key=%s ms=%d chars=%d turns=%d", rec.ClientKey, time.Since(t0).Milliseconds(), len(out.Text), rec.TurnCount)
	return out, nil
}

// --- turn registry: a turn survives its client ---

type subscription struct{ sink domain.Sink }

type turnState struct {
	mu         sync.Mutex
	events     []domain.Event
	subs       map[*subscription]struct{}
	done       chan struct{}
	out        domain.Output
	err        error
	finished   bool
	delivered  bool
	finishedAt time.Time
}

type turnRegistry struct {
	mu    sync.Mutex
	turns map[string]*turnState
	cache time.Duration
}

func newTurnRegistry(cache time.Duration) *turnRegistry {
	return &turnRegistry{turns: map[string]*turnState{}, cache: cache}
}

func (r *turnRegistry) sweep(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.turns {
		t.mu.Lock()
		done := t.finished
		at := t.finishedAt
		t.mu.Unlock()
		if done && now.Sub(at) > r.cache {
			delete(r.turns, k)
		}
	}
}

func (r *turnRegistry) get(key string) *turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[key]
}

func (r *turnRegistry) put(key string, t *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns[key] = t
}

func (r *turnRegistry) delete(key string, t *turnState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.turns[key] == t {
		delete(r.turns, key)
	}
}

func (t *turnState) emit(ev domain.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, ev)
	for sub := range t.subs {
		sub.sink.Emit(ev.Kind, ev.Value)
	}
}

func (t *turnState) subscribe(sub *subscription) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range t.events {
		sub.sink.Emit(ev.Kind, ev.Value)
	}
	t.subs[sub] = struct{}{}
}

func (t *turnState) unsubscribe(sub *subscription, delivered bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.subs, sub)
	if delivered {
		t.delivered = true
	}
}

func turnKey(p *plan, messages []domain.Message, extraSystem string) string {
	who := "oneshot"
	if p.kind == "session" {
		who = "s:" + p.rec.ClientKey
	}
	what, _ := json.Marshal(map[string]any{
		"messages": messages, "extraSystem": extraSystem,
		"tools": p.tools, "mcp": p.useMCP, "task": p.task,
	})
	cli := ""
	if p.backend.Name() != "claude" {
		cli = p.backend.Name() + ":"
	}
	return fmt.Sprintf("%s|%s%s|%s|%s", who, cli, p.model, p.effort, domain.SHA256Hex(string(what), 32))
}

// runOrAttach runs a turn, or attaches to the identical one already running
// or just finished while nobody was listening.
func (s *Server) runOrAttach(ctx context.Context, p *plan, messages []domain.Message, extraSystem string, sink domain.Sink) (domain.Output, error) {
	s.turns.sweep(time.Now())
	key := turnKey(p, messages, extraSystem)
	creator := false
	t := s.turns.get(key)
	if t != nil {
		t.mu.Lock()
		finished, out, err, delivered := t.finished, t.out, t.err, t.delivered
		t.mu.Unlock()
		if finished {
			if err == nil && !delivered {
				s.logf("turn replay key=%s (cached answer from an abandoned turn)", truncate(key, 40))
				s.turns.delete(key, t)
				t.mu.Lock()
				for _, ev := range t.events {
					sink.Emit(ev.Kind, ev.Value)
				}
				t.mu.Unlock()
				return out, nil
			}
			// A failed or delivered turn is never replayed.
			s.turns.delete(key, t)
			t = nil
		} else {
			s.logf("turn attach key=%s (retry joined a running turn)", truncate(key, 40))
		}
	}
	if t == nil {
		creator = true
		t = &turnState{subs: map[*subscription]struct{}{}, done: make(chan struct{})}
		s.turns.put(key, t)
		go func() {
			out, err := s.executeTurn(p, messages, extraSystem, func(ev domain.Event) { t.emit(ev) })
			t.mu.Lock()
			t.out, t.err = out, err
			t.finished = true
			t.finishedAt = time.Now()
			t.mu.Unlock()
			close(t.done)
		}()
	}
	sub := &subscription{sink: sink}
	t.subscribe(sub)
	delivered := false
	defer func() {
		t.unsubscribe(sub, delivered)
		if creator && delivered {
			s.turns.delete(key, t)
		}
	}()
	select {
	case <-t.done:
		t.mu.Lock()
		out, err := t.out, t.err
		t.mu.Unlock()
		if err == nil {
			delivered = true
		}
		return out, err
	case <-ctx.Done():
		return domain.Output{}, ctx.Err()
	}
}
