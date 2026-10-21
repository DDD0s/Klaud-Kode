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

agy 也能使用非 Gemini 模型。为了不和 Claude 后端的名称冲突，用 `komputer-agy-<模型>` 的形式，
例如 `KOMPUTER_AGY_MODELS=komputer-gemini-4-argon,komputer-gemini-3.8-flash,komputer-agy-claude-opus-5-5`，
请求 `komputer-agy-claude-opus-5-5` 时传给 agy 的是 `--model claude-opus-5-5`。

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

- Antigravity 有两种写法：模型名带后缀（`gemini-3.8-flash-low`），或基础模型名加 `--effort`。
  两者同时出现会被 agy 拒绝，所以模型名已带后缀时不再传 `--effort`。
  Gemini 模型不接受 `xhigh`、`max`，默认降到 `KOMPUTER_AGY_EFFORTS`（`low,medium,high`）中最高一档；
  未指定模型时也按 Gemini 处理。其他经 agy 使用的模型（如 `claude-opus-5-5`）保留 CLI 的完整范围。
  `minimal`/`none` 按 `low` 处理。
- Muse 原样传给 `--reasoning-effort`，只接受它列出的取值。

## 工具和隔离

Claude 用 `--tools ""` 关闭内置工具，这一点没有变化。另外两个 CLI 的做法如下。

**Antigravity。** agy 只从全局的 `~/.gemini/config/mcp_config.json`（以及插件）读取 MCP，
没有工作目录级或单个进程级的配置；它启动 stdio MCP 服务时会沿用自己的环境变量和工作目录。
所以服务启动时用真实的 `agy mcp add` 为每个 MCP 名称（默认 `komputer_use`）注册一个 stdio 转接器
[agy-mcp-bridge.mjs](../komputer-api/backends/agy-mcp-bridge.mjs)，全局配置里只有 node 和脚本路径，没有地址或 token。
每个 agy 回合把这次的目标写进一个 0600 文件，通过环境变量 `KOMPUTER_AGY_MCP_FILE` 交给转接器：

- Body 模式：指向 `KOMPUTER_MCP_CONFIG` 里的服务（HTTP 或 stdio），文件放在 agy 工作区之外，回合结束后删除。
- harness 模式：只指向本回合的客户端工具 MCP，不会看到 Body。
- 后台任务，以及手动运行的 agy：没有目标，转接器是一个没有工具的 MCP 服务。

这会改动运行服务的系统用户的 agy 全局配置，只增改上述名称，不动其他条目；`agy mcp add` 也会重新启用被禁用的同名条目。
不希望服务改动时设 `KOMPUTER_AGY_MCP_SETUP=off`，启动日志会给出需要手动执行的命令。
全局配置中其他已启用的 MCP 服务会进入每个 agy 回合并在远端运行，启动时会提醒，可用 `agy mcp disable <名称>` 关闭。

agy 没有关闭内置工具的参数。`init` 事件列出它自己的 60 个工具（`run_command`、`view_file`、`list_dir`、
`write_to_file`、`browser_*`、`search_web`、子代理等），MCP 工具不单独列出，而是通过其中的 `call_mcp_tool` 调用。
已登录的真实记录里 `init.permission_mode` 是 `always-proceed`，`run_command` 直接在 agy 所在的主机上执行了 `ls -la`，
没有任何确认。服务让模型照常使用这些工具，但让它们作用在用户的电脑上，见下文“agy 的内置工具在用户电脑上执行”。

不管内置工具在哪里执行，都要在远端账号的 `~/.gemini/antigravity-cli/settings.json` 中允许本服务的 MCP：

```json
{ "permissions": { "allow": ["mcp(komputer_use/*)"] } }
```

服务启动时检查这条规则，缺少时在日志里提醒。程序里还能找到 `disabledTools`、`excludeTools` 等设置名，
但它们的效果没有验证，服务不会写入，也不依赖它们。

agy 不接受图片和 PDF 附件，带附件的请求返回 400。

每次调用 agy 有约 6.6k 输入 token 的基础提示开销，单次 11–17 秒，进程约 240MB 内存。
`KOMPUTER_MAX_IN_FLIGHT` 按内存设置；后台任务（`### Task:` 标题等）默认用 `gemini-3.8-flash-low`
（`KOMPUTER_AGY_TASK_MODEL`）。agy 支持在一个进程里通过 stdin 连续多轮，服务目前每个回合启动一个进程并用
`--conversation` 续接。

