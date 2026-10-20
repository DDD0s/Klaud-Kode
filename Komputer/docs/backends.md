# CLI 后端

API 不模拟任何客户端协议，也不伪造请求头。每个回合都启动对应的官方 CLI，
上游看到的就是这个 CLI 自己的流量。服务只负责拼参数、写 stdin、读它的 JSON 输出。

目前有三个后端：

| 后端 | CLI | `KOMPUTER_BACKENDS` 名称 | 默认模型名 |
| --- | --- | --- | --- |
| Claude Code | `claude` | `claude`、`claude-code` | `komputer-claude` 及 `KOMPUTER_MODELS` |
| Antigravity | `agy` | `antigravity`、`agy` | `komputer-agy`、`komputer-gemini-4-argon`、`komputer-gemini-3.8-flash` |
| Muse Code | `muse` | `muse`、`muse-code` | `komputer-muse` 及 `KOMPUTER_MUSE_MODELS` |

## 怎么选后端

`KOMPUTER_BACKENDS` 默认只有 `claude`，这时服务和加后端之前完全一样：
Claude 的参数、stdin 和子进程环境由 `test/claude-regression.test.mjs` 逐字节固定。

一个进程可以同时开多个后端，例如 `KOMPUTER_BACKENDS=claude,antigravity,muse`。
请求的 `model` 按下面的顺序选后端：

1. 与某个后端公布的模型名完全相同。
2. 名称前缀：`agy-`、`antigravity-`、`gemini-`（可带 `komputer-`）归 Antigravity，`muse-` 归 Muse。
3. Claude 认识的名称和别名。
4. 都不匹配时用 `KOMPUTER_DEFAULT_BACKEND`，默认是列表里第一个。

`/v1/models` 列出所有已开启后端的模型名，两个后端不能公布同一个名称。
响应头 `X-Komputer-Backend` 显示实际使用的后端，`X-Komputer-Backend-Session` 是该 CLI 的会话 ID，只用于调试。

也可以每个后端跑一个实例：给每个实例单独的 `env`，设不同的 `KOMPUTER_BACKENDS` 和端口。
无论哪种方式，默认都只监听 `127.0.0.1`。

### 会话和续接

Body 模式下，客户端对话 ID 映射到当前后端的 CLI 会话：

- Claude：服务生成 UUID，第一次 `--session-id`，之后 `--resume`，与以前相同。
- Antigravity：会话 ID 由 agy 分配，服务从 `init` 事件读到后保存，之后用 `--conversation <id>`。
- Muse：服务生成 UUID，用 `muse exec --session-id <id>`，这个参数会新建或续接会话。

同一对话中途换到另一个后端时，不会把会话 ID 交给别的 CLI，而是用客户端历史在新后端重建。
CLI 报告会话不存在时，和 Claude 一样只在没有任何模型或工具活动时用历史重试一次。

agy 和 muse 没有单独的系统提示参数，系统提示放在 `<system_instructions>` 中随第一次提示发送；
续接时只有提示内容变了才重新发送。

### 推理强度

三个后端都读取 `reasoning_effort`、`effort`、`output_config.effort`、Responses 的
`reasoning.effort`，以及换算后的 `thinking.budget_tokens`。

- Antigravity 默认只接受 `low`、`medium`、`high`（`KOMPUTER_AGY_EFFORTS`），更高的值降到列表里最高一档，
  `minimal`/`none` 按 `low` 处理。模型名已带 `-low` 这类后缀时不再传 `--effort`。
- Muse 原样传给 `--reasoning-effort`，只接受它列出的取值。

## 工具和隔离

Claude 用 `--tools ""` 关闭内置工具，这一点没有变化。另外两个 CLI 的做法如下。

**Antigravity。** agy 没有关闭内置工具的参数，靠它自己的权限设置限制工具。
headless 运行时，没有被允许的工具调用会被拒绝，不会弹出确认。
因此要在远端账号的 `~/.gemini/antigravity-cli/settings.json` 中允许本服务的 MCP：

```json
{ "permissions": { "allow": ["mcp(komputer_use/*)"] } }
```

服务启动时检查这条规则，缺少时在日志里提醒。`KOMPUTER_AGY_PERMISSIONS=skip` 会传
`--dangerously-skip-permissions`，这时 agy 的 shell 和文件工具也会在远端直接运行，只在你确实需要时使用。

