# 远程大脑设计（brain-in-a-vat / island mode）

扫描基线：`main` `7cb0323`（PR #1 已合并）。API `kloud-kode-api` 0.10.0，工作站 `kloud-kode-body` 1.3.0。
本文只记录目标、现状和远程大脑这一条下一步。不改代码。

`docs/FEATURES-inventory.md` 写于合并前的 `90753b7`，最后一节把「关掉官方内置工具、改走 MCP」当成设计、把内置工具转发当成偏离。合并后的代码默认确实如此。那不是本文的目标。本文以原始远程大脑意图为准。

## 目标

模型永远只相信一台机器：调用者正在用的那台电脑（常常是 Windows）。shell 的 stdout、stderr 和退出码必须来自那台电脑。

官方客户端（`agy`、`muse`、`claude`）可以跑在 Linux 上。工具仍然在调用者的操作系统上执行。大脑在 Linux、身体在 Windows 时，PowerShell、路径映射和换行都要按调用者的机器处理。映射是实现细节，模型看见的路径、shell 和结果都是调用者的。

优先把官方客户端自己的内置工具透明转到调用者机器上（harness 或 Body）。模型继续调用它已经会的工具名（对 agy 就是 `run_command`、`read_file` 一类）。MCP 遥控桩（`komputer_use`、`k_<序号>_<摘要>`）不是模型的主要双手。内置工具被调用时，回合继续，把调用者的结果交回去。

Island mode 是这台机器的口吻：模型可见的文字不出现两台机器，也不出现 “service host”。现实由 `system_info` 以及 Body / harness 的真实执行呈现：操作系统、shell、时区、根目录、退出码、输出。

先做完远程大脑。半代理 / 混合模式只记为以后再说，本文不设计它。

默认 `KOMPUTER_BACKENDS=claude` 的纯 Claude 通路保持今天的行为。见下一节。

## 非目标

- 不设计半代理 / 混合模式。那是「官方内置工具关掉或只转一部分，同时把另一套 MCP 工具交给模型当双手」，以及「有的官方工具留在跑 CLI 的主机上、有的转到调用者」。现状里已有这种默认。把它做成长期产品，不在这一步。
- 这一步不改默认 Claude 通路，也不把 Muse、Claude 的内置工具转发排进同一批验收。
- 不用伪造操作系统代替真实执行。改提示里的 OS 字样、改工具名字，都不改变命令实际跑在哪。
- 不把「拒绝内置工具并结束回合」当成隔离手段。
- 不在模型可见文字里解释有两台机器。
- 不做 GUI / 浏览器 / 多台调用者电脑。Body 已有的 shell、文件、进程是执行底座，不是这一步的新产品。
- 不做 token 代理或账号池。上游看见的仍是官方 CLI 自己的登录。

## 现状

下面是 `7cb0323` 上实际存在的行为。

### 默认 Claude 通路原样保留

`KOMPUTER_BACKENDS` 默认是 `claude`（`komputer-api/backends/index.mjs`）。未设置时，服务就是合并前的 Claude 服务：每个回合启动官方 `claude`，参数、stdin 和子进程环境由 `test/claude-regression.test.mjs` 固定。

这条通路今天的工具做法是：

- `--tools ""`（`KOMPUTER_BUILTIN_TOOLS` 默认空）。Bash、Read、Write、Edit、Glob、Grep、Task、网页工具都关掉。`default` 才会恢复 CLI 自带工具，那些工具跑在 CLI 所在的主机上。
- 没有客户端工具时走 Body：`komputer-session/.mcp.json` 里的 `komputer_use`，工作说明要求模型用 `mcp__komputer_use__*`，并先调用 `system_info`。
- 请求带非空 `tools` 或显式 `tool_choice` 时走 harness：`client-tools.mjs` 在本机回环上起一个只含本次客户端工具的 MCP，工具名是 `k_<序号>_<摘要>`，结果由调用者的 harness 执行后经同一条 HTTP 连接返回。官方内置工具同样关掉。
- 工作说明的注释要求只描述一台电脑，不提启动器、第二台机器或本地/远端（`server.mjs` 里 `DEFAULT_WORK_NOTE` 上方）。说明正文仍然把 MCP 工具写成双手。

