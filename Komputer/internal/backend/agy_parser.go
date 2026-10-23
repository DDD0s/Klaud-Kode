package backend

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

type agyToolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
}

type agyStep struct {
	StepIndex      *int        `json:"step_index"`
	State          string      `json:"state"`
	StepType       string      `json:"step_type"`
	TextDelta      string      `json:"text_delta"`
	ToolName       string      `json:"tool_name"`
	ToolInfo       agyToolInfo `json:"tool_info"`
	ConversationID string      `json:"conversation_id"`
}

type agyResult struct {
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	ConversationID string `json:"conversation_id"`
	NumTurns       int    `json:"num_turns"`
	Usage          *struct {
		InputTokens    int `json:"input_tokens"`
		OutputTokens   int `json:"output_tokens"`
		ThinkingTokens int `json:"thinking_tokens"`
		CacheRead      int `json:"cache_read_tokens"`
	} `json:"usage"`
}

type agyEnvelope struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           *struct {
		ConversationID string `json:"conversation_id"`
		PermissionMode string `json:"permission_mode"`
	} `json:"init"`
	StepUpdate *agyStep   `json:"step_update"`
	Result     *agyResult `json:"result"`
}

func agyUsage(r *agyResult) *domain.Usage {
	if r == nil || r.Usage == nil {
		return nil
	}
	return &domain.Usage{
		InputTokens: r.Usage.InputTokens, CacheReadInputTokens: r.Usage.CacheRead,
		OutputTokens: r.Usage.OutputTokens, ReasoningTokens: r.Usage.ThinkingTokens,
	}
}

