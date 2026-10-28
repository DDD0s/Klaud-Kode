// Package harness implements client-executed tools: the CLI sees an
// isolated loopback MCP server, while the API client receives ordinary tool
// calls and supplies results on its next request. This package never
// executes a client-provided command itself.
package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/DDD0s/Klaud-Kode/Komputer/internal/domain"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func toolError(status int, message string) *domain.APIError {
	return &domain.APIError{Status: status, Type: "invalid_request_error", Message: message}
}

func typedToolError(status int, message, typ string) *domain.APIError {
	return &domain.APIError{Status: status, Type: typ, Message: message, Detail: message}
}

func digestJSON(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func rawString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func mustRaw(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

var imageDataPattern = regexp.MustCompile(`(?s)^data:(image/[^;,]+);base64,(.*)$`)

func resultContent(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": ""})}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []json.RawMessage{mustRaw(map[string]any{"type": "text", "text": s})}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, toolError(400, "Tool result content must be text or content blocks")
	}
	out := make([]json.RawMessage, 0, len(parts))
	for _, part := range parts {
		m, ok := rawObject(part)
		if !ok {
			return nil, toolError(400, "Tool results support text and base64 images only")
		}
		switch rawString(m["type"]) {
		case "text":
			text, ok := func() (string, bool) { var v string; err := json.Unmarshal(m["text"], &v); return v, err == nil }()
			if !ok {
				return nil, toolError(400, "Tool results support text and base64 images only")
			}
			out = append(out, mustRaw(map[string]any{"type": "text", "text": text}))
		case "image":
			source, ok := rawObject(m["source"])
			if !ok || rawString(source["type"]) != "base64" {
				return nil, toolError(400, "Tool results support text and base64 images only")
			}
			data := rawString(source["data"])
			if data == "" {
				return nil, toolError(400, "Tool results support text and base64 images only")
			}
			out = append(out, mustRaw(map[string]any{
				"type": "image", "data": data, "mimeType": rawString(source["media_type"]),
			}))
		case "image_url":
			u := rawString(m["image_url"])
			if u == "" {
				if obj, ok := rawObject(m["image_url"]); ok {
					u = rawString(obj["url"])
				}
			}
			match := imageDataPattern.FindStringSubmatch(u)
			if match == nil {
				return nil, toolError(400, "Tool results support text and base64 images only")
			}
			out = append(out, mustRaw(map[string]any{"type": "image", "mimeType": match[1], "data": match[2]}))
		default:
			return nil, toolError(400, "Tool results support text and base64 images only")
		}
	}
	return out, nil
}

// Tool is one client-defined tool exposed to the CLI under an alias.
type Tool struct {
	Name        string
	Alias       string
	Description string
	Schema      json.RawMessage
	validator   *jsonschema.Schema
}

// Validate reports whether args are a JSON object matching the schema.
func (t Tool) Validate(args json.RawMessage) bool {
	if t.validator == nil {
		return false
	}
	if _, ok := rawObject(args); !ok {
		return false
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	return err == nil && t.validator.Validate(v) == nil
}

type denyLoader struct{}

func (denyLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are unsupported: %s", url)
}

// CompileTools compiles every tool schema before any process starts.
func CompileTools(tools []Tool) ([]Tool, error) {
	compiled := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(tool.Schema))
		if err != nil {
			return nil, toolError(400, "Invalid or unsupported input schema for tool "+tool.Name)
		}
		compiler := jsonschema.NewCompiler()
		compiler.AssertFormat()
		compiler.UseLoader(denyLoader{})
		if err := compiler.AddResource("tool.json", doc); err != nil {
			return nil, toolError(400, "Invalid or unsupported input schema for tool "+tool.Name)
		}
		schema, err := compiler.Compile("tool.json")
		if err != nil {
			return nil, toolError(400, "Invalid or unsupported input schema for tool "+tool.Name)
		}
		tool.validator = schema
		compiled = append(compiled, tool)
	}
	return compiled, nil
}

// Result is one client-supplied tool result.
type Result struct {
	ID      string
	Content []json.RawMessage
	IsError bool
}

// Policy is the tool-choice policy for one response segment.
type Policy struct {
	Choice   string
	Name     string
	Parallel bool
}