**Muse。** Body 和 harness 模式都通过 `-c mcp_servers.<名称>=...` 传入 MCP，并设为 `required`；
MCP 起不来时回合失败，不会在没有工具的情况下继续。URL 和请求头通过 `${变量}` 从子进程环境读取，
不会出现在命令行里。island 模式下另加 `--disable-shell --disable-write --disable-web-tools`。
总会加 `--disable-approval --disable-request-user-input --no-foreign-personal-context`。
图片附件写成 0600 临时文件，通过 `--image` 传入，回合结束后删除。

两个 CLI 都在仓库外的独立工作目录中运行（`~/.komputer/workspaces/<实例摘要>-agy`、`-muse`），
子进程环境同样经过筛选，名单见 [claude-env.mjs](../komputer-api/claude-env.mjs)。

### agy 的内置工具在用户电脑上执行

评估过三种办法：

| 办法 | 结论 |
| --- | --- |
| (a) 给 agy 一个以 Body 为后端的环境：PATH 上的 `bash` 替身转发命令，Body 根目录的 FUSE 挂载作为工作区 | **采用**，已用真实 agy 验证 |
| (b) 从 stream-json 截获工具步骤，由服务给出结果 | 不可行。agy 在自己进程里执行工具，stream-json 只是事后报告；stdin 只接受 `user` 事件；hook 也不能提供工具结果 |
| (c) agy 自带的远程执行或沙箱设置 | CLI 没有暴露可用的设置。程序里有 `RunCommandToolConfig`（`shell_path`、`execution_host` 等）和 IDE 用的 SSH 远程，但没有对应参数或设置项；`--sandbox` 只限制终端，`--remote-control` 是从别的设备操作本机 agy。可用的是 PreToolUse hook，用作策略层 |

用真实 agy 1.4.3 测到的行为（不登录：`settings.json` 写 `{"modelProvider":"gemini"}`，`GEMINI_API_KEY` 任意，
`GOOGLE_GEMINI_BASE_URL` 指向 [假 Gemini 服务](../komputer-api/test/fake-gemini.mjs)，由它下发工具调用）：

- `run_command` 执行 `bash -c <CommandLine>`，`bash` 从 PATH 查找（不看 `$SHELL`），在 PTY 中运行（输出换行是 `\r\n`），
  工作目录是工作区，环境是 agy 自己的环境加 `ANTIGRAVITY_*`。
- 文件工具在 agy 进程内直接读写磁盘，不启动子进程。
- 启动时只在工作区根目录查看 17 个名字（`.git`、`AGENTS.md`、`GEMINI.md`、`.agents` 等），不遍历整棵树，也不往工作区写文件。
- hook 写在 `~/.gemini/config/hooks.json`：`{"<名称>": {"PreToolUse": [{"matcher": ".*", "hooks": [{"type": "command", "command": "...", "timeout": 10}]}]}}`。
  命令经 `sh -c` 在配置目录运行，环境与 agy 相同，stdin 是 `{toolCall: {name, args}, stepIdx, conversationId, workspacePaths, artifactDirectoryPath, ...}`。
  输出为空表示不干预；`{"decision":"deny","reason":...}` 让工具步骤以 `ERROR`（“tool call denied by pre-tool hook: ...”）结束；
  `{"decision":"allow","overwrite":{...}}` 改写参数（浅合并）；**`{}` 或缺少 decision 也算拒绝**。`allow` 不能解除 headless 的权限拒绝。
- 权限：传 `--dangerously-skip-permissions` 时是 `always-proceed`；不传时是 `request-review`，headless 下 `run_command`
  被自动拒绝（stderr 说明，回合结束），除非 `permissions.allow` 有 `"command(*)"`（只写 `"command"` 无效）。
  工作区内的文件读写允许，工作区外的被拒绝。
- 这条 API key 路径只有 12 个工具（`read_file`、`write_file`、`edit_file`、`run_command`、`manage_task`、子代理、
  `search_web`、`ask_question` 等）；已登录的 `view_file`、`list_dir`、`grep_search` 在这里是未知工具。hook 两套名称都处理。

做法（`KOMPUTER_AGY_REMOTE_TOOLS`，默认 `auto`）：

