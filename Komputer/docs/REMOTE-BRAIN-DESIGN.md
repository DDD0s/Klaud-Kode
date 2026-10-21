# 远程大脑设计（brain-in-a-vat / island mode）

扫描基线：`main` `7cb0323`（PR #1 已合并）。API `kloud-kode-api` 0.10.0，工作站 `kloud-kode-body` 1.3.0。
本文只记录目标、现状，以及远程大脑后续工作应遵守的判据。不改代码。

`docs/FEATURES-inventory.md` 写于合并前的 `90753b7`，记录的是当时已经落地的设计：关掉官方内置工具，改走 MCP。它不是远程大脑完成与否的判据。本文用一条更高的判据回看这些做法：云端 CLI 里的模型必须认为自己正运行在调用者的本地电脑上。工具叫什么名字、由哪个后端承载，都排在这条判据之后。

## 目标

### 最高约束：模型必须认为自己就在本地电脑上

云端 CLI 运行时，模型必须认为自己正运行在调用者的本地电脑上。这是远程大脑的定义，不是体验修饰，优先级高于选哪个 CLI、工具叫什么名字、走 harness 还是 Body。

因此，模型能看见和使用的操作系统、shell、工作目录、根目录、文件、进程、时区、命令输出、错误与退出码，必须共同指向调用者那台电脑（常常是 Windows），而且这些操作必须真的发生在那台电脑上。不能一边让工具在本地执行，一边用大脑主机的工作区、home、工具说明、拒绝理由或副作用告诉模型还有另一台机器；也不能只改提示里的 OS 字样，冒充执行位置已经改变。验收要核对调用者机器上的副作用、返回字节，以及全部模型可见输入和输出是否只组成这一台电脑的现实，不能只听模型自称它在哪里。

官方 CLI 是可替换的大脑，可以跑在 Linux 或其他大脑主机上。Claude 是本项目最初研究和实现的样本，不是产品身份。使用 `agy` 时，要的也只是它的模型与会话引擎，不是 Antigravity 的客户端和本地工具层；那一层必须关掉、替换或转到调用者机器。其他 CLI 同理。

本地入口有两条同等重要的链路：harness 负责客户端自己的界面、权限和工具时，结果由 harness 在调用者电脑上产生；客户端只聊天时，由 Body 在调用者电脑上执行。两者是到达同一台电脑的两种执行接法，不是主线与备选。

模型的双手按后端机制选择：官方内置工具透明转发、harness 自己的工具、Body 的 MCP 工具都可以。透明转发有价值，是因为它保留模型已经熟悉的工具语义；MCP 也可以只在底下传输，或作为模型可见的本地能力呈现。关键不是工具名，而是模型不需要理解远端与本地的分离，工具就表现为这台电脑本身的能力，并且权限、取消、长进程、路径、shell、输出和副作用都按这台电脑工作。不能在调用者机器上完成的操作，不要包装成本地能力。

Island mode 是这台机器的口吻：模型可见的文字不出现两台机器，也不出现 “service host”。现实由 `system_info` 以及 Body / harness 的真实执行呈现：操作系统、shell、时区、根目录、退出码、输出。

实现可以按后端和工具能力分阶段，但判据不降级。阶段性混合实现必须如实标注哪些能力已在调用者机器上、哪些只是大脑内部或网络动作、哪些还不存在；不得靠关掉工具后让回合失败来冒充远程大脑已经完成。

默认 `KOMPUTER_BACKENDS=claude` 的纯 Claude 通路在各自验收完成前保持今天的行为。见下一节。

## 非目标

- 不把任何一家 CLI、任何一种工具名定为唯一正统。后端和实现顺序按实际日常使用组合与机制可行性决定，不按厂商偏好，也不只按哪个最容易拦截决定。
- 不接受“标成本地能力、实际在大脑主机执行”的混合状态。分阶段实现可以存在，但每项能力的真实执行位置必须说清，不能用阶段性实现冒充最高约束已经满足。
- 任一后端在通过自己的远程大脑验收前，不改变它的既有默认行为。
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
- 工作说明的注释要求只描述一台电脑，不提启动器、第二台机器或本地/远端（`server.mjs` 里 `DEFAULT_WORK_NOTE` 上方）。说明正文把这组 MCP 工具写成到达 shell 和文件的方式。

这条默认通路在它通过自己的远程大脑验收前保持这样。打开 `antigravity` 或 `muse` 才进入下面的多后端行为。

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

