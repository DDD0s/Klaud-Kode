// Package responses translates the OpenAI Responses API onto the chat
// shapes the service already runs. previous_response_id chains live in a
// local store and are mapped onto CLI sessions by the API service.
package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

func invalid(message string) *domain.APIError {
	return &domain.APIError{Status: 400, Type: "invalid_request_error", Message: message}
}

func hexID() string { return strings.ReplaceAll(domain.NewUUID(), "-", "") }

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func mustRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

func isEmptyJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s == ""
}

// ChatToolCall is one function call in chat-message form.
type ChatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func newChatToolCall(id, name, args string) ChatToolCall {
	var c ChatToolCall
	c.ID, c.Type = id, "function"
	c.Function.Name, c.Function.Arguments = name, args
	return c
}

// ChatMessage is a chat-shaped message that retains tool calls and ids.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Domain converts to the service's protocol-neutral message.
func (m ChatMessage) Domain() domain.Message {
	return domain.Message{Role: m.Role, Content: append(json.RawMessage{}, m.Content...)}
}

func partsToChat(content json.RawMessage) (json.RawMessage, error) {
	if isNull(content) {
		return mustRaw(""), nil
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return mustRaw(s), nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, invalid("content must be a string or an array of content parts")
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, raw := range parts {
		p, ok := rawObject(raw)
		if !ok {
			return nil, invalid("Unsupported content part type: ")
		}
		switch rawString(p["type"]) {
		case "input_text", "output_text", "text":
			out = append(out, mustRaw(map[string]any{"type": "text", "text": rawString(p["text"])}))
		case "refusal":
			out = append(out, mustRaw(map[string]any{"type": "text", "text": rawString(p["refusal"])}))
		case "input_image":
			u := rawString(p["image_url"])
			if u == "" {
				if obj, ok := rawObject(p["image_url"]); ok {
					u = rawString(obj["url"])
				}
			}
			if u == "" {
				return nil, invalid("input_image needs image_url; file_id is not supported")
			}
			out = append(out, mustRaw(map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}))
		case "input_file":
			fileData := rawString(p["file_data"])
			if fileData == "" {
				return nil, invalid("input_file needs file_data; file_id and file_url are not supported")
			}
			file := map[string]any{"file_data": fileData}
			if fn := rawString(p["filename"]); fn != "" {
				file["filename"] = fn
			}
			out = append(out, mustRaw(map[string]any{"type": "file", "file": file}))
		default:
			return nil, invalid(fmt.Sprintf("Unsupported content part type: %s", rawString(p["type"])))
		}
	}
	return mustRaw(out), nil
}

func outputToChat(output json.RawMessage) (json.RawMessage, error) {
	if isNull(output) {
		return mustRaw(""), nil
	}
	var s string
	if err := json.Unmarshal(output, &s); err == nil {
		return mustRaw(s), nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(output, &parts); err != nil {
		compact, _ := json.Marshal(json.RawMessage(output))
		// A non-array, non-string output is stringified, as before.
		var v any
		_ = json.Unmarshal(output, &v)
		data, _ := json.Marshal(v)
		_ = compact
		return mustRaw(string(data)), nil
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, raw := range parts {
		p, ok := rawObject(raw)
		if !ok {
			return nil, invalid("function_call_output supports text and images only")
		}
		switch rawString(p["type"]) {
		case "input_text", "output_text":
			out = append(out, mustRaw(map[string]any{"type": "text", "text": rawString(p["text"])}))
		case "input_image":
			u := rawString(p["image_url"])
			if u == "" {
				if obj, ok := rawObject(p["image_url"]); ok {
					u = rawString(obj["url"])
				}
			}
			out = append(out, mustRaw(map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}))
		default:
			return nil, invalid("function_call_output supports text and images only")
		}
	}
	return mustRaw(out), nil
}

// InputToMessages translates Responses input into chat messages.
func InputToMessages(input json.RawMessage) ([]ChatMessage, error) {
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		return []ChatMessage{{Role: "user", Content: mustRaw(s)}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, invalid("input must be a string or an array of items")
	}
	out := []ChatMessage{}
	for _, raw := range items {
		item, ok := rawObject(raw)
		if !ok {
			return nil, invalid("input items must be objects")
		}
		typ := rawString(item["type"])
		if typ == "" && rawString(item["role"]) != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			role := rawString(item["role"])
			switch role {
			case "user", "assistant", "system", "developer":
			default:
				return nil, invalid(fmt.Sprintf("Unsupported message role: %s", role))
			}
			content, err := partsToChat(item["content"])
			if err != nil {
				return nil, err
			}
			out = append(out, ChatMessage{Role: role, Content: content})
		case "function_call":
			callID, name := rawString(item["call_id"]), rawString(item["name"])
			if callID == "" || name == "" {
				return nil, invalid("function_call items need call_id and name")
			}
			args := rawString(item["arguments"])
			if args == "" && len(item["arguments"]) > 0 && !isEmptyJSONString(item["arguments"]) {
				var v any
				_ = json.Unmarshal(item["arguments"], &v)
				data, _ := json.Marshal(v)
				args = string(data)
			}
			if len(item["arguments"]) == 0 {
				args = "{}"
			}
			call := newChatToolCall(callID, name, args)
			if n := len(out); n > 0 && out[n-1].Role == "assistant" {
				out[n-1].ToolCalls = append(out[n-1].ToolCalls, call)
			} else {
				out = append(out, ChatMessage{Role: "assistant", Content: mustRaw(""), ToolCalls: []ChatToolCall{call}})
			}
		case "function_call_output":
			callID := rawString(item["call_id"])
			if callID == "" {
				return nil, invalid("function_call_output items need call_id")
			}
			content, err := outputToChat(item["output"])
			if err != nil {
				return nil, err
			}
			out = append(out, ChatMessage{Role: "tool", ToolCallID: callID, Content: content})
		case "reasoning":
			// Earlier reasoning is never replayed into a CLI.
		default:
			return nil, invalid(fmt.Sprintf("Unsupported input item type: %s", typ))
		}
	}
	return out, nil
}