// Spec is one recognized client-tool request shape.
type Spec struct {
	Protocol string
	HasTools bool
	Tools    []Tool
	Results  []Result
	Policy
}

func toolDigest(tools []Tool) string {
	type digestTool struct {
		Name        string
		Alias       string
		Description string
		Schema      any
	}
	items := make([]digestTool, 0, len(tools))
	for _, t := range tools {
		var schema any
		_ = json.Unmarshal(t.Schema, &schema)
		items = append(items, digestTool{t.Name, t.Alias, t.Description, schema})
	}
	return digestJSON(items)
}

// ValidateChoice checks a policy against an available tool set.
func ValidateChoice(spec *Spec, tools []Tool) error {
	if spec.Choice == "named" {
		found := false
		for _, t := range tools {
			if t.Name == spec.Name {
				found = true
			}
		}
		if !found {
			return toolError(400, "tool_choice names an unavailable tool")
		}
	}
	if spec.Choice == "required" && len(tools) == 0 {
		return toolError(400, "Required tool choice needs at least one tool")
	}
	return nil
}

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type messageObject struct {
	role       string
	content    json.RawMessage
	toolCallID string
	isError    bool
}

func parseMessageObjects(fields map[string]json.RawMessage) []messageObject {
	var rawItems []json.RawMessage
	if raw, ok := fields["messages"]; ok {
		_ = json.Unmarshal(raw, &rawItems)
	}
	out := make([]messageObject, 0, len(rawItems))
	for _, raw := range rawItems {
		m, ok := rawObject(raw)
		if !ok {
			out = append(out, messageObject{})
			continue
		}
		var isErr bool
		_ = json.Unmarshal(m["is_error"], &isErr)
		out = append(out, messageObject{
			role: rawString(m["role"]), content: m["content"],
			toolCallID: rawString(m["tool_call_id"]), isError: isErr,
		})
	}
	return out
}

