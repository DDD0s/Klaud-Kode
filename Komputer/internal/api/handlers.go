package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/harness"
)

func conversationHeaders(p *plan) map[string]string {
	if p.kind != "session" {
		mode := "stateless"
		if p.task {
			mode = "task"
		}
		return map[string]string{"X-Komputer-Mode": mode}
	}
	h := map[string]string{
		"X-Conversation-Id":         p.rec.ClientKey,
		"X-Chat-Id":                 p.rec.ClientKey,
		"X-Komputer-Session":        p.rec.ClientKey,
		"X-Komputer-Mode":           "session",
		"X-Komputer-Claude-Session": p.rec.ClaudeSessionID,
	}
	if p.recovered != "" {
		h["X-Komputer-Session-Recovered"] = p.recovered
	}
	return h
}

func planKeyFields(p *plan) map[string]any {
	if p.kind != "session" {
		return nil
	}
	k := p.rec.ClientKey
	return map[string]any{
		"conversation_id": k, "chat_id": k,
		"komputer_conversation_id": k, "komputer_session": k,
	}
}

func mergeFields(dst map[string]any, extra map[string]any) map[string]any {
	for k, v := range extra {
		dst[k] = v
	}
	return dst
}

func openAIUsage(u *domain.Usage) map[string]any {
	input, output := 0, 0
	if u != nil {
		input = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		output = u.OutputTokens
	}
	return map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
}

func validateRoles(messages []domain.Message, allowed map[string]bool) error {
	for _, m := range messages {
		if !allowed[strings.ToLower(m.Role)] {
			return httpErr(http.StatusBadRequest, "Invalid message role or shape")
		}
	}
	return nil
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rb, err := s.parseBody(r, false)
	if err != nil {
		return err
	}
	messages, err := rb.Messages()
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return httpErr(http.StatusBadRequest, "messages required")
	}
	if err := validateRoles(messages, map[string]bool{"system": true, "developer": true, "user": true, "assistant": true, "tool": true}); err != nil {
		return err
	}
	spec, err := harness.ParseRequest("openai", rb.Fields)
	if err != nil {
		return err
	}
	if spec != nil {
		return s.handleClientTools(w, r, "openai", rb, messages, spec)
	}
	model := rb.Model
	if model == "" {
		model = s.backend.DefaultID()
	}
	p, err := s.planRequest(r, rb, messages, "")
	if err != nil {
		return err
	}
	id := "chatcmpl-" + strings.ReplaceAll(domain.NewUUID(), "-", "")[:24]
	created := time.Now().Unix()
	keyFields := planKeyFields(p)
	hdrs := func() map[string]string { return conversationHeaders(p) }
	s.logf("chat id=%s mode=%s%s model=%s msgs=%d stream=%v", id, p.kind, map[bool]string{true: "/task"}[p.task], model, len(messages), rb.Stream)

	if !rb.Stream {
		out, err := s.runOrAttach(r.Context(), p, messages, rb.String("system"), nil)
		if err != nil {
			if errors.Is(err, r.Context().Err()) {
				return nil
			}
			return err
		}
		message := map[string]any{"role": "assistant", "content": out.Text}
		if out.Thinking != "" && s.cfg.StreamThinking {
			message["reasoning_content"] = out.Thinking
		}
		body := map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
			"usage":   openAIUsage(out.Usage),
		}
		writeJSON(w, http.StatusOK, mergeFields(body, keyFields), hdrs())
		return nil
	}

	sse := newDeferredSSE(w, r, hdrs, s.cfg.Heartbeat, s.cfg.SSEOpen)
	defer sse.Close("data: [DONE]" + blankLine)
	chunk := func(delta map[string]any, finish any, usage map[string]any) map[string]any {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		mergeFields(c, keyFields)
		if usage != nil {
			c["usage"] = usage
		}
		return c
	}
	var mu sync.Mutex
	started := false
	send := func(obj any) {
		mu.Lock()
		defer mu.Unlock()
		if !started {
			started = true
			sse.Write("data: " + mustJSON(chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil)) + blankLine)
		}
		sse.Write("data: " + mustJSON(obj) + blankLine)
	}
	sink := func(ev domain.Event) {
		switch ev.Kind {
		case domain.EventText:
			send(chunk(map[string]any{"content": ev.Value}, nil, nil))
		case domain.EventThinking:
			if s.cfg.StreamThinking {
				send(chunk(map[string]any{"reasoning_content": ev.Value}, nil, nil))
			}
		case domain.EventTool:
			if s.cfg.StreamToolNotes && s.cfg.StreamThinking {
				send(chunk(map[string]any{"reasoning_content": newline + "[tool] " + ev.Value + newline}, nil, nil))
			}
		}
	}
	out, err := s.runOrAttach(r.Context(), p, messages, rb.String("system"), sink)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return nil
		}
		if !sse.Open() {
			return err
		}
		s.logf("chat stream error id=%s: %s", id, privateDetail(err))
		send(map[string]any{"error": map[string]any{
			"message": publicMessage(err), "type": errorType(err), "code": errorStatusOf(err),
		}})
		return nil
	}
	send(chunk(map[string]any{}, "stop", openAIUsage(out.Usage)))
	return nil
}

