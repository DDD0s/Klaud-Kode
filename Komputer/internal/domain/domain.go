// Package domain holds the protocol-neutral types shared by the API
// adapters, the CLI backends and the caller-side execution paths.
package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Message is one conversation message in the shared internal shape. Content
// stays raw so OpenAI and Anthropic content parts keep their fidelity until
// a backend needs text or attachment blocks.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
}

// Usage is the token accounting every backend reports in the same fields.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
}

// Output is one finished backend turn.
type Output struct {
	Text      string
	Thinking  string
	SessionID string
	Usage     *Usage
}

// EventKind identifies one streamed fragment of a turn.
type EventKind string

const (
	EventText     EventKind = "text"
	EventThinking EventKind = "thinking"
	EventTool     EventKind = "tool"
)

// Event is one streamed fragment in protocol-neutral form.
type Event struct {
	Kind  EventKind
	Value string
}

// Sink receives streamed fragments. A nil Sink is valid.
type Sink func(Event)

// Emit calls s when it is set.
func (s Sink) Emit(kind EventKind, value string) {
	if s != nil && value != "" {
		s(Event{Kind: kind, Value: value})
	}
}

// APIError is an error that maps to one public HTTP response.
type APIError struct {
	Status                int
	Type                  string
	Message               string
	Detail                string
	RetryAfter            int
	MissingBeforeActivity bool
}

func (e *APIError) Error() string { return e.Message }

// NewUUID returns a random RFC 4122 version 4 UUID.
func NewUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// IsUUID reports whether s is a UUID in canonical text form.
func IsUUID(s string) bool { return uuidPattern.MatchString(strings.TrimSpace(s)) }

// NormalizeKey cleans a client conversation key. It returns "" for values
// clients use to mean "no conversation".
func NormalizeKey(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	switch strings.ToLower(t) {
	case "undefined", "null", "none", "anonymous", "default":
		return ""
	}
	r := []rune(t)
	if len(r) > 512 {
		r = r[:512]
	}
	return string(r)
}

// SHA256Hex returns the first n hex characters of the SHA-256 of s.
func SHA256Hex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	h := hex.EncodeToString(sum[:])
	if n > 0 && n < len(h) {
		return h[:n]
	}
	return h
}

var (
	redactMCPPath = regexp.MustCompile(`(?i)mcp/[^\s/?#"'<>]+`)
	redactBearer  = regexp.MustCompile(`(?i)\bBearer\s+[^\s"',;]+`)
	redactURLAuth = regexp.MustCompile(`(?i)(https?://)[^\s/@]+:[^\s/@]+@`)
	redactSK      = regexp.MustCompile(`sk-[A-Za-z0-9_-]{10,}`)
)

// Redact removes credential-shaped fragments from text bound for logs.
func Redact(s string) string {
	s = redactMCPPath.ReplaceAllString(s, "mcp/<redacted>")
	s = redactBearer.ReplaceAllString(s, "Bearer <redacted>")
	s = redactURLAuth.ReplaceAllString(s, "$1<redacted>@")
	return redactSK.ReplaceAllString(s, "sk-<redacted>")
}

func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return m, true
}

