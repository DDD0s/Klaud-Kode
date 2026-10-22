package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/harness"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/responses"
)

func cloneResponseMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Server) rememberResponse(fields map[string]json.RawMessage, parsed *responses.ParsedRequest, response map[string]any, key, text string, calls []responses.ToolCall) {
	if raw, ok := fields["store"]; ok {
		var store bool
		if err := json.Unmarshal(raw, &store); err == nil && !store {
			return
		}
	}
	content, _ := json.Marshal(text)
	outMsg := responses.ChatMessage{Role: "assistant", Content: content}
	for _, call := range calls {
		var c responses.ChatToolCall
		c.ID = call.CallID
		c.Type = "function"
		c.Function.Name = call.Name
		c.Function.Arguments = call.Arguments
		outMsg.ToolCalls = append(outMsg.ToolCalls, c)
	}
	var previous *string
	if id := rawString(fields["previous_response_id"]); id != "" {
		previous = &id
	}
	var keyPtr *string
	if key != "" {
		keyPtr = &key
	}
	responseRaw, _ := json.Marshal(response)
	createdAt, _ := response["created_at"].(int64)
	rec := &responses.Record{
		ID: response["id"].(string), PreviousResponseID: previous, Key: keyPtr,
		CreatedAt: createdAt, Input: responses.StoredMessages(parsed.NewMessages),
		Output: []responses.ChatMessage{outMsg}, Response: responseRaw,
	}
	if err := s.responses.Put(rec); err != nil {
		s.logf("warn: failed to store response %s: %v", rec.ID, err)
	}
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rb, err := s.parseBody(r, false)
	if err != nil {
		return err
	}
	parsed, err := responses.ParseRequest(rb.Fields, s.responses)
	if err != nil {
		return err
	}
	chatRB := &requestBody{Fields: parsed.ChatFields, Model: rawString(parsed.ChatFields["model"]), Stream: parsed.Stream}
	messages := parsed.DomainMessages()
	spec, err := harness.ParseRequest("openai", chatRB.Fields)
	if err != nil {
		return err
	}
	if spec != nil {
		return s.handleResponsesTools(w, r, rb, chatRB, messages, parsed, spec)
	}
	p, err := s.planRequest(r, chatRB, messages, parsed.FallbackKey)
	if err != nil {
		return err
	}
	createdAt := time.Now().Unix()
	model := rb.Model
	if model == "" {
		model = s.backend.DefaultID()
	}
	id := "resp_" + strings.ReplaceAll(domain.NewUUID(), "-", "")
	base := responses.ResponseObject(rb.Fields, id, createdAt, model, "in_progress", nil, nil, nil)
	hdrs := func() map[string]string { return conversationHeaders(p) }
	key := ""
	if p.kind == "session" {
		key = p.rec.ClientKey
	}
	previousID := rawString(rb.Fields["previous_response_id"])
	s.logf("responses id=%s mode=%s%s key=%s model=%s prev=%s stream=%v", id, p.kind,
		map[bool]string{true: "/task"}[p.task], orDefault(key, "-"), model, orDefault(previousID, "-"), rb.Stream)
	extraSystem := rawString(chatRB.Fields["system"])

	if !rb.Stream {
		out, err := s.runOrAttach(r.Context(), p, messages, extraSystem, nil)
		if err != nil {
			if errors.Is(err, r.Context().Err()) {
				return nil
			}
			return err
		}
		thinking := ""
		if s.cfg.StreamThinking {
			thinking = out.Thinking
		}
		response := cloneResponseMap(base)
		response["status"] = "completed"
		response["output"] = responses.Output(out.Text, thinking, nil)
		response["usage"] = responses.Usage(out.Usage)
		s.rememberResponse(rb.Fields, parsed, response, key, out.Text, nil)
		writeJSON(w, http.StatusOK, response, hdrs())
		return nil
	}

	sse := newDeferredSSE(w, r, hdrs, s.cfg.Heartbeat, s.cfg.SSEOpen)
	defer sse.Close("")
	writer := responses.NewStreamWriter(func(eventType string, data any) {
		sse.Write("event: " + eventType + newline + "data: " + mustJSON(data) + blankLine)
	}, base)
	text := ""
	out, err := s.runOrAttach(r.Context(), p, messages, extraSystem, func(ev domain.Event) {
		switch ev.Kind {
		case domain.EventText:
			text += ev.Value
			writer.Text(ev.Value)
		case domain.EventThinking:
			if s.cfg.StreamThinking {
				writer.Reasoning(ev.Value)
			}
		case domain.EventTool:
			if s.cfg.StreamToolNotes {
				writer.Reasoning(newline + "[tool] " + ev.Value + newline)
			}
		}
	})
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return nil
		}
		if !sse.Open() {
			return err
		}
		s.logf("responses stream error id=%s: %s", id, privateDetail(err))
		writer.Fail(map[string]any{"code": errorType(err), "message": publicMessage(err)})
		return nil
	}
	if text == "" && out.Text != "" {
		text = out.Text
		writer.Text(out.Text)
	}
	response := writer.Complete(responses.Usage(out.Usage))
	s.rememberResponse(rb.Fields, parsed, response, key, text, nil)
	return nil
}

