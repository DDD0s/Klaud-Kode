// Package backend defines the adapter boundary between the API service and
// one official CLI. Backends render a turn into the CLI's native argv and
// stdin, and parse its native output back into domain events.
package backend

import (
	"encoding/json"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/domain"
)

// SessionMode says how a turn relates to the CLI's own session store.
type SessionMode string

const (
	SessionNone   SessionMode = "none"
	SessionNew    SessionMode = "new"
	SessionResume SessionMode = "resume"
)

// SessionRef identifies the CLI session for one turn.
type SessionRef struct {
	Mode SessionMode
	ID   string
}

// EffortInput carries the effort hints a client request may express.
type EffortInput struct {
	ReasoningEffort string
	Effort          string
	OutputEffort    string
	ThinkingEnabled bool
	ThinkingBudget  int
}

// Value returns the first explicitly requested effort string.
func (e EffortInput) Value() string {
	for _, v := range []string{e.ReasoningEffort, e.Effort, e.OutputEffort} {
		if v != "" {
			return v
		}
	}
	return ""
}

// Turn is one backend-ready model turn.
type Turn struct {
	Session       SessionRef
	Prompt        string
	Attachments   []json.RawMessage
	SystemPrompt  string
	IncludeSystem bool
	Model         string
	Effort        string
	Tools         string
	UseMCP        bool
	MCPConfig     string
	PrivateMCP    bool
	WorkDir       string
	MCPServer     *MCPServerSpec
}

// MCPServerSpec is one MCP server the CLI should reach.
type MCPServerSpec struct {
	Name    string
	URL     string
	Headers map[string]string
	Command string
	Args    []string
	Env     map[string]string
}

// Prepared is a rendered CLI invocation.
type Prepared struct {
	Args     []string
	Stdin    string
	Cwd      string
	ExtraEnv map[string]string
	Cleanup  func()
}

// Failure is a backend run failure before HTTP classification.
type Failure struct {
	Message  string
	Type     string
	Activity bool
}

func (f *Failure) Error() string { return f.Message }

// Parser consumes one CLI run's output.
type Parser interface {
	Line(line string)
	Stderr(stderr string)
	Finish(code int, signal string, stderr string) (domain.Output, *Failure)
	Activity() bool
}

// Backend is one official CLI adapter.
type Backend interface {
	Name() string
	Bin() string
	Cwd() string
	IDs() []string
	DefaultID() string
	TaskModel() string
	AssignsSessionID() bool
	InlinesSystem() bool
	NewSessionID() string
	ResolveModel(requested string) string
	ResolveEffort(input EffortInput) string
	ChildEnv(source map[string]string, cwd string) map[string]string
	Prepare(turn Turn) (Prepared, error)
	NewParser(session SessionRef, sink domain.Sink, bail func(*Failure)) Parser
	Owns(id string) bool
	Classify(message string) string
}
