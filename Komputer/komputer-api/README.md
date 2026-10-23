# Komputer API

API 接收本地客户端的消息，在远端启动官方 CLI，并把回复和工具调用交回客户端。
默认只用 Claude Code；也可以按模型名路由到 Antigravity（`agy`）或 Muse Code（`muse`），
见 [CLI 后端](../docs/backends.md)。服务不模拟客户端协议，每个回合都启动真实的 CLI。
首次搭建请看 [安装步骤](../SETUP.md)，这里记录接口和配置细节。

## 两种工具接法

请求带非空 `tools` 或显式 `tool_choice` 时，进入 harness 模式：客户端提供工具定义，
Claude 发起调用，本地客户端执行后回传结果。等待结果期间保留同一个 Claude 进程。
这一模式不读取 Body MCP 配置，也不使用远端内置工具。

不带客户端工具设置的请求使用 Body 模式：Claude 通过预先配置的 MCP 服务操作本地电脑。
Body 的配置和连接需要提前准备好。

两种工具不会在同一请求里混用。工具格式和限制见 [客户端工具协议](../docs/harness-api.md)。
接口支持的是 Chat Completions、Messages 和 Responses 的常用部分，不是所有模型 API 参数的完整实现。

## 地址和鉴权

默认监听 `http://127.0.0.1:18888`。这是远端主机自身的地址，不能直接填到另一台电脑上。
公网连接请先配置 [HTTPS](../docs/public-api.md)。

请求用下面任一请求头提供 `KEYS.txt` 中的 key：

```http
Authorization: Bearer YOUR_KOMPUTER_API_KEY
```

```http
x-api-key: YOUR_KOMPUTER_API_KEY
```

所有 key 都访问同一套会话和工具，没有按 key 隔离用户。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| GET | `/healthz` | 不带 key 只返回基础存活信息；带 key 可查看会话和工具运行状态 |
| GET | `/v1/models` | 获取本服务配置的模型名称 |
| POST | `/v1/chat/completions` | Chat Completions；支持 `stream: true` |
| POST | `/v1/messages` | Messages；支持 `stream: true` |
| POST | `/v1/messages/count_tokens` | 按字符数估算，不是模型的精确 tokenizer |
| POST | `/v1/responses` | Responses；支持 `stream: true`、函数工具和 `previous_response_id` |
| GET | `/v1/responses/:id` | 读取已保存的 Responses 回复 |
| DELETE | `/v1/responses/:id` | 删除已保存的回复，不删除 CLI 会话 |
| POST | `/v1/sessions` | 预建 Body 模式的会话映射，可传 `conversation_id` 和 `label` |
| GET | `/v1/sessions` | 查看 Body 模式的会话映射 |
| GET | `/v1/sessions/:id` | 查看指定映射；`:id` 可用客户端对话 ID |
| DELETE | `/v1/sessions/:id` | 删除映射，不删除 Claude 的磁盘转录 |
| DELETE | `/v1/tool-runs/:id` | 取消 `X-Komputer-Run` 对应的 harness 回合 |

除匿名健康检查外，都需要 key。把路径参数放进 URL 时记得编码。

## 模型和推理强度

`komputer-claude` 使用 Claude Code 的默认模型，也可通过 `KOMPUTER_CLAUDE_MODEL` 指定。
`komputer-opus`、`komputer-sonnet` 等名称会去掉 `komputer-` 前缀，传给 CLI 的 `--model`。
`claude-` 开头的名称，以及代码识别的别名，会直接传给 CLI；其他未知名称回退到默认模型。

`/v1/models` 只是本服务的配置列表，不是账号可用模型的实时查询。一个名称能出现在列表里，
不代表当前 Claude Code 和账号一定能使用它。

推理强度读取 `reasoning_effort`、`effort` 或 `output_config.effort`，支持
`low`、`medium`、`high`、`xhigh`、`max`；`minimal` 和 `none` 按 `low` 处理。
未显式指定时，`thinking.budget_tokens` 也会换算成 effort，并不是精确传递 token 预算。
最终是否支持对应强度，取决于 CLI 和所选模型。

开启 Antigravity 或 Muse 后，`/v1/models` 会加上它们的模型名，请求按 `model` 选择后端。
各后端的模型名、强度和隔离方式见 [CLI 后端](../docs/backends.md)。

