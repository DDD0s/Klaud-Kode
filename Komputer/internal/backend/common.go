package backend

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

// InlineSystem puts instructions at the top of a user turn for CLIs without
// a system-prompt flag.
func InlineSystem(system, prompt string) string {
	if system == "" {
		return prompt
	}
	nl := string([]byte{10})
	return "<system_instructions>" + nl + system + nl + "</system_instructions>" + nl + nl + prompt
}

// PromptWithText appends textual attachment blocks to a prompt.
func PromptWithText(prompt string, texts []string) string {
	if len(texts) == 0 {
		return prompt
	}
	return strings.Join(append([]string{prompt}, texts...), string([]byte{10, 10}))
}

// AttachmentBlock is one decoded attachment content block.
type AttachmentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// SplitAttachments separates textual attachment blocks from binary ones.
func SplitAttachments(attachments []json.RawMessage) (texts []string, binary []AttachmentBlock) {
	for _, raw := range attachments {
		var block AttachmentBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			continue
		}
		if block.Type == "text" {
			texts = append(texts, block.Text)
		} else {
			binary = append(binary, block)
		}
	}
	return texts, binary
}

var mcpServerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ReadMCPServers reads the Claude-shaped body MCP config so other CLIs get
// the same servers translated into their own format.
func ReadMCPServers(file string) ([]MCPServerSpec, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, &domain.APIError{
			Status: 503, Type: "body_unavailable",
			Message: "The computer is unavailable. Please retry later.",
			Detail:  "MCP config unreadable: " + err.Error(),
		}
	}
	var parsed struct {
		Servers map[string]struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, &domain.APIError{
			Status: 503, Type: "body_unavailable",
			Message: "The computer is unavailable. Please retry later.",
			Detail:  "MCP config unreadable: " + err.Error(),
		}
	}
	var out []MCPServerSpec
	for name, server := range parsed.Servers {
		if !mcpServerNamePattern.MatchString(name) {
			continue
		}
		if server.URL != "" {
			headers := server.Headers
			if headers == nil {
				headers = map[string]string{}
			}
			out = append(out, MCPServerSpec{Name: name, URL: server.URL, Headers: headers})
		} else if server.Command != "" {
			out = append(out, MCPServerSpec{Name: name, Command: server.Command, Args: server.Args, Env: server.Env})
		}
	}
	return out, nil
}

// SplitArgs splits a whitespace-separated extra-args setting.
func SplitArgs(s string) []string { return strings.Fields(s) }