// ToolsToChat translates Responses function tools to Chat tools. A nil
// result with nil error means the field was absent.
func ToolsToChat(raw json.RawMessage, present bool) ([]json.RawMessage, error) {
	if !present || isNull(raw) {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, invalid("tools must be an array")
	}
	out := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		t, ok := rawObject(entry)
		if !ok || rawString(t["type"]) != "function" {
			typ := ""
			if ok {
				typ = rawString(t["type"])
			}
			return nil, invalid(fmt.Sprintf("Only function tools are supported (got %s)", typ))
		}
		fn := map[string]json.RawMessage{"name": mustRaw(rawString(t["name"]))}
		if v := rawString(t["description"]); v != "" {
			fn["description"] = mustRaw(v)
		}
		if p, ok := t["parameters"]; ok {
			fn["parameters"] = p
		}
		out = append(out, mustRaw(map[string]any{"type": "function", "function": fn}))
	}
	return out, nil
}

func toolChoiceToChat(raw json.RawMessage, present bool) (json.RawMessage, bool, error) {
	if !present || isNull(raw) {
		return nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return mustRaw(s), true, nil
	}
	obj, ok := rawObject(raw)
	if ok && rawString(obj["type"]) == "function" && rawString(obj["name"]) != "" {
		return mustRaw(map[string]any{"type": "function", "function": map[string]any{"name": rawString(obj["name"])}}), true, nil
	}
	return nil, false, invalid("Unsupported tool_choice")
}

func storedContent(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var kept []string
	for _, raw := range parts {
		p, ok := rawObject(raw)
		if !ok {
			continue
		}
		switch rawString(p["type"]) {
		case "text":
			if t := rawString(p["text"]); t != "" {
				kept = append(kept, t)
			}
		case "image_url":
			kept = append(kept, "[image]")
		case "file":
			name := ""
			if f, ok := rawObject(p["file"]); ok {
				name = rawString(f["filename"])
			}
			if name != "" {
				kept = append(kept, "[file: "+name+"]")
			} else {
				kept = append(kept, "[file]")
			}
		}
	}
	return strings.Join(kept, string([]byte{10}))
}

// StoredMessages returns the text-only copy kept in the store. Attachments
// are never persisted.
func StoredMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for _, m := range messages {
		out = append(out, ChatMessage{
			Role: m.Role, Content: mustRaw(storedContent(m.Content)),
			ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID,
		})
	}
	return out
}

// ParsedRequest is a validated Responses request in chat form.
type ParsedRequest struct {
	ChatFields  map[string]json.RawMessage
	Messages    []ChatMessage
	NewMessages []ChatMessage
	Previous    *Record
	FallbackKey string
	Stream      bool
}