Body 模式的 MCP 配置从 `KOMPUTER_MCP_CONFIG` 转换为工作目录下的 `.agents/mcp_config.json`（权限 0600）；
harness 模式每个回合使用独立目录，只写当前回合的客户端工具 MCP。
agy 不接受图片和 PDF 附件，带附件的请求返回 400。

**Muse。** Body 和 harness 模式都通过 `-c mcp_servers.<名称>=...` 传入 MCP，并设为 `required`；
MCP 起不来时回合失败，不会在没有工具的情况下继续。URL 和请求头通过 `${变量}` 从子进程环境读取，
不会出现在命令行里。island 模式下另加 `--disable-shell --disable-write --disable-web-tools`。
总会加 `--disable-approval --disable-request-user-input --no-foreign-personal-context`。
图片附件写成 0600 临时文件，通过 `--image` 传入，回合结束后删除。

两个 CLI 都在仓库外的独立工作目录中运行（`~/.komputer/workspaces/<实例摘要>-agy`、`-muse`），
子进程环境同样经过筛选，名单见 [claude-env.mjs](../komputer-api/claude-env.mjs)。

## 哪些验证过，哪些是假设

替身 CLI（`test/fake-agy.mjs`、`test/fake-muse.mjs`）复现了下表“已验证”一栏的输出格式。
`test/fixtures/` 里是真实 CLI 的输出记录，解析器单元测试直接读取它们。
设置 `KOMPUTER_TEST_REAL_AGY` / `KOMPUTER_TEST_REAL_MUSE`（可选 `_HOME`）可以对本机真实 CLI 跑冒烟测试，默认跳过。

### Antigravity（agy 1.4.3）

| 已对真实 CLI 验证 | 假设，可通过配置调整 |
| --- | --- |
| `--input-format stream-json --output-format stream-json`、`--model`、`--effort`、`--conversation`、`--dangerously-skip-permissions` 参数存在 | 账号登录后 `--model` 接受的模型名（未登录时 agy 不校验模型）；可用 `KOMPUTER_AGY_MODEL`、`KOMPUTER_AGY_MODELS` 调整 |
| stdin 用户消息的格式、`init` / `step_update` / `result` 事件结构 | 多个工具步骤之间 `text_delta` 和最终 `response` 的关系；流式回复用增量，非流式回复以 `result.response` 为准，缺失时用增量拼接 |
| 未登录时的 `result` 行、stderr 中的 `authentication required` 和退出码 1，映射为鉴权错误 | 工作目录下的 `.agents/mcp_config.json` 会被加载（格式与 `agy mcp add` 写出的一致） |
| `--conversation` 指向不存在的会话时，agy 打印警告后新开会话；服务把它视为会话丢失 | `permissions.allow` / `deny` 与 skip 参数同时存在时的优先级 |
| | 模型名与 `--effort` 的组合是否都被接受；可用 `KOMPUTER_AGY_EFFORTS` 收窄 |
| | 用量字段的含义（`thinking` 记为 reasoning tokens） |

### Muse Code（muse 1.4.11，echo provider）

| 已对真实 CLI 验证 | 假设，可通过配置调整 |
| --- | --- |
| `exec --json` 的 JSONL 结构，`run.output.delta` 文本、`run.usage`、`run.terminal.completed` / `failed` | 真实模型调用工具时的 task 名称；解析器依次读取 `tool_name`、`name`、`task_kind` |
| `--session-id` 新建时第一条会话记录序号为 1，续接时为 2；续接却得到 1 视为会话丢失 | `--disable-approval` 下 MCP 工具不再请求确认 |
| 换工作目录续接时的拒绝信息 | 用量字段在真实模型下的含义 |
| 没有凭据时只在 stderr 报错，映射为鉴权错误 | 两个 CLI 都不输出 thinking 文本，所以 `reasoning_content` 为空 |
| `required` MCP 起不来时返回 `run.terminal.failed`，映射为 `body_unavailable` | |
| `-c mcp_servers` 中 url 和 headers 的 `${变量}` 替换（本地 MCP 服务确实收到了替换后的 Bearer） | |

真实账号下的完整回合（登录后的模型调用、真实工具调用、续接）还没有跑过。
上线前按 [安装步骤](../SETUP.md) 的方法，先让模型运行 `hostname` 并核对工具记录。