func firstString(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

var warnedAlwaysProceed atomic.Bool

type agyParser struct {
	backend      *Antigravity
	session      SessionRef
	requested    string
	sink         domain.Sink
	bail         func(*Failure)
	policy       *AGYPolicy
	guard        string
	conversation string
	always       bool
	result       *agyResult
	streamed     strings.Builder
	lastTextStep *int
	afterTool    bool
	activity     bool
	announced    map[int]bool
	flagged      map[int]bool
	bailed       bool
}

func (p *agyParser) Activity() bool { return p.activity }

func (p *agyParser) doBail(message, typ string) {
	if p.bailed {
		return
	}
	p.bailed = true
	if p.bail != nil {
		p.bail(&Failure{Message: message, Type: typ, Activity: p.activity})
	}
}

func agyDisplayToolName(st *agyStep) string {
	params := map[string]json.RawMessage{}
	_ = json.Unmarshal(st.ToolInfo.Parameters, &params)
	server := firstString(rawStringValue(params["ServerName"]), rawStringValue(params["server_name"]), rawStringValue(params["serverName"]))
	tool := firstString(rawStringValue(params["ToolName"]), rawStringValue(params["tool_name"]), rawStringValue(params["toolName"]))
	rawName := st.ToolName
	if rawName == "" {
		rawName = st.ToolInfo.Name
	}
	if rawName == "call_mcp_tool" && server != "" && tool != "" {
		return "mcp__" + server + "__" + tool
	}
	if rawName == "" {
		return "tool"
	}
	return rawName
}

func rawStringValue(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func (p *agyParser) hostTool(st *agyStep, name string) {
	if p.guard == "off" {
		return
	}
	key := -1
	if st.StepIndex != nil {
		key = *st.StepIndex
	}
	if p.flagged[key] {
		return
	}
	if st.State != "DONE" && !(p.always && !p.policy.Hooked) {
		return
	}
	raw := st.ToolName
	if raw == "" {
		raw = st.ToolInfo.Name
	}
	var args any
	_ = json.Unmarshal(st.ToolInfo.Parameters, &args)
	call := map[string]any{
		"toolCall":       map[string]any{"name": raw, "args": args},
		"conversationId": firstString(st.ConversationID, p.conversation),
	}
	verdict := DecideForPolicy(call, p.policy, p.backend.cfg.Home)
	if verdict == nil || verdict["decision"] != "deny" {
		return
	}
	p.flagged[key] = true
	permission := "unknown"
	if p.always {
		permission = "always-proceed"
	}
	message := fmt.Sprintf("agy ran its built-in tool %s on the service host (permission_mode %s)", name, permission)
	if p.guard == "stop" {
		p.doBail(message, "builtin_tool_blocked")
	} else {
		p.backend.logf("WARNING: %s", message)
	}
}

func (p *agyParser) Line(line string) {
	var ev agyEnvelope
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return
	}
	switch ev.Event {
	case "init":
		id := ev.ConversationID
		if ev.Init != nil && id == "" {
			id = ev.Init.ConversationID
		}
		if p.requested != "" && id != "" && id != p.requested {
			p.doBail(fmt.Sprintf("conversation %q not found (agy started %s)", p.requested, id), "session_not_found")
			return
		}
		if id != "" {
			p.conversation = id
		}
		if ev.Init != nil {
			p.always = ev.Init.PermissionMode == "always-proceed"
		}
		if p.always && warnedAlwaysProceed.CompareAndSwap(false, true) {
			p.backend.logf("WARNING: agy reports permission_mode always-proceed without --dangerously-skip-permissions: its built-in tools run on this host without asking")
		}
	case "step_update":
		st := ev.StepUpdate
		if st == nil {
			return
		}
		if st.StepType == "agent_response" || st.StepType == "tool" {
			p.activity = true
		}
		switch st.StepType {
		case "agent_response":
			if st.TextDelta != "" {
				if p.streamed.Len() > 0 && (p.afterTool || (p.lastTextStep != nil && st.StepIndex != nil && *st.StepIndex != *p.lastTextStep)) {
					sep := string([]byte{10, 10})
					p.streamed.WriteString(sep)
					p.sink.Emit(domain.EventText, sep)
				}
				p.afterTool = false
				p.lastTextStep = st.StepIndex
				p.streamed.WriteString(st.TextDelta)
				p.sink.Emit(domain.EventText, st.TextDelta)
			}
		case "tool":
			name := agyDisplayToolName(st)
			key := -1
			if st.StepIndex != nil {
				key = *st.StepIndex
			} else {
				key = len(p.announced)
			}
			if !p.announced[key] {
				p.announced[key] = true
				p.afterTool = true
				p.sink.Emit(domain.EventTool, name)
			}
			p.hostTool(st, name)
		}
	case "result":
		if ev.Result != nil {
			p.result = ev.Result
			if ev.Result.NumTurns > 0 {
				p.activity = true
			}
			if ev.Result.ConversationID != "" && p.requested == "" {
				p.conversation = ev.Result.ConversationID
			}
		}
	}
}

var agyMissingConversation = regexp.MustCompile(`(?i)conversation "([^"]+)" not found`)

func (p *agyParser) Stderr(text string) {
	if m := agyMissingConversation.FindStringSubmatch(text); m != nil && p.requested != "" {
		p.doBail(fmt.Sprintf("conversation %q not found", m[1]), "session_not_found")
	}
}

func (p *agyParser) Finish(code int, signal string, stderr string) (domain.Output, *Failure) {
	streamed := p.streamed.String()
	if p.result != nil {
		if p.result.Status == "SUCCESS" {
			response := p.result.Response
			text := streamed
			if strings.TrimSpace(streamed) == "" || (strings.HasPrefix(response, streamed) && len(response) > len(streamed)) {
				if len(response) > len(streamed) {
					p.sink.Emit(domain.EventText, response[len(streamed):])
				}
				text = response
			} else if !strings.Contains(squash(streamed), squash(response)) {
				text = response
			}
			return domain.Output{
				Text: strings.TrimSpace(text), Thinking: "",
				SessionID: firstString(p.conversation, p.result.ConversationID), Usage: agyUsage(p.result),
			}, nil
		}
		return domain.Output{}, &Failure{Message: strings.TrimSpace(fmt.Sprintf("agy %s: %s %s", firstString(p.result.Status, "ERROR"), p.result.Error, tailText(stderr, 400)))}
	}
	msg := fmt.Sprintf("agy exit %d", code)
	if signal != "" {
		msg += " signal=" + signal
	}
	return domain.Output{}, &Failure{Message: msg + ": " + tailText(stderr, 800)}
}