// DomainMessages returns all parsed messages in service form.
func (p *ParsedRequest) DomainMessages() []domain.Message {
	out := make([]domain.Message, 0, len(p.Messages))
	for _, m := range p.Messages {
		out = append(out, m.Domain())
	}
	return out
}

func decodeAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	_ = json.Unmarshal(raw, &v)
	return v
}

// ParseRequest validates a Responses request and translates it to a chat
// body, resolving any previous_response_id chain from the store.
func ParseRequest(fields map[string]json.RawMessage, store *Store) (*ParsedRequest, error) {
	if raw, ok := fields["background"]; ok {
		var bg bool
		if err := json.Unmarshal(raw, &bg); err == nil && bg {
			return nil, invalid("background responses are not supported")
		}
	}
	input, hasInput := fields["input"]
	if !hasInput || isNull(input) || isEmptyJSONString(input) {
		return nil, invalid("input required")
	}
	var inputItems []json.RawMessage
	if err := json.Unmarshal(input, &inputItems); err == nil && len(inputItems) == 0 {
		return nil, invalid("input required")
	}
	if text, ok := rawObject(fields["text"]); ok {
		if format, ok := rawObject(text["format"]); ok {
			if typ := rawString(format["type"]); typ != "" && typ != "text" {
				return nil, invalid("Only the text output format is supported")
			}
		}
	}
	if raw, ok := fields["instructions"]; ok && !isNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, invalid("instructions must be a string")
		}
	}
	previousID := ""
	if raw, ok := fields["previous_response_id"]; ok && !isNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, invalid("previous_response_id must be a string")
		}
		previousID = s
	}
	conversation := ""
	conversationPresent := false
	if raw, ok := fields["conversation"]; ok && !isNull(raw) {
		conversationPresent = true
		if s := rawString(raw); s != "" {
			conversation = s
		} else if obj, ok := rawObject(raw); ok {
			conversation = rawString(obj["id"])
		}
	}
	if previousID != "" && conversationPresent {
		return nil, invalid("conversation and previous_response_id cannot be used together")
	}
	if raw, ok := fields["reasoning"]; ok && !isNull(raw) {
		if _, ok := rawObject(raw); !ok {
			return nil, invalid("reasoning must be an object")
		}
	}
	var previous *Record
	var history []ChatMessage
	if previousID != "" {
		previous = store.Get(previousID)
		if previous == nil {
			return nil, invalid(fmt.Sprintf("Previous response with id '%s' not found.", previousID))
		}
		history = store.History(previousID)
	}
	newMessages, err := InputToMessages(input)
	if err != nil {
		return nil, err
	}
	messages := append(append([]ChatMessage{}, history...), newMessages...)
	hasUserOrTool := false
	for _, m := range messages {
		if m.Role == "user" || m.Role == "tool" {
			hasUserOrTool = true
		}
	}
	if !hasUserOrTool {
		return nil, invalid("input needs a user message or a function_call_output")
	}
	tools, err := ToolsToChat(fields["tools"], hasField(fields, "tools"))
	if err != nil {
		return nil, err
	}
	toolChoice, hasChoice, err := toolChoiceToChat(fields["tool_choice"], hasField(fields, "tool_choice"))
	if err != nil {
		return nil, err
	}
	if len(tools) == 0 {
		tools = nil
		if s := rawString(toolChoice); s == "auto" || s == "none" {
			toolChoice, hasChoice = nil, false
		}
	}
	chatFields := map[string]json.RawMessage{}
	if raw, ok := fields["model"]; ok {
		chatFields["model"] = raw
	}
	chatFields["messages"] = mustRaw(messages)
	stream := false
	if raw, ok := fields["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	chatFields["stream"] = mustRaw(stream)
	if instructions := rawString(fields["instructions"]); instructions != "" {
		chatFields["system"] = mustRaw(instructions)
	}
	if tools != nil {
		chatFields["tools"] = mustRaw(tools)
	}
	if hasChoice {
		chatFields["tool_choice"] = toolChoice
	}
	if raw, ok := fields["parallel_tool_calls"]; ok {
		chatFields["parallel_tool_calls"] = raw
	}
	if reasoning, ok := rawObject(fields["reasoning"]); ok {
		if effort := rawString(reasoning["effort"]); effort != "" {
			chatFields["reasoning_effort"] = mustRaw(effort)
		}
	}
	if user := fields["user"]; len(user) > 0 && !isNull(user) && !isEmptyJSONString(user) {
		chatFields["user"] = user
	}
	if _, ok := rawObject(fields["metadata"]); ok {
		chatFields["metadata"] = fields["metadata"]
	}
	if conversation != "" {
		chatFields["conversation_id"] = mustRaw(conversation)
	}
	fallback := ""
	if previous != nil {
		if previous.Key != nil && *previous.Key != "" {
			fallback = *previous.Key
		} else {
			fallback = "resp-" + previous.ID
		}
	}
	return &ParsedRequest{
		ChatFields: chatFields, Messages: messages, NewMessages: newMessages,
		Previous: previous, FallbackKey: fallback, Stream: stream,
	}, nil
}