这条默认通路在远程大脑做完之前保持这样。打开 `antigravity` 或 `muse` 才进入下面的多后端行为。

### 多后端和 Responses

每个回合仍启动对应的官方 CLI。服务只拼 argv、写 stdin、读 JSON。不模拟客户端协议。

| 后端 | CLI | 关掉 CLI 自己的工具（今天） | 模型实际用的双手（今天） |
| --- | --- | --- | --- |
| Claude Code | `claude` | `--tools ""` | Body 的 `komputer_use`，或 harness 的 `k_*` |
| Antigravity | `agy` | 没有等价开关。默认 PreToolUse hook 拒绝内置工具（`KOMPUTER_AGY_REMOTE_TOOLS=off`） | 同上，经全局 `~/.gemini/config/mcp_config.json` 里的 stdio 桥 `agy-mcp-bridge.mjs`，再 `call_mcp_tool` |
| Muse Code | `muse` | `--disable-shell --disable-write --disable-web-tools` | HTTP MCP，`-c mcp_servers.<名称>`，`required`。stdio Body 配置会被跳过 |

`POST /v1/responses` 已落地（`responses.mjs`）。`instructions`、`reasoning.effort`、`previous_response_id`、function 工具和 `function_call_output` 转成现有的 Chat 流程。function 工具仍进 harness。`web_search` 等 Responses 内置工具返回 400。它没有单独的远程大脑路径。

agy、muse 没有系统提示参数，说明放进用户回合的 `<system_instructions>`（`backends/common.mjs` 的 `inlineSystem`）。Claude 仍用 `--append-system-prompt-file`。

### agy 上已经有的内置工具转发（默认关，且不是 Windows）

agy 1.4.3 的已登录记录里，`run_command` 在 agy 所在主机上直接执行了 `ls -la`，`permission_mode` 为 `always-proceed`，没有确认（`test/fixtures/agy-1.4.3-signed-in-run-command.jsonl`）。未登录验证：`run_command` 是 PATH 上的 `bash -c`，在 PTY 里、工作区目录下运行；文件工具在进程内读写磁盘。

对应实现：

- `agy-bash-shim.mjs`：PATH 最前面的 `bash` 把 `bash -c` 转到 Body 的 `start_process` / `read_process`（没有则退回 `run_command`），退出码原样返回。路径改写只认 POSIX 根（`toRemote`：根不是 `/` 开头则原样返回）。
- `komputer-fs.py`：FUSE 把 Body 根挂到工作区，文件工具因此落在 Body 上。只在有 `/dev/fuse` 的 Linux 上可用。
- `agy-hook.mjs`：`remote` 时把路径限制在挂载里；`island` 时拒绝内置工具。

`KOMPUTER_AGY_REMOTE_TOOLS` 默认 `off`。`auto` / `all` / `commands` 只对 `system_info.platform !== "win32"` 的 Body 生效。Body 报 `win32` 时，即使打开，也记一条日志后仍走 island：agy 自己的工具关掉，改用 `komputer_use`（`antigravity.mjs` 的 `prepare`）。harness 回合和 `### Task:` 回合固定 island，不转发 agy 的 `run_command`。