// ContentToText flattens an OpenAI or Anthropic content value to text.
// Attachment parts become short placeholders.
func ContentToText(content json.RawMessage) string {
	if len(content) == 0 || string(content) == "null" {
		return ""
	}
	if s, ok := rawString(content); ok {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err == nil {
		var out []string
		for _, part := range parts {
			if s, ok := rawString(part); ok {
				if s != "" {
					out = append(out, s)
				}
				continue
			}
			m, ok := rawObject(part)
			if !ok {
				continue
			}
			if text, ok := rawString(m["text"]); ok && text != "" {
				out = append(out, text)
				continue
			}
			typ, _ := rawString(m["type"])
			switch typ {
			case "image_url", "image":
				out = append(out, "[image]")
			case "file", "document":
				name := ""
				if f, ok := rawObject(m["file"]); ok {
					name, _ = rawString(f["filename"])
				}
				if name == "" {
					name, _ = rawString(m["title"])
				}
				if name != "" {
					out = append(out, "[file: "+name+"]")
				} else {
					out = append(out, "[file]")
				}
			case "tool_result":
				if t := ContentToText(m["content"]); t != "" {
					out = append(out, t)
				}
			}
		}
		return strings.Join(out, "\x0a")
	}
	if m, ok := rawObject(content); ok {
		if s, ok := rawString(m["text"]); ok {
			return s
		}
	}
	return string(content)
}

var attachmentTypes = map[string]bool{"image_url": true, "image": true, "file": true, "document": true}

// ContentTextOnly is ContentToText without attachment placeholders.
func ContentTextOnly(content json.RawMessage) string {
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return ContentToText(content)
	}
	kept := parts[:0:0]
	for _, p := range parts {
		if m, ok := rawObject(p); ok {
			if typ, _ := rawString(m["type"]); attachmentTypes[typ] {
				continue
			}
		}
		kept = append(kept, p)
	}
	raw, _ := json.Marshal(kept)
	return ContentToText(raw)
}

var dataURLPattern = regexp.MustCompile(`(?s)^data:([^;,]+)?(;base64)?,(.*)$`)

type dataURL struct {
	mediaType string
	base64    bool
	data      string
}

func parseDataURL(s string) (dataURL, bool) {
	m := dataURLPattern.FindStringSubmatch(s)
	if m == nil {
		return dataURL{}, false
	}
	mt := m[1]
	if mt == "" {
		mt = "application/octet-stream"
	}
	return dataURL{mediaType: mt, base64: m[2] != "", data: m[3]}, true
}

func mustRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ExtractAttachments converts one message's attachments into Anthropic
// content blocks for CLI backends that accept them.
func ExtractAttachments(content json.RawMessage) []json.RawMessage {
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil
	}
	var blocks []json.RawMessage
	for _, part := range parts {
		m, ok := rawObject(part)
		if !ok {
			continue
		}
		typ, _ := rawString(m["type"])
		if (typ == "image" || typ == "document") && len(m["source"]) > 0 {
			block := map[string]json.RawMessage{"type": mustRaw(typ), "source": m["source"]}
			if len(m["title"]) > 0 {
				block["title"] = m["title"]
			}
			blocks = append(blocks, mustRaw(block))
			continue
		}
		if typ == "image_url" {
			u, _ := rawString(m["image_url"])
			if u == "" {
				if obj, ok := rawObject(m["image_url"]); ok {
					u, _ = rawString(obj["url"])
				}
			}
			if d, ok := parseDataURL(u); ok && d.base64 {
				blocks = append(blocks, mustRaw(map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "base64", "media_type": d.mediaType, "data": d.data},
				}))
			} else if strings.HasPrefix(strings.ToLower(u), "http://") || strings.HasPrefix(strings.ToLower(u), "https://") {
				blocks = append(blocks, mustRaw(map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "url", "url": u},
				}))
			}
			continue
		}
		if typ == "file" {
			f, ok := rawObject(m["file"])
			if !ok {
				continue
			}
			raw, _ := rawString(f["file_data"])
			filename, _ := rawString(f["filename"])
			d, isData := parseDataURL(raw)
			mediaType := "application/pdf"
			data := raw
			if isData {
				mediaType, data = d.mediaType, d.data
			}
			if data == "" {
				continue
			}
			switch {
			case mediaType == "application/pdf":
				block := map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": data}}
				if filename != "" {
					block["title"] = filename
				}
				blocks = append(blocks, mustRaw(block))
			case strings.HasPrefix(mediaType, "image/"):
				blocks = append(blocks, mustRaw(map[string]any{
					"type":   "image",
					"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
				}))
			default:
				var text string
				if isData && !d.base64 {
					text, _ = url.PathUnescape(data)
				} else {
					decoded, _ := base64.StdEncoding.DecodeString(data)
					text = string(decoded)
				}
				blocks = append(blocks, mustRaw(map[string]any{"type": "text", "text": "File " + filename + ":\x0a" + text}))
			}
		}
	}
	return blocks
}

