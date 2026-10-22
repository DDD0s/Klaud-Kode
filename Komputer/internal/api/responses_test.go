package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func (f fixture) method(t *testing.T, method, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, data
}

type responseObject struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Status    string `json:"status"`
	Model     string `json:"model"`
	Instr     string `json:"instructions"`
	Store     bool   `json:"store"`
	Previous  string `json:"previous_response_id"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Output []struct {
		Type    string `json:"type"`
		CallID  string `json:"call_id"`
		Name    string `json:"name"`
		Args    string `json:"arguments"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
		InputDetails struct {
			Cached int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

func (r responseObject) text() string {
	for i := len(r.Output) - 1; i >= 0; i-- {
		if r.Output[i].Type == "message" && len(r.Output[i].Content) > 0 {
			return r.Output[i].Content[0].Text
		}
	}
	return ""
}

func TestGoAPIResponsesNonStreamingAndRetrieval(t *testing.T) {
	f := newFixture(t)
	status, _, data := f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-sonnet", "input": "hi", "instructions": "BE BRIEF",
		"reasoning": map[string]any{"effort": "high"}, "metadata": map[string]any{"tag": "x"},
	}), nil)
	if status != 200 {
		t.Fatalf("status %d: %s", status, data)
	}
	var body responseObject
	decode(t, data, &body)
	if !regexp.MustCompile(`^resp_[0-9a-f]{32}$`).MatchString(body.ID) || body.Object != "response" || body.Status != "completed" || body.Model != "komputer-sonnet" {
		t.Fatalf("response identity: %s", data)
	}
	if body.Instr != "BE BRIEF" || body.Reasoning.Effort != "high" || body.text() != "echo:hi" {
		t.Fatalf("response body: %s", data)
	}
	if body.Usage.InputTokens != 10 || body.Usage.InputDetails.Cached != 3 || body.Usage.OutputTokens != 11 || body.Usage.TotalTokens != 21 {
		t.Fatalf("usage: %+v", body.Usage)
	}
	calls := f.calls(t)
	if len(calls) != 1 {
		t.Fatalf("calls: %d", len(calls))
	}
	args := calls[0].Args
	if len(args) < 5 || strings.Join(args[len(args)-5:], " ") != "--model sonnet --effort high --no-session-persistence" {
		t.Fatalf("args tail: %q", args)
	}
	if !strings.HasSuffix(calls[0].System, "BE BRIEF") {
		t.Fatalf("system does not end with instructions: %q", calls[0].System)
	}
	status, data = f.method(t, http.MethodGet, "/v1/responses/"+body.ID)
	if status != 200 || !strings.Contains(string(data), body.ID) {
		t.Fatalf("retrieve %d: %s", status, data)
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestGoAPIResponsesPreviousChainResumesSession(t *testing.T) {
	f := newFixture(t)
	post := func(body map[string]any) (responseObject, http.Header) {
		status, headers, data := f.request(t, "/v1/responses", jsonBody(t, body), nil)
		if status != 200 {
			t.Fatalf("status %d: %s", status, data)
		}
		var out responseObject
		decode(t, data, &out)
		return out, headers
	}
	r1, _ := post(map[string]any{"model": "komputer-opus", "input": "hi"})
	r2, headers := post(map[string]any{"model": "komputer-opus", "input": "second", "previous_response_id": r1.ID})
	if r2.Previous != r1.ID {
		t.Fatalf("previous: %q", r2.Previous)
	}
	if headers.Get("X-Komputer-Session") != "resp-"+r1.ID {
		t.Fatalf("session header: %v", headers)
	}
	calls := f.calls(t)
	if len(calls) != 2 {
		t.Fatalf("calls: %d", len(calls))
	}
	sid := argAfter(calls[1].Args, "--session-id")
	if sid == "" {
		t.Fatalf("second call args: %q", calls[1].Args)
	}
	nl := string([]byte{10})
	wantPrompt := strings.Join([]string{"Earlier conversation for context:", "", "User: hi", "", "Assistant: echo:hi", "", "---", "", "second"}, nl)
	if calls[1].Prompt != wantPrompt {
		t.Fatalf("second prompt %q, want %q", calls[1].Prompt, wantPrompt)
	}
	r3, _ := post(map[string]any{
		"model": "komputer-opus", "previous_response_id": r2.ID,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "third"}}}},
	})
	calls = f.calls(t)
	if len(calls) != 3 || argAfter(calls[2].Args, "--resume") != sid || calls[2].Prompt != "third" {
		t.Fatalf("resume call: %+v", calls[2])
	}
	if !strings.Contains(r3.text(), "|now=third") {
		t.Fatalf("third text: %q", r3.text())
	}
	status, _, data := f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-opus", "input": "x", "previous_response_id": "resp_missing",
	}), nil)
	if status != 400 || !strings.Contains(string(data), "Previous response with id 'resp_missing' not found.") {
		t.Fatalf("missing previous %d: %s", status, data)
	}
}

type responseEvent struct {
	Type     string          `json:"type"`
	Sequence int             `json:"sequence_number"`
	Delta    string          `json:"delta"`
	Response *responseObject `json:"response"`
}

