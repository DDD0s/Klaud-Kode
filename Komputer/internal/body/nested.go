package body

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"sync"
	"sync/atomic"
)

// NestedTool is one tool from a stdio MCP server installed on this machine.
type NestedTool struct {
	Server     string
	Name       string
	Definition json.RawMessage
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

type nestedClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64
	pending sync.Map // int64 -> chan rpcReply
	done    chan struct{}
}

func (c *nestedClient) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.stdin.Write(append(data, byte(10)))
	return err
}

func (c *nestedClient) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcReply, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("nested MCP server disconnected")
	}
}

func (c *nestedClient) readLoop(stdout io.Reader, name string, logf func(string, ...any)) {
	reader := bufio.NewReader(stdout)
	defer close(c.done)
	for {
		line, err := reader.ReadBytes(10)
		if len(line) > 0 {
			var msg struct {
				ID     *int64          `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(line, &msg); err == nil && msg.ID != nil {
				if ch, ok := c.pending.Load(*msg.ID); ok {
					reply := rpcReply{result: msg.Result}
					if msg.Error != nil {
						reply.err = fmt.Errorf("%s", msg.Error.Message)
					}
					ch.(chan rpcReply) <- reply
				}
			} else if err != nil && logf != nil {
				logf("nested mcp %s: unreadable line: %v", name, err)
			}
		}
		if err != nil {
			return
		}
	}
}

func startNestedClient(ctx context.Context, name string, spec NestedServer, logf func(string, ...any)) (*nestedClient, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	if spec.Cwd != "" {
		cmd.Dir = expandHome(spec.Cwd)
	}
	env := CommandEnv()
	for k, v := range spec.Env {
		env[k] = v
	}
	cmd.Env = envList(env)
	cmd.Stderr = os.Stderr
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	client := &nestedClient{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go client.readLoop(stdout, name, logf)
	_, err = client.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "komputer", "version": Version},
	})
	if err != nil {
		client.Close()
		return nil, err
	}
	if err := client.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func (c *nestedClient) listTools(ctx context.Context) ([]json.RawMessage, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var result struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	return result.Tools, nil
}

func (c *nestedClient) callTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
}

// Close ends the nested server process.
func (c *nestedClient) Close() {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	killTree(c.cmd)
	_ = c.cmd.Wait()
}

// Nested owns every forwarded stdio MCP server on this machine.
type Nested struct {
	cfg     Config
	logf    func(string, ...any)
	mu      sync.Mutex
	clients map[string]*nestedClient
	tools   []NestedTool
}

// NewNested creates an empty nested-server registry.
func NewNested(cfg Config, logf func(string, ...any)) *Nested {
	return &Nested{cfg: cfg, logf: logf, clients: map[string]*nestedClient{}}
}

// Connect starts every enabled configured server and indexes its tools.
// A broken nested server is logged and skipped, never fatal.
func (n *Nested) Connect(ctx context.Context) {
	names := make([]string, 0, len(n.cfg.MCPServers))
	for name := range n.cfg.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := n.cfg.MCPServers[name]
		if spec.Enabled != nil && !*spec.Enabled {
			continue
		}
		if spec.Command == "" {
			n.logf("nested mcp %s: no command configured", name)
			continue
		}
		client, err := startNestedClient(ctx, name, spec, n.logf)
		if err != nil {
			n.logf("nested mcp %s: %v", name, err)
			continue
		}
		defs, err := client.listTools(ctx)
		if err != nil {
			n.logf("nested mcp %s: %v", name, err)
			client.Close()
			continue
		}
		n.mu.Lock()
		n.clients[name] = client
		for _, def := range defs {
			var head struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(def, &head)
			if head.Name != "" {
				n.tools = append(n.tools, NestedTool{Server: name, Name: head.Name, Definition: def})
			}
		}
		count := len(defs)
		n.mu.Unlock()
		n.logf("nested mcp %s: %d tool(s)", name, count)
	}
}

// Tools returns every forwarded tool definition.
func (n *Nested) Tools() []NestedTool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]NestedTool{}, n.tools...)
}

// Call forwards one tool call to a nested server, returning its raw result.
func (n *Nested) Call(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error) {
	n.mu.Lock()
	client := n.clients[server]
	n.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("unknown MCP server: %s", server)
	}
	return client.callTool(ctx, tool, args)
}

// Close stops every nested server.
func (n *Nested) Close() {
	n.mu.Lock()
	clients := make([]*nestedClient, 0, len(n.clients))
	for _, c := range n.clients {
		clients = append(clients, c)
	}
	n.clients = map[string]*nestedClient{}
	n.mu.Unlock()
	for _, c := range clients {
		c.Close()
	}
}