// ParseRequest recognizes tool mode without changing ordinary requests. It
// returns a nil spec when the request is an ordinary chat/Body request.
func ParseRequest(protocol string, fields map[string]json.RawMessage) (*Spec, error) {
	messages := parseMessageObjects(fields)
	var results []Result
	if len(messages) > 0 {
		last := messages[len(messages)-1]
		if protocol == "openai" && last.role == "tool" {
			for i := len(messages) - 1; i >= 0 && messages[i].role == "tool"; i-- {
				content, err := resultContent(messages[i].content)
				if err != nil {
					return nil, err
				}
				results = append([]Result{{ID: messages[i].toolCallID, Content: content, IsError: messages[i].isError}}, results...)
			}
		} else if protocol == "anthropic" && last.role == "user" {
			var blocks []json.RawMessage
			if err := json.Unmarshal(last.content, &blocks); err == nil {
				var toolBlocks []map[string]json.RawMessage
				for _, b := range blocks {
					if m, ok := rawObject(b); ok && rawString(m["type"]) == "tool_result" {
						toolBlocks = append(toolBlocks, m)
					}
				}
				if len(toolBlocks) > 0 {
					if len(toolBlocks) != len(blocks) {
						return nil, toolError(400, "Send tool results without a new user message in the same turn")
					}
					for _, m := range toolBlocks {
						content, err := resultContent(m["content"])
						if err != nil {
							return nil, err
						}
						var isErr bool
						_ = json.Unmarshal(m["is_error"], &isErr)
						results = append(results, Result{ID: rawString(m["tool_use_id"]), Content: content, IsError: isErr})
					}
				}
			}
		}
	}
	if _, ok := fields["functions"]; ok {
		return nil, toolError(400, "Use tools and tool_choice instead of legacy function fields")
	}
	if _, ok := fields["function_call"]; ok {
		return nil, toolError(400, "Use tools and tool_choice instead of legacy function fields")
	}
	toolsRaw, hasTools := fields["tools"]
	var toolEntries []json.RawMessage
	if hasTools {
		if err := json.Unmarshal(toolsRaw, &toolEntries); err != nil {
			return nil, toolError(400, "tools must be an array")
		}
	}
	_, hasChoice := fields["tool_choice"]
	if len(toolEntries) == 0 && !hasChoice && len(results) == 0 {
		return nil, nil
	}
	if raw, ok := fields["n"]; ok {
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil || n != 1 {
			return nil, toolError(400, "Client tool mode supports n=1 only")
		}
	}
	var tools []Tool
	if hasTools {
		if len(toolEntries) > 128 {
			return nil, toolError(400, "At most 128 client tools are supported")
		}
		seen := map[string]bool{}
		for i, entry := range toolEntries {
			entryObj, ok := rawObject(entry)
			if !ok {
				return nil, toolError(400, "Tool names must be unique and contain 1-64 letters, digits, underscores or hyphens")
			}
			if protocol == "openai" && rawString(entryObj["type"]) != "function" {
				return nil, toolError(400, "Only function tools are supported")
			}
			if protocol == "anthropic" {
				if typ, present := entryObj["type"]; present && rawString(typ) != "custom" {
					return nil, toolError(400, "Only client-defined tools are supported")
				}
			}
			fn := entryObj
			if protocol == "openai" {
				var ok bool
				fn, ok = rawObject(entryObj["function"])
				if !ok {
					return nil, toolError(400, "Tool names must be unique and contain 1-64 letters, digits, underscores or hyphens")
				}
			}
			name := rawString(fn["name"])
			if !toolNamePattern.MatchString(name) || seen[name] {
				return nil, toolError(400, "Tool names must be unique and contain 1-64 letters, digits, underscores or hyphens")
			}
			seen[name] = true
			schema := fn["parameters"]
			if protocol == "anthropic" {
				schema = fn["input_schema"]
			}
			if len(schema) == 0 || string(schema) == "null" {
				schema = mustRaw(map[string]any{"type": "object", "properties": map[string]any{}})
			}
			schemaObj, ok := rawObject(schema)
			if !ok || rawString(schemaObj["type"]) != "object" {
				return nil, toolError(400, "Tool input schema must have type object")
			}
			description := ""
			if raw, present := fn["description"]; present {
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return nil, toolError(400, "Tool description must be a string")
				}
				description = s
			}
			alias := fmt.Sprintf("k_%d_%s", i, digestJSON(name)[:12])
			tools = append(tools, Tool{Name: name, Alias: alias, Description: description, Schema: append(json.RawMessage{}, schema...)})
		}
	}
	choice, name, parallel := "auto", "", true
	requested, requestedPresent := fields["tool_choice"]
	if protocol == "openai" {
		if requestedPresent {
			var s string
			if err := json.Unmarshal(requested, &s); err == nil {
				choice = s
			} else {
				obj, ok := rawObject(requested)
				fn, fnOK := rawObject(obj["function"])
				fnName := rawString(fn["name"])
				if !ok || rawString(obj["type"]) != "function" || !fnOK || fnName == "" {
					return nil, toolError(400, "Invalid tool_choice")
				}
				choice, name = "named", fnName
			}
		}
		if raw, present := fields["parallel_tool_calls"]; present {
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, toolError(400, "parallel_tool_calls must be boolean")
			}
			parallel = b
		}
	} else if requestedPresent {
		obj, ok := rawObject(requested)
		if !ok {
			return nil, toolError(400, "tool_choice must be an object")
		}
		switch rawString(obj["type"]) {
		case "any":
			choice = "required"
		case "tool":
			choice = "named"
		default:
			choice = rawString(obj["type"])
		}
		name = rawString(obj["name"])
		if raw, present := obj["disable_parallel_tool_use"]; present {
			var b bool
			if err := json.Unmarshal(raw, &b); err != nil {
				return nil, toolError(400, "disable_parallel_tool_use must be boolean")
			}
			parallel = !b
		}
	}
	switch choice {
	case "auto", "none", "required", "named":
	default:
		return nil, toolError(400, "Unsupported tool_choice")
	}
	ids := map[string]bool{}
	for _, r := range results {
		if r.ID == "" || ids[r.ID] {
			return nil, toolError(400, "Tool results require unique tool call IDs")
		}
		ids[r.ID] = true
	}
	return &Spec{
		Protocol: protocol, HasTools: hasTools, Tools: tools, Results: results,
		Policy: Policy{Choice: choice, Name: name, Parallel: parallel},
	}, nil
}