- **命令。** 服务在 `<agy 工作目录>/shim/bash` 生成包装脚本，Body 回合把这个目录放在 agy 的 PATH 最前。
  只有本回合的目标文件里有 `remote` 时，它才运行 [agy-bash-shim.mjs](../komputer-api/backends/agy-bash-shim.mjs)，
  其余调用交给真实 bash。命令通过 Body 的 `start_process` 启动，`read_process` 流式取回输出，stdin 经 `write_process` 转发，
  agy 终止命令时调用 `stop_process`，退出码原样返回；没有 `start_process` 的 Body 退回 `run_command`。
  工作目录按相对工作区的位置换到 Body 上；命令里的工作区路径换成 Body 路径，输出里的 Body 路径换回工作区路径（仅 POSIX 根目录），
  所以模型看到的始终是同一个工作区。远端 shell 默认 `bash`（Windows Body 为 `default`），可用 `KOMPUTER_AGY_REMOTE_SHELL` 指定。
- **文件。** [komputer-fs.py](../komputer-api/backends/komputer-fs.py)（fusepy）把 Body 根目录挂载到
  `<agy 工作目录>/remote`（`KOMPUTER_AGY_REMOTE_MOUNT`），它就是 Body 回合里 agy 的工作区。每个文件操作变成 Body MCP 调用
  （`stat_path`、`list_directory`、`read_file` / `write_file` 的 base64 模式、`create_directory`、`delete_path`、`move_path`）。
  属性缓存 2 秒，读取按 512KB 预读，写入在关闭或 fsync 时整体写回。整个服务共用一个挂载，第一次使用时建立，
  Body 地址或 token 改变时重建，失败后 30 秒内不重试。服务退出时（包括被强制结束）守护进程的 stdin 关闭，它会自己卸载。
- **策略。** `KOMPUTER_AGY_MCP_SETUP=auto` 时服务在 `~/.gemini/config/hooks.json` 注册 `komputer` 键（只增改这个键），
  运行 [agy-hook.mjs](../komputer-api/backends/agy-hook.mjs)。它只在服务自己的回合里有意见（策略写在 `KOMPUTER_AGY_MCP_FILE` 指向的文件里），
  手动运行 agy 时什么也不输出，由 agy 自己的设置决定。
  - Body 回合（remote）：命令放行。文件工具的路径必须在工作区内或 agy 自己的 brain / artifact 目录里；
    命令输出里出现的 Body 路径会被改写到挂载上；其他绝对路径（如 `/etc/hostname`、`~/.ssh/...`）拒绝。
    没有挂载时文件工具一律拒绝，并提示模型改用 `run_command`。`browser_*` 等只能在服务主机上运行的工具拒绝，
    `search_web` 等只联网的工具放行，不认识的新工具拒绝。
  - harness 回合和后台任务（island）：只允许 MCP 和 agy 内部工具。
  - `KOMPUTER_BUILTIN_TOOLS` 非空的 Body 回合（local）：不干预，内置工具在服务主机上运行（以前的行为）。
- **兜底（`KOMPUTER_AGY_BUILTIN_GUARD`，默认 `warn`）。** 解析器按同一策略检查已经执行的工具步骤。hook 已注册时只看 DONE
  的步骤（被 hook 拒绝的步骤是 ERROR）；没有 hook 且 `permission_mode` 为 `always-proceed` 时在 ACTIVE 就判断。
  `warn` 记录日志；`stop` 结束 agy 进程并返回 `502 builtin_tool_blocked`（流已开始时在流内报告），
  但**已经开始的那条命令可能已经执行完**；`off` 关闭。`call_mcp_tool` 和 agy 内部工具不受影响。

| `KOMPUTER_AGY_REMOTE_TOOLS` | `run_command` | 文件工具 |
| --- | --- | --- |
| `auto`（默认） | 在用户电脑上 | 本机能用 FUSE 时经挂载在用户电脑上，否则拒绝 |
| `all` | 在用户电脑上 | 必须挂载，挂不上时回合返回 `503 body_unavailable` |
| `commands` | 在用户电脑上 | 拒绝，模型用 `run_command` |
| `off` | 被 hook 拒绝（island） | 被 hook 拒绝（island） |

运行前提：

- **agy 权限。** headless agy 默认拒绝 `run_command`。二选一：`KOMPUTER_AGY_PERMISSIONS=skip`（传 `--dangerously-skip-permissions`，
  由 hook 限制），或在 `settings.json` 的 `permissions.allow` 加 `"command(*)"`。这个文件是全局的：
  服务账号手动运行 agy 时命令同样不再询问，而那时 hook 不干预，命令在服务主机上执行。启动时检查并提醒。
- **FUSE（只有文件工具需要）。** Linux，`/dev/fuse`，`fusermount3`（`fuse3` 包），python3 与 fusepy（`python3-fusepy` 包），
  另需 `/etc/mtab`（容器里可能要 `ln -s /proc/self/mounts /etc/mtab`，否则无法卸载）。缺少时 `auto` 退回 `commands`，启动日志列出缺少的部分。
  解释器可用 `KOMPUTER_AGY_PYTHON` 指定。