func (s *Server) handleResponsesTools(w http.ResponseWriter, r *http.Request, rb *requestBody, chatRB *requestBody, messages []domain.Message, parsed *responses.ParsedRequest, spec *harness.Spec) error {
	turn, err := s.harness.Prepare(spec, s.harnessContext(r, chatRB, messages, "openai"))
	if err != nil {
		return err
	}
	id := "resp_" + turn.ID
	model := rb.Model
	if model == "" {
		model = turn.Model
	}
	if model == "" {
		model = s.backend.DefaultID()
	}
	createdAt := time.Now().Unix()
	hdrs := func() map[string]string {
		return map[string]string{"X-Komputer-Mode": "harness", "X-Komputer-Run": turn.RunID}
	}
	base := responses.ResponseObject(rb.Fields, id, createdAt, model, "in_progress", nil, nil, nil)
	callsOf := func(out harness.SegmentOutput) []responses.ToolCall {
		calls := make([]responses.ToolCall, 0, len(out.ToolCalls))
		for _, c := range out.ToolCalls {
			calls = append(calls, responses.ToolCall{CallID: c.ID, Name: c.Name, Arguments: string(c.Arguments)})
		}
		return calls
	}
	toolsLabel := "continued"
	if spec.HasTools {
		toolsLabel = strconv.Itoa(len(spec.Tools))
	}
	s.logf("responses harness id=%s tools=%s results=%d prev=%s", id, toolsLabel, len(spec.Results), orDefault(rawString(rb.Fields["previous_response_id"]), "-"))

	if !rb.Stream {
		out, err := turn.Wait(r.Context(), nil)
		if err != nil {
			if errors.Is(err, r.Context().Err()) {
				return nil
			}
			return err
		}
		calls := callsOf(out)
		thinking := ""
		if s.cfg.StreamThinking {
			thinking = out.Thinking
		}
		response := cloneResponseMap(base)
		response["status"] = "completed"
		response["output"] = responses.Output(out.Text, thinking, calls)
		response["usage"] = responses.Usage(out.Usage)
		s.rememberResponse(rb.Fields, parsed, response, "", out.Text, calls)
		writeJSON(w, http.StatusOK, response, hdrs())
		return nil
	}

	sse := newDeferredSSE(w, r, hdrs, s.cfg.Heartbeat, s.cfg.SSEOpen)
	defer sse.Close("")
	writer := responses.NewStreamWriter(func(eventType string, data any) {
		sse.Write("event: " + eventType + newline + "data: " + mustJSON(data) + blankLine)
	}, base)
	text := ""
	out, err := turn.Wait(r.Context(), func(ev domain.Event) {
		switch ev.Kind {
		case domain.EventText:
			text += ev.Value
			writer.Text(ev.Value)
		case domain.EventThinking:
			if s.cfg.StreamThinking {
				writer.Reasoning(ev.Value)
			}
		}
	})
	if err != nil {
		if errors.Is(err, r.Context().Err()) {
			return nil
		}
		if !sse.Open() {
			return err
		}
		s.logf("responses harness stream error: %s", privateDetail(err))
		writer.Fail(map[string]any{"code": errorType(err), "message": publicMessage(err)})
		return nil
	}
	calls := callsOf(out)
	for _, call := range calls {
		writer.FunctionCall(call)
	}
	response := writer.Complete(responses.Usage(out.Usage))
	s.rememberResponse(rb.Fields, parsed, response, "", text, calls)
	return nil
}

func validResponseID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func (s *Server) handleGetResponse(w http.ResponseWriter, r *http.Request, id string) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	rec := s.responses.Get(id)
	if rec == nil {
		return typedErr(http.StatusNotFound, "Response with id '"+id+"' not found.", "invalid_request_error")
	}
	var response any
	if err := json.Unmarshal(rec.Response, &response); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, response, nil)
	return nil
}

func (s *Server) handleDeleteResponse(w http.ResponseWriter, r *http.Request, id string) error {
	if !s.requireAuth(w, r) {
		return nil
	}
	deleted, err := s.responses.Delete(id)
	if err != nil {
		return err
	}
	if !deleted {
		return typedErr(http.StatusNotFound, "Response with id '"+id+"' not found.", "invalid_request_error")
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response", "deleted": true}, nil)
	return nil
}