func parseResponseEvents(t *testing.T, data []byte) []responseEvent {
	t.Helper()
	var events []responseEvent
	for _, block := range strings.Split(string(data), string([]byte{10, 10})) {
		if !strings.HasPrefix(block, "event: ") {
			continue
		}
		lines := strings.Split(block, string([]byte{10}))
		if len(lines) < 2 {
			t.Fatalf("bad event block %q", block)
		}
		var ev responseEvent
		decode(t, []byte(strings.TrimPrefix(lines[1], "data: ")), &ev)
		if strings.TrimPrefix(lines[0], "event: ") != ev.Type {
			t.Fatalf("event header mismatch: %q", block)
		}
		events = append(events, ev)
	}
	return events
}

func TestGoAPIResponsesStreaming(t *testing.T) {
	f := newFixture(t)
	status, headers, data := f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-claude", "input": "THINK please", "stream": true,
	}), nil)
	if status != 200 {
		t.Fatalf("status %d: %s", status, data)
	}
	if !strings.Contains(headers.Get("Content-Type"), "text/event-stream") || strings.Contains(string(data), "[DONE]") {
		t.Fatalf("stream headers/body: %v %s", headers, data)
	}
	events := parseResponseEvents(t, data)
	for i, ev := range events {
		if ev.Sequence != i {
			t.Fatalf("sequence %d at %d", ev.Sequence, i)
		}
	}
	var types []string
	for _, ev := range events {
		if len(types) == 0 || types[len(types)-1] != ev.Type {
			types = append(types, ev.Type)
		}
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types: %v", types)
	}
	done := events[len(events)-1].Response
	if done == nil || done.Status != "completed" || len(done.Output) != 2 || done.Output[0].Type != "reasoning" || done.Output[1].Type != "message" {
		t.Fatalf("completed response: %s", data)
	}
	text := ""
	for _, ev := range events {
		if ev.Type == "response.output_text.delta" {
			text += ev.Delta
		}
	}
	if text != "echo:THINK please" || done.Usage.OutputTokens != 11 {
		t.Fatalf("stream text/usage: %q %+v", text, done.Usage)
	}
}

func responsesTool() map[string]any {
	return map[string]any{
		"type": "function", "name": "lookup", "description": "Look up",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required": []string{"query"},
		},
	}
}

func TestGoAPIResponsesFunctionToolsThroughPrevious(t *testing.T) {
	f := newFixture(t)
	tools := []any{responsesTool()}
	status, headers, data := f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-sonnet", "input": "CLIENT_TOOL find it", "tools": tools, "tool_choice": "required",
	}), nil)
	if status != 200 {
		t.Fatalf("first status %d: %s", status, data)
	}
	if headers.Get("X-Komputer-Mode") != "harness" {
		t.Fatalf("headers: %v", headers)
	}
	var first responseObject
	decode(t, data, &first)
	if !regexp.MustCompile(`^resp_[0-9a-f]{32}_0$`).MatchString(first.ID) {
		t.Fatalf("harness response id: %q", first.ID)
	}
	var callID string
	for _, item := range first.Output {
		if item.Type == "function_call" {
			callID = item.CallID
			if item.Name != "lookup" || !strings.Contains(item.Args, "hello") {
				t.Fatalf("call item: %+v", item)
			}
		}
	}
	if callID == "" {
		t.Fatalf("no function call: %s", data)
	}
	status, _, data = f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-sonnet", "tools": tools, "previous_response_id": first.ID,
		"input": []any{map[string]any{"type": "function_call_output", "call_id": callID, "output": "forty-two"}},
	}), nil)
	if status != 200 {
		t.Fatalf("continuation status %d: %s", status, data)
	}
	var final responseObject
	decode(t, data, &final)
	if !strings.Contains(final.text(), "forty-two") {
		t.Fatalf("final text: %s", data)
	}
	if calls := f.calls(t); len(calls) != 1 {
		t.Fatalf("fake Claude processes: %d", len(calls))
	}
}

func TestGoAPIResponsesStoreTextOnlyAndDelete(t *testing.T) {
	f := newFixture(t)
	png := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	status, _, data := f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-claude",
		"input": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "what is this"},
			map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + png},
		}}},
	}), nil)
	if status != 200 {
		t.Fatalf("image status %d: %s", status, data)
	}
	var storedResp responseObject
	decode(t, data, &storedResp)
	calls := f.calls(t)
	if len(calls) != 1 || len(calls[0].Attachments) != 1 || calls[0].Attachments[0].Type != "image" {
		t.Fatalf("fake attachments: %+v", calls)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, "responses.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), png) || !strings.Contains(string(raw), "[image]") {
		t.Fatalf("store persisted attachment data or omitted placeholder: %s", raw)
	}
	status, _, data = f.request(t, "/v1/responses", jsonBody(t, map[string]any{
		"model": "komputer-claude", "input": "not stored", "store": false,
	}), nil)
	if status != 200 {
		t.Fatalf("store:false status %d: %s", status, data)
	}
	var off responseObject
	decode(t, data, &off)
	if off.Store {
		t.Fatalf("store echo: %s", data)
	}
	if status, _ := f.method(t, http.MethodGet, "/v1/responses/"+off.ID); status != 404 {
		t.Fatalf("store:false retrieve status %d", status)
	}
	status, data = f.method(t, http.MethodDelete, "/v1/responses/"+storedResp.ID)
	if status != 200 || !strings.Contains(string(data), "deleted") {
		t.Fatalf("delete %d: %s", status, data)
	}
	if status, _ := f.method(t, http.MethodGet, "/v1/responses/"+storedResp.ID); status != 404 {
		t.Fatalf("deleted retrieve status %d", status)
	}
}
