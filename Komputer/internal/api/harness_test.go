package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func openAITool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name, "description": "Look up a value on this computer",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
				"required":   []string{"query"}, "additionalProperties": false,
			},
		},
	}
}

func anthropicTool(name string) map[string]any {
	return map[string]any{
		"name": name, "description": "Look up a value on this computer",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"}, "additionalProperties": false,
		},
	}
}

func TestGoAPIHarnessOpenAIToolCycle(t *testing.T) {
	f := newFixture(t)
	tools := []any{openAITool("lookup")}
	firstBody := jsonBody(t, map[string]any{
		"model":    "komputer-sonnet",
		"messages": []any{map[string]any{"role": "user", "content": "CLIENT_TOOL"}},
		"tools":    tools,
	})
	status, headers, data := f.request(t, "/v1/chat/completions", firstBody, nil)
	if status != 200 {
		t.Fatalf("first status %d: %s", status, data)
	}
	if headers.Get("X-Komputer-Mode") != "harness" || headers.Get("X-Komputer-Run") == "" {
		t.Fatalf("harness headers: %v", headers)
	}
	var first struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decode(t, data, &first)
	if len(first.Choices) != 1 || first.Choices[0].FinishReason != "tool_calls" || len(first.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("first response: %s", data)
	}
	call := first.Choices[0].Message.ToolCalls[0]
	if call.Function.Name != "lookup" || !strings.Contains(call.Function.Arguments, "hello") {
		t.Fatalf("tool call: %+v", call)
	}
	nextBody := jsonBody(t, map[string]any{
		"model": "komputer-sonnet",
		"messages": []any{
			map[string]any{"role": "user", "content": "CLIENT_TOOL"},
			map[string]any{"role": "assistant", "content": "checking ", "tool_calls": []any{map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": call.Function.Arguments},
			}}},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "local go result"},
		},
		"tools": tools,
	})
	status, _, data = f.request(t, "/v1/chat/completions", nextBody, nil)
	if status != 200 {
		t.Fatalf("continuation status %d: %s", status, data)
	}
	var next struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decode(t, data, &next)
	if len(next.Choices) != 1 || next.Choices[0].FinishReason != "stop" || !strings.Contains(next.Choices[0].Message.Content, "local go result") {
		t.Fatalf("continuation response: %s", data)
	}
	if calls := f.calls(t); len(calls) != 1 {
		t.Fatalf("fake Claude processes: %d", len(calls))
	}
}

func TestGoAPIHarnessAnthropicToolCycle(t *testing.T) {
	f := newFixture(t)
	tools := []any{anthropicTool("lookup")}
	status, _, data := f.request(t, "/v1/messages", jsonBody(t, map[string]any{
		"model":    "komputer-sonnet",
		"messages": []any{map[string]any{"role": "user", "content": "CLIENT_TOOL"}},
		"tools":    tools,
	}), nil)
	if status != 200 {
		t.Fatalf("first status %d: %s", status, data)
	}
	var first struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	decode(t, data, &first)
	callID := ""
	for _, block := range first.Content {
		if block.Type == "tool_use" {
			callID = block.ID
			if block.Name != "lookup" {
				t.Fatalf("tool name: %+v", block)
			}
		}
	}
	if first.StopReason != "tool_use" || callID == "" {
		t.Fatalf("first response: %s", data)
	}
	status, _, data = f.request(t, "/v1/messages", jsonBody(t, map[string]any{
		"model": "komputer-sonnet",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": callID, "content": "anthropic local result",
		}}}},
		"tools": tools,
	}), nil)
	if status != 200 {
		t.Fatalf("continuation status %d: %s", status, data)
	}
	var next struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	decode(t, data, &next)
	found := false
	for _, block := range next.Content {
		if strings.Contains(block.Text, "anthropic local result") {
			found = true
		}
	}
	if next.StopReason != "end_turn" || !found {
		t.Fatalf("continuation response: %s", data)
	}
	if calls := f.calls(t); len(calls) != 1 {
		t.Fatalf("fake Claude processes: %d", len(calls))
	}
}

func TestGoAPIHarnessCancelRun(t *testing.T) {
	f := newFixture(t)
	status, headers, data := f.request(t, "/v1/chat/completions", jsonBody(t, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "CLIENT_TOOL"}},
		"tools":    []any{openAITool("lookup")},
	}), nil)
	if status != 200 {
		t.Fatalf("first status %d: %s", status, data)
	}
	req, _ := http.NewRequest(http.MethodDelete, f.base+"/v1/tool-runs/"+headers.Get("X-Komputer-Run"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	decode(t, body, &cancelled)
	if resp.StatusCode != 200 || !cancelled.Cancelled {
		t.Fatalf("cancel status %d: %s", resp.StatusCode, body)
	}
}