func hasField(fields map[string]json.RawMessage, key string) bool {
	_, ok := fields[key]
	return ok
}

// Usage renders domain usage in Responses shape.
func Usage(u *domain.Usage) map[string]any {
	cached, input, output, reasoning := 0, 0, 0, 0
	if u != nil {
		cached = u.CacheReadInputTokens
		input = u.InputTokens + cached + u.CacheCreationInputTokens
		output = u.OutputTokens
		reasoning = u.ReasoningTokens
	}
	return map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
		"total_tokens":          input + output,
	}
}

// ResponseObject builds a Responses response object, echoing request fields.
func ResponseObject(fields map[string]json.RawMessage, id string, createdAt int64, model, status string, output []any, usage map[string]any, errValue any) map[string]any {
	if status == "" {
		status = "in_progress"
	}
	if output == nil {
		output = []any{}
	}
	reasoning, _ := rawObject(fields["reasoning"])
	tools := []any{}
	if raw, ok := fields["tools"]; ok {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err == nil {
			toolsAny := decodeAny(raw)
			if list, ok := toolsAny.([]any); ok {
				tools = list
			}
		}
	}
	metadata := map[string]any{}
	if obj, ok := rawObject(fields["metadata"]); ok {
		_ = obj
		if m, ok := decodeAny(fields["metadata"]).(map[string]any); ok {
			metadata = m
		}
	}
	store := true
	if raw, ok := fields["store"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			store = b
		}
	}
	toolChoice := decodeAny(fields["tool_choice"])
	if toolChoice == nil {
		toolChoice = "auto"
	}
	parallel := decodeAny(fields["parallel_tool_calls"])
	if parallel == nil {
		parallel = true
	}
	return map[string]any{
		"id": id, "object": "response", "created_at": createdAt, "status": status,
		"background": false, "error": errValue, "incomplete_details": nil,
		"instructions":         nullableString(fields["instructions"]),
		"max_output_tokens":    decodeAny(fields["max_output_tokens"]),
		"model":                model,
		"output":               output,
		"parallel_tool_calls":  parallel,
		"previous_response_id": nullableString(fields["previous_response_id"]),
		"reasoning": map[string]any{
			"effort": nullableString(reasoning["effort"]), "summary": nullableString(reasoning["summary"]),
		},
		"store": store, "temperature": decodeAny(fields["temperature"]),
		"text":        map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": toolChoice, "tools": tools,
		"top_p": decodeAny(fields["top_p"]), "truncation": "disabled",
		"usage": usage, "user": decodeAny(fields["user"]), "metadata": metadata,
	}
}

func nullableString(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

// ToolCall is one completed function call for output rendering.
type ToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

func fcID(callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return "fc_" + hex.EncodeToString(sum[:])[:32]
}

func messageItem(id, text, status string) map[string]any {
	if status == "" {
		status = "completed"
	}
	return map[string]any{
		"id": id, "type": "message", "status": status, "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}},
	}
}

func functionCallItem(call ToolCall, status string) map[string]any {
	if status == "" {
		status = "completed"
	}
	return map[string]any{
		"id": fcID(call.CallID), "type": "function_call", "status": status,
		"call_id": call.CallID, "name": call.Name, "arguments": call.Arguments,
	}
}

// Output builds the non-streaming output item list.
func Output(text, thinking string, toolCalls []ToolCall) []any {
	out := []any{}
	if thinking != "" {
		out = append(out, map[string]any{
			"id": "rs_" + hexID(), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": thinking}},
		})
	}
	if text != "" || len(toolCalls) == 0 {
		out = append(out, messageItem("msg_"+hexID(), text, "completed"))
	}
	for _, call := range toolCalls {
		out = append(out, functionCallItem(call, "completed"))
	}
	return out
}