1. **模型看见的还不是一台完整的本地电脑。** 三个后端今天分别用关掉、拒绝或限制 CLI 自带工具，再经 MCP 到达本地执行；执行位置可以正确，但 CLI 注入的大脑环境、脑端工作区、工具呈现和拒绝理由仍会同时出现。问题不在于模型调用的是官方工具名还是 MCP 名字，而在于这些输入没有共同组成调用者这一台电脑。透明转发、harness 工具和 Body 工具都要按这个标准补齐，而不是任选其一就算完成。
2. **Windows 调用者没有内置 `run_command` 转发。** 大脑在 Linux、Body 报 `win32` 时，打开 `KOMPUTER_AGY_REMOTE_TOOLS` 也会退回 island。shim 不改写非 POSIX 根，远端 shell 默认 `bash`。没有 PowerShell 路径。若选择 agy 内置工具作为这条链的执行面，这项验收今天达不到；改用 harness/Body 的本地工具是另一条要单独把环境做完整的路线。
3. **agy 的 harness 回合还没有形成完整的本地工具面。** harness 本身是合格的本地执行者，`k_*` 也可以只是本次调用的内部映射；缺口在于 agy 自己的 `run_command` 等工具会被 island 拒绝，模型随后需要在 `k_*` 与被拒绝的自带工具之间改道。它应当只面对一套连贯的本地能力：可以由 harness 原生工具呈现，也可以把选定的 CLI 内置工具转到 harness，但不能靠带两台机器含义的拒绝来拼凑。
4. **文件工具在 Windows 调用者上没有落点。** FUSE 挂载不能代表 `C:\`。撤回的那一版把 Windows 根映射到挂载下的 `c/Users/...`，当前树里没有。
5. **各后端让内置能力落到调用者的办法都还不完整。** Muse 只有 `--disable-shell --disable-write --disable-web-tools`，没有把自己的 shell/写入转到调用者；Claude 只有 `--tools ""`，没有把 Bash/Read/Write 转到调用者。今天二者都靠 MCP 到达本地。今后每个后端按同一条最高约束逐一验收，顺序不在本文预先绑定为某一家先行。
6. **模型看见的工作区仍是大脑的。** 默认工作目录是 `~/.komputer/workspaces/<摘要>`。CLI 的系统提示仍写 Linux。没有一层把模型要用的路径显示成调用者的 `C:\`，同时让读写落在那台机器上。
7. **懒加载工具说明把模型带进大脑的 home。** 要读 `~/.gemini/antigravity-cli/mcp/...` 才知道 `k_0_…` 是什么。远程大脑不把这条读盘当成正常路径。
8. **Responses 没有改变执行位置。** function 工具进入现有 harness；不带工具的请求进入现有 Body。

## 冲突

这些是现状里和目标对着干的行为。下一步要拆掉它们在远程大脑路径上的作用，而不是再加一套。

- **拒绝文案告诉模型有两台机器。** `agy-hook.mjs` 在 island 策略下的理由是：内置工具会作用在 “service host” 上，让模型改用提供的工具。remote 策略下，浏览器类工具、挂载外的路径，理由同样写出 “service host” 和 “the user's computer”。这些字符串会进模型。Island mode 不允许这种口吻。
- **内置工具会结束回合。** `KOMPUTER_AGY_BUILTIN_GUARD=stop` 杀掉 agy 并返回 `builtin_tool_blocked`。默认 `warn` 只打日志，但 `stop` 已实现并有测试。已登录且 `always-proceed` 时，拒绝发生之前命令可能已经在大脑上跑完。目标是：内置工具的调用得到调用者的结果，回合不因此结束。
- **用 MCP 工具本身不是冲突，呈现方式才是。** `DEFAULT_WORK_NOTE` 写明用 `mcp__komputer_use__*` 到达 shell 和文件。若这些工具被呈现并实际表现为这台电脑自己的能力，它可以是模型的双手；若说明、工具名、schema 读取或错误让模型先理解一套远端拓扑，再去遥控另一台电脑，就违反最高约束。`system_info` 应只报告这台电脑的现实。
- **现有文档的完成标准比最高约束低。** `docs/backends.md` 的「与 Claude 共用的设计」、`docs/tool-redirection.md` 末节、`env.example` 里对 `KOMPUTER_AGY_REMOTE_TOOLS` 的注释、`FEATURES-inventory.md` 第 15 节，主要以“不在大脑执行、改经 MCP 到本地”判定隔离成立，并把内置工具转发写成可删的 POSIX 实验。它们准确描述今天的默认，但没有要求模型所见的工作区、home、shell、路径和拒绝理由也只组成调用者的电脑；不能拿它们替代本文的验收标准。
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
- **CLI 自己的 “OS is linux” 一行目前去不掉。** 不要用第二段文字向模型解释「忽略另一台主机」。工作说明只描述调用者这一台，并让它用 `system_info` 的结果下命令、写路径。但只要这行仍进入模型上下文，最高约束就只能算部分满足，必须把它列为明确的环境泄漏，不能用本地执行成功把它勾销。完全替换系统提示的 `KOMPUTER_SYSTEM_PROMPT_FILE` 只有 Claude 有，仍是实验。

## 推荐下一步（先让一台电脑的现实完整）

不要按厂商列队，也不要只按哪个 CLI 最容易拦截来排序。先选一条实际日常使用的大脑与本地入口组合，把它做到模型只面对调用者这一台电脑；然后再按实际使用和机制可行性逐个后端推进。选择 agy 时，它只是云端的大脑，不是 Antigravity 客户端或本地工具层的替代品；选择 Claude 或 Muse 时同样如此。未通过验收的后端保留现有默认行为，不提前改 `KOMPUTER_BACKENDS` 的默认值，Claude 的既有回归锁继续保持。

1. **先定环境合同。** 从 `system_info` 或 harness 自己的环境信息取得调用者电脑的 OS、shell、工作根、当前目录、时区和编码。模型实际使用的 cwd 与路径要是这台电脑的路径；大脑工作区到调用者路径的改写留实现内部。CLI 自行注入的大脑 OS/cwd 是明确泄漏，要寻找受支持的消除或中和方式并单独验收，不靠追加一段“忽略另一台机器”的文字。
2. **再统一执行面。** harness 与 Body 两条链路都算数。每个后端可选官方内置工具透明转发、harness 自己的本地工具或 Body 工具，但模型面对的应是一套连贯能力：shell、文件与进程都在调用者电脑上，权限、取消、超时和退出码按那台电脑工作。若选 agy 的内置 `run_command`，一条可行路径是经现有 shim 交给 Windows 上的 PowerShell（有 `pwsh` 用 `pwsh`，否则 5.1）；若选 harness 工具，就让 harness 的本地 shell 工具成为这条链的执行面，不必强迫它冒充 `run_command`。做到哪一种，就按哪一种验收并如实命名。
3. **回合不能因工具归属而结束。** 已纳入本地执行面的调用，失败时把调用者 shell 或工具的真实失败交回模型继续处理，不用会杀掉 CLI 的 guard 代替一次本地执行。无法在调用者电脑完成、且仍会暴露大脑的能力不向模型呈现为本地工具。若不得不拒绝，理由只说该能力在这台电脑上不可用，不写 “service host”，不解释拓扑。
4. **补齐文件和进程的一致性。** 读写、编辑、搜索、长进程与 stdin 都要在调用者电脑上发生，模型看到调用者的路径。不要用 POSIX FUSE 挂载充当 Windows 文件方案；内部映射不能改变 CRLF、退出码或路径语义。
5. **逐个后端验收，不预设先后。** 每接一个大脑，都用同一套最高约束复核它注入的系统提示、工作目录、home/schema 读取、工具列表和错误文案。机制上已有的 agy shim、hook 与挂载可以复用或借鉴，但不因 agy 先被量过就把产品顺序锁死在 agy。

分阶段或混合实现不是原罪；把仍在大脑上发生、或让模型看见大脑环境的部分说成已完成，才是问题。

## 验收标准

按选定的真实日常组合验收：正式官方 CLI（不是替身）跑在大脑主机上，调用者是 Windows，本地 harness 或 Body 在那台电脑上。用真实回合核对，假 CLI 单测不能代替。工具呈现方式在验收记录中写明，分别判断“执行已在本地”和“模型只面对本地电脑”两件事。

- 一条能暴露机器身份的命令（例如 `hostname`、`(Get-Location).Path`）返回这台 Windows 的结果；模型随后自然地按这台机器的 shell 与 `C:\` 路径继续工作，不需要被告知去忽略另一台主机。
- 文件新建/编辑和一个长进程的副作用只出现在调用者电脑上；检查大脑工作区没有对应文件或进程树。模型收到的路径是调用者的路径，而不是大脑工作区路径冒充本地路径。
- shell 的 stdout、stderr、退出码与 CRLF 和直接在调用者电脑上运行一致。Windows PowerShell 5.1 的错误是普通文本而非 CLIXML，表格不补列宽空格。
- 回合不会因为模型使用了已纳入执行面的本地能力而被 `builtin_tool_blocked` 一类机制终止；真实的命令失败按本地失败返回并可继续。
- 模型能看见的说明、工具说明、工具结果和拒绝理由不出现 “service host”，不解释本地/远端、大脑/身体或隧道。若选定的是透明转发，模型调用的就是该 CLI 的自带工具名；若选定的是 harness/Body 工具，验收记录不把它写成透明转发。
- 模型不需要进入大脑的 home、全局配置或 schema 目录，才能知道这台电脑有哪些能力。若某 CLI 的这条读盘仍无法避免，记为未通过最高约束的缺口。
- CLI 自行注入的大脑 OS、cwd、home、memory、skills 或用户配置若仍能进入模型上下文，逐项列为环境泄漏并判为部分通过；不能用“命令确实在本地跑了”抵销。完整通过要求模型可见环境与实际副作用都只指向调用者的电脑。
- `system_info`（或 harness 的等价环境信息）报告的是调用者电脑；若命令实际使用的 shell 与其中 `defaultShell` 不同，模型可见说明必须与实际执行面一致，只描述这一台电脑。
- 同一构建上，未改默认后端的既有通路保持兼容；默认仍为 `claude` 时，`test/claude-regression.test.mjs` 继续与固定的 Claude argv、stdin 和子进程环境一致。

通过的判据始终是调用者电脑上的副作用、返回字节以及模型可见上下文的一致性，不是模型自称它在哪台机器上。
