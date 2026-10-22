package responses

import "sync"

// StreamWriter emits the Responses streaming event sequence and builds the
// final output list.
type StreamWriter struct {
	write    func(eventType string, data any)
	response map[string]any

	mu      sync.Mutex
	seq     int
	output  []any
	current *streamItem
	started bool
}

type streamItem struct {
	kind  string
	id    string
	index int
	text  string
}

// NewStreamWriter creates a writer around one send function. The caller
// owns SSE framing; sequence numbers are stamped here.
func NewStreamWriter(write func(eventType string, data any), response map[string]any) *StreamWriter {
	return &StreamWriter{write: write, response: response}
}

func cloneResponse(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (w *StreamWriter) event(eventType string, data map[string]any) {
	payload := map[string]any{"type": eventType, "sequence_number": w.seq}
	for k, v := range data {
		payload[k] = v
	}
	w.seq++
	w.write(eventType, payload)
}

func (w *StreamWriter) startLocked() {
	if w.started {
		return
	}
	w.started = true
	response := cloneResponse(w.response)
	response["status"] = "in_progress"
	response["output"] = []any{}
	response["usage"] = nil
	w.event("response.created", map[string]any{"response": response})
	w.event("response.in_progress", map[string]any{"response": response})
}

// Start emits the created/in-progress pair once.
func (w *StreamWriter) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
}

func (w *StreamWriter) openLocked(kind string) *streamItem {
	if w.current != nil && w.current.kind == kind {
		return w.current
	}
	w.closeLocked()
	index := len(w.output)
	if kind == "text" {
		id := "msg_" + hexID()
		w.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}})
		w.event("response.content_part.added", map[string]any{"item_id": id, "output_index": index, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
		w.current = &streamItem{kind: kind, id: id, index: index}
	} else {
		id := "rs_" + hexID()
		w.event("response.output_item.added", map[string]any{"output_index": index, "item": map[string]any{"id": id, "type": "reasoning", "summary": []any{}}})
		w.event("response.reasoning_summary_part.added", map[string]any{"item_id": id, "output_index": index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
		w.current = &streamItem{kind: kind, id: id, index: index}
	}
	w.output = append(w.output, nil)
	return w.current
}

// Text streams one output_text delta.
func (w *StreamWriter) Text(delta string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
	c := w.openLocked("text")
	c.text += delta
	w.event("response.output_text.delta", map[string]any{"item_id": c.id, "output_index": c.index, "content_index": 0, "delta": delta, "logprobs": []any{}})
}

// Reasoning streams one reasoning summary delta.
func (w *StreamWriter) Reasoning(delta string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
	c := w.openLocked("reasoning")
	c.text += delta
	w.event("response.reasoning_summary_text.delta", map[string]any{"item_id": c.id, "output_index": c.index, "summary_index": 0, "delta": delta})
}

func (w *StreamWriter) closeLocked() {
	c := w.current
	if c == nil {
		return
	}
	w.current = nil
	var item map[string]any
	if c.kind == "text" {
		part := map[string]any{"type": "output_text", "text": c.text, "annotations": []any{}, "logprobs": []any{}}
		w.event("response.output_text.done", map[string]any{"item_id": c.id, "output_index": c.index, "content_index": 0, "text": c.text, "logprobs": []any{}})
		w.event("response.content_part.done", map[string]any{"item_id": c.id, "output_index": c.index, "content_index": 0, "part": part})
		item = messageItem(c.id, c.text, "completed")
	} else {
		w.event("response.reasoning_summary_text.done", map[string]any{"item_id": c.id, "output_index": c.index, "summary_index": 0, "text": c.text})
		w.event("response.reasoning_summary_part.done", map[string]any{"item_id": c.id, "output_index": c.index, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": c.text}})
		item = map[string]any{"id": c.id, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": c.text}}}
	}
	w.output[c.index] = item
	w.event("response.output_item.done", map[string]any{"output_index": c.index, "item": item})
}

func (w *StreamWriter) filteredOutput() []any {
	out := []any{}
	for _, item := range w.output {
		if item != nil {
			out = append(out, item)
		}
	}
	return out
}

// FunctionCall emits one completed function-call item.
func (w *StreamWriter) FunctionCall(call ToolCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
	w.closeLocked()
	index := len(w.output)
	item := functionCallItem(call, "completed")
	w.output = append(w.output, item)
	added := functionCallItem(call, "in_progress")
	added["arguments"] = ""
	w.event("response.output_item.added", map[string]any{"output_index": index, "item": added})
	w.event("response.function_call_arguments.delta", map[string]any{"item_id": item["id"], "output_index": index, "delta": call.Arguments})
	w.event("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": index, "arguments": call.Arguments})
	w.event("response.output_item.done", map[string]any{"output_index": index, "item": item})
}

// Complete seals the stream and returns the final response object.
func (w *StreamWriter) Complete(usage map[string]any) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
	hasFinal := false
	for _, item := range w.output {
		if m, ok := item.(map[string]any); ok && (m["type"] == "message" || m["type"] == "function_call") {
			hasFinal = true
		}
	}
	if !hasFinal && (w.current == nil || w.current.kind != "text") {
		w.openLocked("text")
	}
	w.closeLocked()
	response := cloneResponse(w.response)
	response["status"] = "completed"
	response["output"] = w.filteredOutput()
	response["usage"] = usage
	w.event("response.completed", map[string]any{"response": response})
	return response
}

// Fail seals the stream as failed and returns the final response object.
func (w *StreamWriter) Fail(errValue any) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked()
	w.closeLocked()
	response := cloneResponse(w.response)
	response["status"] = "failed"
	response["output"] = w.filteredOutput()
	response["error"] = errValue
	w.event("response.failed", map[string]any{"response": response})
	return response
}