func lastIndexOfRole(messages []Message, role string, before int) int {
	if before > len(messages) {
		before = len(messages)
	}
	for i := before - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, role) {
			return i
		}
	}
	return -1
}

// LatestUserMessage returns the last user message, or nil.
func LatestUserMessage(messages []Message) *Message {
	i := lastIndexOfRole(messages, "user", len(messages))
	if i < 0 {
		return nil
	}
	return &messages[i]
}

// LatestUserText returns the text of the last user message.
func LatestUserText(messages []Message) string {
	m := LatestUserMessage(messages)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(ContentTextOnly(m.Content))
}

// CollectSystemText joins the caller-supplied system and developer text.
func CollectSystemText(messages []Message, extraSystem string) string {
	var parts []string
	if extraSystem != "" {
		parts = append(parts, extraSystem)
	}
	for _, m := range messages {
		if strings.EqualFold(m.Role, "system") || strings.EqualFold(m.Role, "developer") {
			if t := strings.TrimSpace(ContentToText(m.Content)); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\x0a\x0a")
}

// PriorTranscript renders the client history before the latest user message.
func PriorTranscript(messages []Message, maxChars int) string {
	end := lastIndexOfRole(messages, "user", len(messages))
	if end <= 0 {
		return ""
	}
	var lines []string
	for _, m := range messages[:end] {
		role := strings.ToLower(m.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		if t := strings.TrimSpace(ContentToText(m.Content)); t != "" {
			label := "User"
			if role == "assistant" {
				label = "Assistant"
			}
			lines = append(lines, label+": "+t)
		}
	}
	text := strings.Join(lines, "\x0a\x0a")
	r := []rune(text)
	if maxChars > 0 && len(r) > maxChars {
		text = "[…earlier history truncated…]\x0a" + string(r[len(r)-maxChars:])
	}
	return text
}

// BuildTurnPrompt renders the backend prompt for one turn. With history it
// seeds the transcript the client sent before its latest user message.
func BuildTurnPrompt(messages []Message, withHistory bool, maxHistoryChars int) string {
	latest := LatestUserMessage(messages)
	hasAttachments := latest != nil && len(ExtractAttachments(latest.Content)) > 0
	user := LatestUserText(messages)
	if user == "" {
		if hasAttachments {
			user = "(see attachment)"
		} else {
			user = "(empty)"
		}
	}
	if !withHistory {
		return user
	}
	prior := PriorTranscript(messages, maxHistoryChars)
	if prior == "" {
		return user
	}
	return "Earlier conversation for context:" + "\x0a\x0a" + prior + "\x0a\x0a---\x0a\x0a" + user
}

// ReplyTail is a whitespace-insensitive fingerprint of a reply's end.
func ReplyTail(text string) string {
	var b strings.Builder
	for _, r := range text {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	r := []rune(b.String())
	if len(r) > 160 {
		r = r[len(r)-160:]
	}
	return string(r)
}

// HistoryDiverged reports whether the client's history no longer matches
// the stored fingerprint of the last reply in this session.
func HistoryDiverged(messages []Message, historySeen bool, lastReplyTail string) bool {
	lastUser := lastIndexOfRole(messages, "user", len(messages))
	if lastUser < 0 {
		return false
	}
	prevAssistant := lastIndexOfRole(messages, "assistant", lastUser)
	if prevAssistant < 0 {
		return historySeen
	}
	if lastReplyTail == "" {
		return false
	}
	return ReplyTail(ContentToText(messages[prevAssistant].Content)) != lastReplyTail
}

// HasAssistantMessage reports whether any message has the assistant role.
func HasAssistantMessage(messages []Message) bool {
	return lastIndexOfRole(messages, "assistant", len(messages)) >= 0
}