- **Body 是 HTTP MCP。** `KOMPUTER_MCP_CONFIG` 中 `KOMPUTER_BODY_MCP` 那一项要有 `url`；stdio 的 Body 回合退回 island。

取舍和限制：

- 延迟：每条命令多一次 node 启动和几次经 SSH 隧道的 Body 往返；每个文件操作都是一次 Body 调用，
  在挂载上做大范围搜索会慢。agy 启动时约 17 次 stat。
- 单个文件的读写受 Body 的 `maxFileBytes`（默认 8MB）限制。
- 属性缓存 2 秒；用户同时在电脑上改同一个文件时，后写的覆盖先写的。
- 长时间运行的命令：shim 一直等到远端进程结束，agy 照常把超过 `WaitMsBeforeAsync` 的命令当作后台命令；
  `command_status`、`send_command_input` 没有逐项验证。
- Windows Body：远端 shell 不是 bash，路径不做映射；agy 的系统提示里的操作系统仍是服务主机的 linux。
- agy 自己如果在 Body 回合里另外调用 `bash -c`，也会被转发到 Body；目前没有观察到这种调用。

**Claude 不变。** Claude Code 的 Bash 工具同样启动 shell，文件工具同样在进程内，同一套 shim、挂载和 PreToolUse hook
原则上也适用，但没有验证；Claude 的参数和环境受逐字节回归测试约束，所以这次没有改。

## 哪些验证过，哪些是假设

替身 CLI（`test/fake-agy.mjs`、`test/fake-muse.mjs`）复现了下表中已验证的输出格式。
`test/fixtures/` 里是真实 CLI 的输出记录，解析器单元测试直接读取它们。
设置 `KOMPUTER_TEST_REAL_AGY` / `KOMPUTER_TEST_REAL_MUSE`（可选 `_HOME`）可以对本机真实 CLI 跑冒烟测试，默认跳过。

### Antigravity（agy 1.4.3）

“已登录验证”来自项目维护者在已登录的真实主机上的结果，其中运行 `ls -la` 的完整 stream-json 记录（已去掉账号名和会话 ID）
保存在 `test/fixtures/agy-1.4.3-signed-in-run-command.jsonl`，解析器单元测试直接读取它，替身 agy 也按它的结构输出。
“未登录验证”是在没有登录的环境里对同一版本程序的检查，包括 `real-cli.test.mjs` 的冒烟测试。

| 已登录验证 | 未登录验证 | 假设，可通过配置调整 |
| --- | --- | --- |
| print 模式和 `--output-format stream-json` | 参数 `--input-format stream-json`、`--model`、`--effort`、`--conversation`、`--dangerously-skip-permissions`、`--mode` 存在 | 解析器仍接受旧的扁平 `step_update` 结构并忽略未知事件，以防版本变化 |
| `init`：`conversation_id`，`init.cwd`、`init.permission_mode`（记录中为 `always-proceed`）和 60 个内置工具名；MCP 经 `call_mcp_tool` | 未登录时的 `result` 行、stderr 中的 `authentication required` 和退出码 1，映射为鉴权错误 | 已登录时不传 `--dangerously-skip-permissions` 的 `permission_mode`（API key 路径见下表，为 `request-review`）；`permissions.deny` 与 skip 参数的优先级 |
| `step_update` 的字段都在 `step_update` 对象里：`step_index`、`state`（ACTIVE/DONE）、`step_type`（`user_input`、`agent_response`、`tool`） | MCP 只读全局 `~/.gemini/config/mcp_config.json` 和插件；工作目录下的 `.agents/mcp_config.json` 不会读取 | `call_mcp_tool` 的参数名 `ServerName`、`ToolName`（记录里只有内置工具；解析器也接受 `server_name` / `tool_name` 等写法） |
| 文本增量是 `agent_response` 步骤的 `text_delta`，DONE 步骤也可能带最后一段；调用工具前的 `agent_response` 步骤可以没有文本 | `agy mcp add` 写出的格式（HTTP 为 `serverUrl` + `headers`，stdio 为 `command` + `args` + `env`），并会重新启用同名条目 | `disabledTools`、`excludeTools` 设置的效果 |
| 工具步骤有 `tool_name` 和 `tool_info.{name, parameters, output}`（如 `run_command` 的 `CommandLine`） | stdio MCP 服务继承 agy 的环境变量和工作目录；会话开始时先发 `server/discover`，失败后回退到 `initialize`（2025-11-25），接受 2025-06-18 的回复并调用 `tools/list` | 上面列为“不接触主机和网络”的内置工具确实如此；已登录工具集里 `view_file`、`list_dir`、`grep_search`、`find_by_name` 的参数名（hook 按以 path / file / dir 结尾的参数名检查） |
| `result`：`status`、`response`（等于全部文本增量）、`duration_seconds`、`num_turns`、`usage`；每个 DONE 的 `agent_response` 步骤也带自己的 `usage` | 通过转接器，真实 agy 用 Bearer 头完成 Body MCP 握手并列出工具 | |
| 用量：`input_tokens` 不含缓存读取，`total_tokens` = 输入 + 输出，`cache_read_tokens` 单独计；thinking 只有 `thinking_tokens`，没有文本 | `--conversation` 指向不存在的会话时，agy 打印警告后新开会话；服务把它视为会话丢失 | |
| 模型：`gemini-4-argon`、`gemini-3.8-flash`，另有 `claude-opus-5-5`、`claude-sonnet-5-5`、`gpt-oss-120b-medium` | | |
| 强度：模型名后缀或 `--effort low\|medium\|high`，两者同时用会被拒绝；Gemini 拒绝 `xhigh`/`max` | | |
| `--conversation <id>` 续接；同一进程内通过 stdin 多轮可用 | | |
| 基础提示约 6.6k 输入 token（记录中第一步 6611），单次 11–17 秒，约 240MB 内存 | | |