## Responses API

`POST /v1/responses` 把请求转换为 Chat Completions 后走同一条路径，因此后端路由、Body 模式和
harness 模式都与其他接口一致。`function` 类型的 `tools` 进入 harness 模式，
`function_call_output` 输入项把工具结果交回正在等待的回合。

- `instructions` 作为系统提示，`reasoning.effort` 作为推理强度。
- `previous_response_id` 从本服务保存的回复中取回历史。同一条链的第二个回合建立 CLI 会话，
  之后的回合直接续接，不再重放历史。也可以传 `conversation` 或前面列出的对话 ID 请求头。
- `stream: true` 按 Responses 的事件顺序输出 `response.created`、`response.output_text.delta`、
  `response.function_call_arguments.delta`、`response.completed` 等事件，带 `sequence_number`，没有 `[DONE]`。
  输出开始前的错误返回 HTTP 状态码，之后的错误通过 `response.failed` 报告。
- 回复默认保存在 `responses.json`（权限 0600），只存文字，图片和文件变成占位符。
  `store: false` 的回复不保存，也就不能作为 `previous_response_id`。
- 不支持 `background`、非文字的 `text.format`、内置工具（如 `web_search`）以及未知的输入项类型，
  这些请求返回 400。

## 对话怎么续接

### harness 模式

客户端提交完整历史。Claude 的工具调用通过 API 返回，客户端执行后提交匹配 ID 的结果，
继续当前进程。新用户回合仍需要客户端提供历史。