func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out []string
		for _, p := range parts {
			if text, err := func() (string, error) { var v string; err := json.Unmarshal(p, &v); return v, err }(); err == nil {
				out = append(out, text)
				continue
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(p, &obj); err == nil {
				out = append(out, rawString(obj["text"]))
			}
		}
		return strings.Join(out, newline)
	}
	return ""
}

type anthropicBlockWriter struct {
	send    func(event string, data any)
	index   int
	current string
	opened  bool
}

func newAnthropicBlockWriter(send func(event string, data any)) *anthropicBlockWriter {
	return &anthropicBlockWriter{send: send, index: -1}
}

func (b *anthropicBlockWriter) close() {
	if b.current == "" {
		return
	}
	b.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.index})
	b.current = ""
}

func (b *anthropicBlockWriter) open(kind string) {
	if b.current == kind {
		return
	}
	b.close()
	b.index++
	b.current = kind
	b.opened = true
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": ""}
	}
	b.send("content_block_start", map[string]any{"type": "content_block_start", "index": b.index, "content_block": block})
}

func (b *anthropicBlockWriter) text(t string) {
	b.open("text")
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "text_delta", "text": t},
	})
}

func (b *anthropicBlockWriter) thinking(t string) {
	b.open("thinking")
	b.send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": t},
	})
}

func (b *anthropicBlockWriter) finish() {
	if !b.opened {
		b.open("text")
	}
	b.close()
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rb, err := s.parseBody(r, false)
	if err != nil {
		return err
	}
	messages, err := rb.Messages()
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return httpErr(http.StatusBadRequest, "messages required")
	}
	if err := validateRoles(messages, map[string]bool{"user": true, "assistant": true}); err != nil {
		return err
	}
	spec, err := harness.ParseRequest("anthropic", rb.Fields)
	if err != nil {
		return err
	}
	if spec != nil {
		return s.handleClientTools(w, r, "anthropic", rb, messages, spec)
	}
	model := rb.Model
	if model == "" {
		model = s.backend.DefaultID()
	}
	system := anthropicSystemText(rb.Fields["system"])
	msgID := "msg_" + strings.ReplaceAll(domain.NewUUID(), "-", "")[:24]
	p, err := s.planRequest(r, rb, messages, "")
	if err != nil {
		return err
	}
	hdrs := func() map[string]string { return conversationHeaders(p) }
	keyFields := planKeyFields(p)
	s.logf("messages id=%s mode=%s%s model=%s stream=%v", msgID, p.kind, map[bool]string{true: "/task"}[p.task], model, rb.Stream)

	if !rb.Stream {
		out, err := s.runOrAttach(r.Context(), p, messages, system, nil)
		if err != nil {
			if errors.Is(err, r.Context().Err()) {
				return nil
			}
			return err
		}
		content := []any{}
		if out.Thinking != "" && s.cfg.StreamThinking {
			content = append(content, map[string]any{"type": "thinking", "thinking": out.Thinking, "signature": ""})
		}
		content = append(content, map[string]any{"type": "text", "text": out.Text})
		inputTokens, outputTokens := 0, 0
		if out.Usage != nil {
			inputTokens, outputTokens = out.Usage.InputTokens, out.Usage.OutputTokens
		}
		body := map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens},
		}
		writeJSON(w, http.StatusOK, mergeFields(body, keyFields), hdrs())
		return nil
	}

	sse := newDeferredSSE(w, r, hdrs, s.cfg.Heartbeat, s.cfg.SSEOpen)
	defer sse.Close("")
	var mu sync.Mutex
	started := false
	send := func(event string, data any) {
		mu.Lock()
		defer mu.Unlock()
		if !started {
			started = true
			start := map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": msgID, "type": "message", "role": "assistant", "model": model,
					"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			}
			sse.Write("event: message_start" + newline + "data: " + mustJSON(start) + blankLine)
		}
		sse.Write("event: " + event + newline + "data: " + mustJSON(data) + blankLine)
	}
	blocks := newAnthropicBlockWriter(send)
	sink := func(ev domain.Event) {
		switch ev.Kind {
		case domain.EventText:
			blocks.text(ev.Value)
		case domain.EventThinking:
			if s.cfg.StreamThinking {
				blocks.thinking(ev.Value)
			}
		}
	}
	out, err := s.runOrAttach(r.Context(), p, messages, system, sink)
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return nil
		}
		if !sse.Open() {
			return err
		}
		s.logf("messages stream error id=%s: %s", msgID, privateDetail(err))
		send("error", map[string]any{"type": "error", "error": map[string]any{"type": errorType(err), "message": publicMessage(err)}})
		return nil
	}
	blocks.finish()
	outputTokens := 0
	if out.Usage != nil {
		outputTokens = out.Usage.OutputTokens
	}
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
	return nil
}