内置工具的执行位置用真实 agy 1.4.3 的 API key 路径验证（假 Gemini 服务下发工具调用，真实 Body 代表用户电脑，
`KOMPUTER_TEST_REAL_AGY` 的冒烟测试会重跑）：

| 用真实 agy 验证 | 假设 |
| --- | --- |
| `run_command` 是 PATH 上的 `bash -c`，在 PTY 中、工作区目录下运行；替身转发后 `ls -la` 列出的是 Body 根目录，退出码原样返回 | 已登录工具集的 `run_command` 也这样启动（同一个程序，已登录记录里的输出同样是 `\r\n`） |
| 文件工具在进程内读写；工作区是挂载时 `read_file`、`write_file`、`edit_file` 都落到 Body 上 | 已登录时 `grep_search` / `find_by_name` 用内嵌 ripgrep（changelog），同样只读工作区所在的挂载（ripgrep 在挂载上可用） |
| hooks.json 的格式、stdin 字段、`deny` / `allow`+`overwrite` / 空输出 / `{}` 的效果；hook 继承 agy 的环境 | hook 超时或崩溃时 agy 的处理（服务按 hook 没有生效处理，由解析器兜底） |
| `--dangerously-skip-permissions` 时 `always-proceed`，不传时 `request-review`；headless 下 `run_command` 被拒绝，`"command(*)"` 放行，裸 `"command"` 无效 | 已登录账号的权限默认值相同 |
| agy 启动时只 stat 工作区根目录的 17 个名字，不写入工作区 | 更长的会话（计划、任务文件）只写到 `~/.gemini/antigravity-cli/brain/<会话>` |

### Muse Code（muse 1.4.11，echo provider）

| 已对真实 CLI 验证 | 假设，可通过配置调整 |
| --- | --- |
| `exec --json` 的 JSONL 结构，`run.output.delta` 文本、`run.usage`、`run.terminal.completed` / `failed` | 真实模型调用工具时的 task 名称；解析器依次读取 `tool_name`、`name`、`task_kind` |
| `--session-id` 新建时第一条会话记录序号为 1，续接时为 2；续接却得到 1 视为会话丢失 | `--disable-approval` 下 MCP 工具不再请求确认 |
| 换工作目录续接时的拒绝信息 | 用量字段在真实模型下的含义 |
| 没有凭据时只在 stderr 报错，映射为鉴权错误 | muse 不输出 thinking 文本，所以 `reasoning_content` 为空 |
| `required` MCP 起不来时返回 `run.terminal.failed`，映射为 `body_unavailable` | |
| `-c mcp_servers` 中 url 和 headers 的 `${变量}` 替换（本地 MCP 服务确实收到了替换后的 Bearer） | |

本服务还没有在已登录的 agy 或 muse 上跑过完整回合；agy 的工具调用只在上面的 API key 路径上跑通过。
上线前按 [安装步骤](../SETUP.md) 的方法，先让模型运行 `hostname` 并核对工具记录。