工具等待和重试状态只存在内存里，不能靠 `/v1/sessions` 在重启后恢复。
断网后先检查本地操作结果，不要直接重新执行有副作用的命令。
细节见 [状态、权限与恢复](../docs/harness-api.md#状态权限与恢复)。

### Body 模式

客户端为每个聊天提供一个稳定的 ID，API 把它映射到 Claude Code 的会话。
第一次创建会话，后续使用 `--resume`；客户端不需要保存 Claude 自己的 UUID。
映射默认保存在 `sessions.json`。会话空闲超时只影响活动计数，不删除映射和转录。

最直接的办法是传请求头：

```http
X-Conversation-Id: my-chat-001
```

查找顺序是请求头、请求体、最后 `user` 字段。具体接受：

- 请求头依次为 `X-Conversation-Id`、`X-Chat-Id`、`X-OpenWebUI-Chat-Id`、
  `X-LibreChat-Conversation-Id`、`X-Open-WebUI-Chat-Id`、`X-Komputer-Session`、
  `X-Komputer-Session-Id`、`X-Session-Id`。
- 请求体依次为 `conversation_id`、`chat_id`、`thread_id`、`metadata.chat_id`、
  `metadata.conversation_id`、`metadata.thread_id`、`session_id`、`komputer_session`、`komputer_conversation_id`。
- 最后读取 `user`。不要把所有聊天共用的用户名填进这里，否则它们会接到同一个 Claude 会话。

响应头 `X-Conversation-Id`、`X-Chat-Id`、`X-Komputer-Session` 回显客户端 ID。
`X-Komputer-Claude-Session` 是内部 Claude ID，只用于调试。
JSON 中的 `conversation_id`、`chat_id`、`komputer_conversation_id`、`komputer_session` 也是客户端 ID。

不提供对话 ID 时，默认每次用客户端传来的历史单独请求，不建立会话映射。
也可用 `X-Komputer-Ephemeral: 1` 或 `komputer_ephemeral: true` 强制单次请求。
同一对话的回合串行执行，不同对话可并行，默认最多同时运行 4 个回合。

服务会比较历史，识别重新生成或编辑消息，必要时创建新会话，旧转录仍留在磁盘。
如果 CLI 明确报告旧会话不存在，且还没有模型或工具活动，会尝试用客户端历史重建一次。
响应头 `X-Komputer-Session-Recovered` 标记 `client-history` 或 `empty`；没有传来的历史不能恢复。
已经发生工具活动的失败不会自动重跑。

### 客户端设置

先确认客户端能发正确的接口和工具格式，再配置对话 ID。聊天界面不是远端的安装依赖。
各客户端菜单和变量会随版本变化，这里不把未验证的模板写成通用安装步骤。

能设置动态请求头的客户端，请用每个聊天各自的 ID。
不能提供独立 ID 的客户端可以使用默认无状态模式，但每次必须携带所需历史。
不要写死一个 ID 给所有聊天使用，也不要把 `{{chat_id}}` 这样的占位符原样发送给 API。

## 附件和流式回复

支持 OpenAI 风格的 `image_url` 和 PDF `file`，以及 Anthropic 风格的 `image`、`document` 块。
文本文件会解码后附在提示中。只有最新用户消息的附件会实际发送，较早的附件在历史里使用文字占位。
客户端工具结果支持文本和 base64 图片，不自动下载工具结果中的图片 URL。

`stream: true` 返回 SSE。harness 模式通过 `tool_calls` 或 `tool_use` 返回工具请求。
OpenAI 风格的回复可带 `reasoning_content`；harness 的 Messages 回复不生成 thinking 签名或 thinking 块。

流开始之前的失败可以返回对应 HTTP 错误。流一旦发出 `200`，后续错误通过流内事件报告，
客户端不能只凭 HTTP 状态判断整次任务成功。
客户端断开不等于取消任务；Body 模式会暂存未送达的结果，harness 模式可按工具 ID 重试或显式取消。

## 权限和云端环境

当前 API 总是用 `--dangerously-skip-permissions` 启动 Claude，CLI 端没有交互式审批。
agy 和 muse 的权限做法不同，见 [CLI 后端](../docs/backends.md#工具和隔离)。
本地 harness 的工具仍经过它自己的权限检查；Body 没有逐次审批功能。两端 permission 设置不会自动同步。

默认通过 `--tools ""` 关闭云端内置工具，只加载指定 MCP；harness 模式只加载当前客户端工具。
Claude 的工作目录默认放在仓库外的 `~/.komputer/workspaces/<实例摘要>`，并筛选子进程环境变量。
这些措施减少操作错机器和混入云端信息的机会，不是操作系统沙箱。

Claude Code 自身的提示、用户配置、memory 或 skills 仍可能带入云端信息。
建议为这套服务使用独立的普通系统用户，避免混用日常开发环境；不要为此删除已有个人配置。
更完整的限制见 [工具重定向讨论](../docs/tool-redirection.md)。

## 配置

`start.sh` 读取同目录的 `env`，下载并运行与 `Komputer/VERSION` 对应的预编译 `kloud-kode` 二进制（校验 SHA-256）；直接执行二进制或 `node server.mjs` 都不会自动读取这个文件。
修改配置后需要重启。文件格式和示例见 [env.example](env.example)。
需要回退到旧的 JavaScript 实现时用 `KOMPUTER_IMPL=node ./start.sh`；两种实现读写同一份 `KEYS.txt`、`sessions.json`、`responses.json` 和 MCP 配置，可以互相接续。

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `KOMPUTER_API_HOST` / `KOMPUTER_API_PORT` | `127.0.0.1` / `18888` | 监听地址和端口；地址可逗号分隔，也可写 `host:port` |
| `KLOUD_KODE_VERSION` / `KLOUD_KODE_BIN` / `KLOUD_KODE_BUILD` | `Komputer/VERSION` / 无 / 无 | 固定 Release 版本、指定已有二进制，或以 `1` 从源码构建；正常运行不需要 Go |
| `KOMPUTER_IMPL` | `go` | 设为 `node` 时回退运行 `server.mjs` |
| `KOMPUTER_WORKSPACE_ROOT` | `~/.komputer/workspaces` | 各 CLI 工作目录的父目录；家目录受保护或想放数据盘时指定 |
| `KOMPUTER_BACKENDS` | `claude` | 开启的后端，逗号分隔：`claude`、`antigravity`、`muse` |
| `KOMPUTER_DEFAULT_BACKEND` | 列表第一个 | 模型名不匹配任何后端时使用 |
| `CLAUDE_BIN` / `AGY_BIN` / `MUSE_BIN` | `claude` / `agy` / `muse` | 各 CLI 的可执行文件 |
| `KOMPUTER_AGY_MODEL_ID` / `KOMPUTER_AGY_MODELS` | `komputer-agy` / `komputer-gemini-4-argon,komputer-gemini-3.8-flash` | Antigravity 的模型名 |
| `KOMPUTER_AGY_MODEL` / `KOMPUTER_AGY_EFFORT` | 空 | Antigravity 的默认模型和强度 |
| `KOMPUTER_AGY_TASK_MODEL` | `gemini-3.8-flash-low` | Antigravity 的后台任务模型 |
| `KOMPUTER_AGY_EFFORTS` | `low,medium,high` | Gemini 模型允许的强度，更高的值降档 |
| `KOMPUTER_AGY_PERMISSIONS` | `settings` | `settings` 使用 agy 自己的权限设置；`skip` 传 `--dangerously-skip-permissions` |
| `KOMPUTER_AGY_MCP_SETUP` | `auto` | `auto` 启动时用 `agy mcp add` 在 agy 全局配置中注册 MCP 转接器；`off` 只检查并提示 |
| `KOMPUTER_AGY_REMOTE_TOOLS` | `off` | `off` 与 Claude 相同：Body 回合里 agy 自己的工具被 hook 拒绝，模型经 `komputer_use` 操作用户电脑。实验功能，只对 POSIX Body 生效（Windows Body 仍按 `off`）：`auto` 命令经 bash 替身在用户电脑上运行，能用 FUSE 时文件工具也经挂载；`all` 要求挂载；`commands` 只转发命令。见 [CLI 后端](../docs/backends.md#与-claude-共用的设计) |
| `KOMPUTER_AGY_REMOTE_SHELL` | `bash` | 打开转发时远端执行命令的 shell，取值同 Body `run_command` 的 `shell` |
| `KOMPUTER_AGY_REMOTE_MOUNT` / `KOMPUTER_AGY_PYTHON` | `<agy 工作目录>/remote` / `python3` | 打开转发时 Body 根目录的 FUSE 挂载点，以及运行 `komputer-fs.py`（需要 fusepy）的解释器 |
| `KOMPUTER_AGY_BUILTIN_GUARD` | `warn` | agy 的工具步骤在执行后仍被策略判为“在服务主机上”时：`warn` 记日志，`stop` 结束回合并返回 502，`off` 不检查 |
| `KOMPUTER_AGY_CWD` / `KOMPUTER_MUSE_CWD` | `~/.komputer/workspaces/<实例摘要>-agy` / `-muse` | agy、muse 的工作目录 |
| `KOMPUTER_MUSE_MODEL_ID` / `KOMPUTER_MUSE_MODELS` | `komputer-muse` / 空 | Muse 的模型名 |
| `KOMPUTER_MUSE_MODEL` / `KOMPUTER_MUSE_EFFORT` / `KOMPUTER_MUSE_TASK_MODEL` | 空 | Muse 的默认模型、强度和后台任务模型 |
| `KOMPUTER_AGY_EXTRA_ARGS` / `KOMPUTER_MUSE_EXTRA_ARGS` | 空 | 追加到 CLI 命令行的参数，按空白拆分，不处理引号 |
| `KOMPUTER_RESPONSES_FILE` | API 目录下的 `responses.json` | Responses 回复存储；设为空字符串只存在内存 |
| `KOMPUTER_MAX_STORED_RESPONSES` | `1000` | 最多保存的 Responses 回复数，超出删除最旧的 |
| `KOMPUTER_MCP_CONFIG` | `komputer-session/.mcp.json` | Body 模式 MCP 文件；真实配置应放仓库外 |
| `KOMPUTER_CLAUDE_CWD` | `~/.komputer/workspaces/<实例摘要>` | Claude 工作目录 |
| `KOMPUTER_KEYS_FILE` / `KOMPUTER_SESSIONS_FILE` | API 目录下的 `KEYS.txt` / `sessions.json` | 密钥和 Body 会话映射文件 |
| `KOMPUTER_MAX_IN_FLIGHT` | `4` | 同时运行的 Claude 回合数，多出的等待 |
| `KOMPUTER_MAX_SESSIONS` | `0` | Body 活动会话上限，0 表示不限 |
| `KOMPUTER_IDLE_TIMEOUT_MS` | `2700000` | Body 会话空闲 45 分钟后不再计为活动会话 |
| `KOMPUTER_CLAUDE_TIMEOUT_MS` | `600000` | 单回合超时，10 分钟；对所有后端生效 |
| `KOMPUTER_CLIENT_TOOL_TIMEOUT_MS` | `300000` | 等待客户端工具结果，5 分钟 |
| `KOMPUTER_MAX_CLIENT_TOOL_RUNS` | `16` | harness 活跃及缓存回合上限 |
| `KOMPUTER_CLAUDE_MODEL` / `KOMPUTER_EFFORT` | 空 | 默认模型和推理强度 |
| `KOMPUTER_MODELS` | `komputer-opus,komputer-sonnet,komputer-haiku,komputer-fable` | `/v1/models` 附加名称 |
| `KOMPUTER_NO_KEY_MODE` | `stateless` | 未提供对话 ID 时的行为，也可用 `auto-session`；与 API 鉴权无关 |
| `KOMPUTER_DETECT_REGENERATE` | `1` | 检测 Body 对话重新生成和编辑 |
| `KOMPUTER_TASK_DETECT` / `KOMPUTER_TASK_MODEL` | `1` / `haiku` | 把最新消息以 `### Task:` 开头的请求视为无工具的后台任务 |
| `KOMPUTER_MAX_BODY_BYTES` | `50000000` | HTTP 请求体上限 |
| `KOMPUTER_MAX_HISTORY_CHARS` | `60000` | 传入的历史字符数上限 |
| `KOMPUTER_TURN_CACHE_MS` | `600000` | 回合重试缓存时间，10 分钟 |
| `KOMPUTER_HEARTBEAT_MS` | `15000` | SSE 保活间隔 |
| `KOMPUTER_SSE_OPEN_MS` | `25000` | 最长等待 25 秒后打开流，后续错误在流内报告 |
| `KOMPUTER_STREAM_THINKING` / `KOMPUTER_STREAM_TOOL_NOTES` | `1` / `1` | 输出可用的思考内容和工具进度；不保证各协议、各客户端都显示 |
| `KOMPUTER_AUTH_FAIL_DELAY_MS` | `1000` | 鉴权失败后的回复延迟 |
| `KOMPUTER_BUILTIN_TOOLS` | 空 | Body 模式的云端内置工具；设为 `default` 会重新允许操作云端 |
| `KOMPUTER_SYSTEM_PROMPT_FILE` | 空 | 实验性完整提示替换，需要自己验证工具行为 |
| `KOMPUTER_CHILD_ENV_EXTRA` | 空 | 额外传入 Claude 的环境变量名，逗号分隔；不接受保留的服务配置变量 |
| `KOMPUTER_SKIP_TUNNEL` | 未设置 | `1` 跳过 Body 健康探测，不会单独阻止启动脚本尝试隧道 |
| `KOMPUTER_TUNNEL_UP` | `komputer/tunnel-up.sh` | 建立 Body 隧道的脚本；纯 harness 安装可设 `/bin/true` |
| `KOMPUTER_HEALTH_URL` | `http://127.0.0.1:18787/healthz` | Body 探测地址 |
| `KOMPUTER_TUNNEL_FAIL_CACHE_MS` | `30000` | Body 探测失败后等多久再试；0 关闭退避 |

TLS、CORS 和公网限制见 [公网配置](../docs/public-api.md)。
出网代理沿用运行环境中的 `HTTP_PROXY`、`HTTPS_PROXY` 等变量，支持大小写名称。
转交 `ALL_PROXY` 不代表每个下游组件都支持它；本机 MCP 和隧道地址应绕过代理。
CLI 登录目录、证书等必要环境会保留，完整名单见 [claude-env.mjs](claude-env.mjs)。

更新 `KEYS.txt` 后，可以在 Linux 主机上重新加载 key：

```bash
kill -HUP "$(cat komputer-api.pid)"
```

## 日志和更新

日志在 `logs/komputer-api.log`。客户端只得到错误类型和简要信息，诊断详情在服务端。
带 key 的 `/healthz` 中，`body_ok` 是缓存结果：`true` 表示最近 15 秒探测成功，
`false` 表示仍在失败退避期，`null` 表示没有有效结果或已跳过检查。
健康检查本身不会建立 SSH 隧道，也不代表工具实时可用。

已经搭建好的环境可以使用 `deploy/` 脚本更新。它会测试、等待回合结束、更新远端并检查健康状态，
失败时尝试回滚。它不是首次安装器；运行前阅读脚本和 [开发说明](../DEVELOPING.md)，
核对部署目标、路径和需要保留的配置。