曾经有过一版 Windows 内置工具（`50440d9`：PowerShell、`C:\` 映射、CRLF、简单 POSIX 命令的翻译），随后被 `0baeda3` 撤回。`487a309` 把默认改成现在的共用 MCP 设计，并把转发标成 POSIX 实验。那一版不要整包恢复：它的拒绝理由里仍然有两台机器的说法，工具输出里还会插入翻译说明。要留下的是执行事实：命令在 Windows 上、用调用者的 PowerShell、路径是调用者的 `C:\`。

### 调用者机器上的执行底座

Body 1.3.0 跑在调用者电脑上。`system_info` 报告这台机器的 `platform`、`osVersion`、时钟、时区、`defaultShell`、`availableShells`、控制台编码、根目录和限制。工具说明的测试要求不出现第二台机器（`kloud-kode-body/test/body.test.mjs`）。

Windows 上（`server.mjs` 的 `detectShells` / `powershellArgs`，CI 的 `test/windows-host.test.mjs`）：

- 默认 shell 是 `cmd.exe`。`powershell`、`pwsh`、`cmd`、Git Bash、`wsl` 分开。System32 里的 `bash.exe` 是 WSL 启动器，不算 Git Bash。
- Windows PowerShell 5.1 用带 BOM 的临时 `.ps1`，避免 CLIXML 错误和 ANSI 代码页。表格行去掉补到列宽的空格，CRLF 保留，退出码按最后一条语句。
- cmd 用代码页 65001。进程树用 `taskkill /t`。`idleTimeoutMs` 是无人读取后的租约。

harness 不经过 Body。结果是调用者客户端自己执行后回传的。模型看见的是 `k_*`，不是官方 CLI 的 `run_command`。

### Windows 上跑测试时已经修过的两处

`3760b81`，只影响测试和路径判断，不改变 Linux 上的产品默认：

- `fileURLToPath(import.meta.url)`。`new URL(...).pathname` 在 Windows 上不是文件系统路径，hook 和 bash shim 的入口判断会失败，hook 进程等于没跑。`agy-hook.mjs`、`agy-bash-shim.mjs` 已改。
- `os.homedir()` 在 Windows 上读 `USERPROFILE`，不读 `HOME`。测试夹具同时设置 `USERPROFILE`、`HOMEDRIVE`、`HOMEPATH` 和 `HOME`（`test/helpers.mjs`）。只设 `HOME` 会把 agy 的 hook 写进开发者自己的 `~/.gemini`。
- 路径按字符串的风格选 `path.win32` 或 `path.posix`（盘符或 `\\` 用 win32）。hook 命令在 win32 上用双引号。

子进程环境允许名单里本来就有 `USERPROFILE`、`APPDATA`、`HOMEDRIVE`、`HOMEPATH`（`claude-env.mjs`）。这是让 CLI 在 Windows 主机上能登录，不是把调用者的 Windows 配置映射进 Linux 大脑。

### 模型今天实际相信什么

同时存在三套说法：

1. CLI 自己写进系统提示的环境改不掉。Claude 的 `<env>` 是 `Platform: linux` 和大脑的工作目录。agy 写 “The USER's OS version is linux”。
2. 工作说明和（若模型去调）`system_info` 描述调用者的电脑。
3. agy 的 hook 在拒绝时直接对模型说内置工具会作用在 “service host” 上，并让它改用别的工具。解析器在 `KOMPUTER_AGY_BUILTIN_GUARD=stop`（默认是 `warn`）时结束子进程，客户端看到 `502`，类型 `builtin_tool_blocked`，文案同样写了 “service host”。

agy 的 MCP 工具默认懒加载。模型要先用 `read_file` 读 `~/.gemini/antigravity-cli/mcp/<服务>/<工具>.json`，再 `call_mcp_tool`。island 策略因此放行对这个目录的只读。那条路径在跑 agy 的主机上，不在调用者的 Windows 上。

## 对齐点

这些已经符合目标，远程大脑应沿用，而不是重做。

- 官方 CLI 跑在一侧，调用者的电脑在另一侧。上游凭证留在 CLI 自己的登录里。大脑可以是 Linux。
- 一次回合一个 CLI 进程。结果以 CLI 自己的 JSON 为准，不伪造工具成功。
- Body 的 `system_info` 和 shell/文件/进程工具报告并作用在调用者机器上。Windows 的 PowerShell 5.1、cmd UTF-8、CRLF、退出码、进程树已经在真实 Windows CI 上测过。
- Body 工具文案不叙述第二台机器。工作说明的注释也要求单一机器口吻。
- harness 的执行在调用者侧，云端不反向连接调用者。
- 子进程环境按名单过滤，工作目录在仓库外，并设置 `GIT_CEILING_DIRECTORIES`。部署秘密不进 CLI。
- agy 的机制事实已经量过：`run_command` 找 PATH 上的 `bash -c`，文件工具在进程内读写，PreToolUse 能改参数或拒绝，空 stdout 表示不干预，`{}` 缺少 decision 会被当成拒绝。POSIX Body 上，shim 加 FUSE 能让内置工具的副作用出现在 Body 上。这证明转发可以透明，不是从流里事后补结果。
- `fileURLToPath` 和 `USERPROFILE` 隔离已经写进测试。以后在 Windows 上加入口脚本或隔离 home，沿用这两条。
- 默认 `KOMPUTER_BACKENDS=claude` 与回归测试仍在，纯 Claude 通路没有被多后端改写。

## 缺口

相对目标，这些还没有。

1. **模型的双手不是官方内置工具。** 三个后端的默认都是关掉或拒绝 CLI 自己的 shell/文件工具，再把 MCP 桩交给模型。agy 的转发默认关闭。这和「透明转发官方内置工具」相反。MCP 可以留在转发的底下（shim 今天就是经 Body MCP 执行的），但不能是模型被教会去调用的那一层。
2. **Windows 调用者没有内置 `run_command` 转发。** 大脑在 Linux、Body 报 `win32` 时，打开 `KOMPUTER_AGY_REMOTE_TOOLS` 也会退回 island。shim 不改写非 POSIX 根，远端 shell 默认 `bash`。没有 PowerShell 路径。验收标准因此今天达不到。
3. **harness 不转发官方内置工具。** agy 的 harness 回合策略固定为 island：`run_command` 被拒绝，调用者执行的是 `k_*`。目标要求同一条 `run_command` 的结果来自调用者，无论执行者是 Body 还是 harness。
4. **文件工具在 Windows 调用者上没有落点。** FUSE 挂载不能代表 `C:\`。撤回的那一版把 Windows 根映射到挂载下的 `c/Users/...`，当前树里没有。
5. **Muse 只有关闭开关，没有把自己的 shell/写入转到调用者。** Claude 只有 `--tools ""`，没有把 Bash/Read/Write 转到调用者。二者都留到 agy 的 Windows `run_command` 验收之后。
6. **模型看见的工作区仍是大脑的。** 默认工作目录是 `~/.komputer/workspaces/<摘要>`。CLI 的系统提示仍写 Linux。没有一层把模型要用的路径显示成调用者的 `C:\`，同时让读写落在那台机器上。
7. **懒加载工具说明把模型带进大脑的 home。** 要读 `~/.gemini/antigravity-cli/mcp/...` 才知道 `k_0_…` 是什么。远程大脑不把这条读盘当成正常路径。
8. **Responses 没有改变执行位置。** function 工具进入现有 harness；不带工具的请求进入现有 Body。

## 冲突

这些是现状里和目标对着干的行为。下一步要拆掉它们在远程大脑路径上的作用，而不是再加一套。

- **拒绝文案告诉模型有两台机器。** `agy-hook.mjs` 在 island 策略下的理由是：内置工具会作用在 “service host” 上，让模型改用提供的工具。remote 策略下，浏览器类工具、挂载外的路径，理由同样写出 “service host” 和 “the user's computer”。这些字符串会进模型。Island mode 不允许这种口吻。
- **内置工具会结束回合。** `KOMPUTER_AGY_BUILTIN_GUARD=stop` 杀掉 agy 并返回 `builtin_tool_blocked`。默认 `warn` 只打日志，但 `stop` 已实现并有测试。已登录且 `always-proceed` 时，拒绝发生之前命令可能已经在大脑上跑完。目标是：内置工具的调用得到调用者的结果，回合不因此结束。
- **工作说明把 MCP 桩写成双手。** `DEFAULT_WORK_NOTE` 写明用 `mcp__komputer_use__*` 到达 shell 和文件。这和「官方内置工具才是双手」冲突。`system_info` 本身没有这个问题。
- **文档把 MCP 默认写成设计。** `docs/backends.md` 的「与 Claude 共用的设计」、`docs/tool-redirection.md` 末节、`env.example` 里对 `KOMPUTER_AGY_REMOTE_TOOLS` 的注释、`FEATURES-inventory.md` 第 15 节，都把关掉内置工具当成目标，把转发当成可删的 POSIX 实验。它们描述的是今天的默认，不是本文的目标。
- **`KOMPUTER_BUILTIN_TOOLS` 非空时，官方工具跑在 CLI 主机上**（agy 的 hook 策略 `local` 不干预）。结果不是调用者的。这是今天的开关，不是远程大脑的做法。

## 已知坑

做远程大脑时会踩到，和上面的冲突不是同一类。

- **入口脚本必须用 `fileURLToPath`。** 否则在 Windows 上 hook / shim 根本不运行，内置工具就在大脑上执行。Linux 上两种写法碰巧都能对上。
- **隔离 agy 的 home 时，Windows 要设 `USERPROFILE`，不能只设 `HOME`。** 否则 `os.homedir()` 指向真实用户，`agy mcp add` 和 `hooks.json` 写进真实的 `~/.gemini`。
- **同一条策略里会同时出现 POSIX 路径和 `C:\` 路径。** 用路径自己的风格做 `resolve` / `relative`，不要用进程所在操作系统的 `path`。
- **agy 的 `run_command` 没有 shell 参数，启动方式固定是 `bash -c`。** 调用者是 Windows 时，这串命令要在这台电脑的 PowerShell 里执行（有 `pwsh` 用 `pwsh`，否则 5.1）。Git Bash 只作内部兜底，不能用 System32 的 `wsl.exe`（那是另一套 Linux 路径）。Body 自己的 `run_command` 在 Windows 上默认仍是 `cmd.exe`，`system_info.defaultShell` 会这么报。agy 这条路径的说明必须把「这台电脑上敲命令用的 shell」说成 PowerShell，并且只说这一台电脑，否则模型会按 cmd 或 bash 来写。简单 POSIX 命令可以在内部译成 PowerShell 再执行；工具输出里不要再加一段翻译说明，否则模型看见的不是这台电脑自己的 shell 结果。
- **PowerShell 5.1 不能用裸 `-Command` 或无 BOM 的脚本。** 错误会变成 CLIXML，表格会被补空格，退出码不是原生程序的退出码。Body 1.3.0 的 `powershellScript` / `powershellArgs` 已经处理。转发应调用这层，不要在 shim 里另写一套。
- **CRLF 是调用者文件的内容。** 编辑工具把 CRLF 当成不匹配，或把文件改成 LF，都不是这台 Windows 电脑的结果。agy 的 PTY 在 Linux 上也会把输出变成 `\r\n`，不能靠「看见 `\r\n`」判断执行位置。
- **hook 的空输出和 `{}` 相反。** 空输出表示不干预，工具接着在大脑上跑。没有 decision 的 `{}` 会被 agy 当成拒绝。转发路径要明确允许并改写参数，不能靠沉默。
- **`always-proceed` 加上 PATH 上没有 shim，命令就在大脑上执行。** 已登录记录就是这样。验收要看调用者机器上的副作用，不能看模型怎么说。
- **FUSE 解决不了 Windows 根。** 它把文件字节搬到大脑上搜索，CPU 在大脑。Windows 调用者的文件工具要在调用者上执行（Body 的文件工具或 harness），路径映射只负责让 agy 进程内的读写对上 `C:\`。
- **全局 `hooks.json` 和 `mcp_config.json` 属于运行服务的那个系统用户。** 手动开的 agy 会带上同一个 hook。hook 在没有本回合策略文件时必须不输出，这个行为要保持。
- **已登录工具名和 API key 路径的工具名不一样**（`view_file` / `list_dir` / `grep_search` 对 `read_file` / `write_file`）。转发和放行名单要两套都认。
- **CLI 自己的 “OS is linux” 一行去不掉。** 不要用第二段文字向模型解释「忽略另一台主机」。工作说明只描述调用者这一台，并让它用 `system_info` 的结果下命令、写路径。完全替换系统提示的 `KOMPUTER_SYSTEM_PROMPT_FILE` 只有 Claude 有，仍是实验，这一步不靠它。

## 推荐下一步（只远程大脑）

只做 agy。官方客户端跑在 Linux，调用者是 Windows，模型调用的是 agy 自己的 `run_command`，结果来自那台 Windows。默认 `KOMPUTER_BACKENDS=claude` 不动，`claude-regression` 保持通过。

1. **Body 回合。** Linux 上的 agy 发出 `run_command` 时，命令在调用者的 Windows 上执行。shell 用这台电脑的 PowerShell（有 `pwsh` 用 `pwsh`，否则 Windows PowerShell 5.1），经 Body 1.3.0 已有的启动方式。退出码、stdout、stderr、CRLF 原样回到 agy。模型看见和使用的路径是调用者的 `C:\` 路径；Linux 工作区和 `C:\` 之间的改写留在 shim / hook 内部。
2. **回合不因这次内置调用而结束。** 这条路径上不使用会杀掉进程的 guard。转发失败时，模型拿到的是这次执行的失败（调用者 shell 的错误，或这一台电脑上的「没能运行」），然后继续。不要返回 `builtin_tool_blocked`。
3. **模型可见文字只描述这一台电脑。** 用现有工作说明的单一机器口吻，加上 `system_info`。不要写 “service host”，不要告诉模型去改用 `komputer_use`。Body MCP 只作为 `run_command` 底下的传输。若 hook 仍须拒绝浏览器一类无法转到调用者的工具，理由只说这个工具在这台电脑上不可用，不提到另一台机器。
4. **harness 同一条工具。** 调用者的 harness 执行 agy 的 `run_command`，把 Windows 结果交回。这一步不要求模型改口去调 `k_*`。
5. **然后才是 agy 的文件工具**（`view_file`、`read_file`、`write_file`、`edit_file` 以及已登录的对应名字）。仍是远程大脑：读写发生在调用者的 Windows 文件上，模型看到的是那些路径。不要用 POSIX FUSE 挂载充当 Windows 方案，也不要在这一步打开「部分工具留在大脑上」的模式。
6. **Muse 和 Claude 的内置工具转发等第 1 步验收通过后再开。** 在那之前不要改 `KOMPUTER_BACKENDS` 的默认值。

半代理 / 混合模式（MCP 桩继续当模型的主界面，或官方工具一部分留在大脑、一部分转到调用者，作为一种支持的模式）推迟，本文不展开。

## 验收标准

Linux 上跑官方 `agy`（不是替身）。调用者是 Windows，Body 或 harness 在那台电脑上。用一次真实回合核对，假 CLI 单测不能代替。

- 模型调用的工具名是 agy 的 `run_command`，不是 `mcp__komputer_use__run_command`，也不是 `k_*`。
- 一条能暴露机器身份的命令（例如 PowerShell 的 `hostname`，或 `(Get-Location).Path`）返回这台 Windows 的主机名或 `C:\` 路径，退出码是这台机器上的退出码。
- PowerShell 5.1 的错误是普通文本，不是 CLIXML。表格行没有被补到列宽。输出里的 CRLF 按这台机器保留。
- 回合完成。没有 `builtin_tool_blocked`。hook 不因为「这是内置工具」而拒绝 `run_command`。
- 这一回合里模型能看见的说明、工具结果和拒绝理由（如果有别的工具被拒绝）不出现 “service host”，也不说有两台机器。
- 若模型调用 `system_info`，结果是 `platform: "win32"`、这台机器的 shell 列表和 `C:\` 根。不另加一段文字教它忽略 Linux 那一行。
- 同一构建上，不设置 `KOMPUTER_BACKENDS`（即默认 `claude`）时，`test/claude-regression.test.mjs` 仍与固定的 Claude argv、stdin 和子进程环境一致。

通过的判据是调用者 Windows 上的副作用和返回字节，不是模型自称它在哪台机器上。
