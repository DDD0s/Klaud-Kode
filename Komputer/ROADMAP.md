# 还想改什么

项目的两个目标没变：操作尽量接近本地原生体验，模型尽量只接触要操作的那台电脑。
下面是现在需要继续做的事情，不是已经排好日期的发布计划。

## 先把实际使用跑顺

- 找具体的本地 harness 联调，记录版本、配置和不能用的功能。优先确认工具权限、长任务、断线重试和取消。
- 做本地和云端安装流程，减少手动配置。现在只有安装文档和已有环境的更新脚本。
- 完成真实公网 HTTPS、官方 Claude Code 和本地工具的整套测试。目前接口 CI 使用替身 CLI。
- 用登录后的 agy 和 muse 跑完整回合，核对 [CLI 后端](docs/backends.md) 里标为假设的部分：
  工具步骤字段、MCP 工具调用和权限、用量字段。拿到 agy 的真实输出样例后放进 `test/fixtures/`。
- agy 和 Muse 沿用 Claude 的设计（自己的工具关闭，用户电脑经 MCP 到达，环境写在说明里），agy 已用真实程序跑通 harness 和 Body 回合。
  还要在已登录的 agy 和真实模型的 Muse 上确认工具调用，以及 Windows 用户电脑上模型是否按 `system_info` 写 PowerShell。
- agy 内置工具经 bash 替身和 FUSE 转到用户电脑（`KOMPUTER_AGY_REMOTE_TOOLS`）现在是默认关闭的实验功能，只支持 POSIX Body。
  如果共用设计在已登录的 agy 上足够好，可以删掉它（`agy-bash-shim.mjs`、`komputer-fs.py` 和 hook 的 remote 策略）。
- agy 每次调用要 11–17 秒、约 6.6k token 的基础提示。可以研究为每个对话保留一个通过 stdin 多轮的 agy 进程，
  但要先解决空闲进程的内存（约 240MB）和超时回收。
- Responses 的回复存储只按数量淘汰，还没有按时间清理。
- 改进进程管理。现有启动脚本使用 PID 文件，没有附带 systemd 或其他服务管理配置。

## 本地操作还差什么

- GUI、浏览器和应用操作需要更多实际工具，也需要确认它们运行在哪个用户会话中。
- 错误输出、编码、路径和长进程行为继续按真实电脑验证，不能只在协议上返回成功。
- 多台本地电脑之间的选择和权限还没有设计好。切换电脑时，工具和历史不能混用。

## 隔离和恢复

- Claude Code 的提示、用户配置、memory 和 skills 仍可能暴露云端环境，需要继续检查。
- harness 的未完成工具状态还在内存里。研究恢复前，先明确怎么避免重复执行已经完成的操作。
- Body 的文件根目录不限制 shell，也没有逐次审批。要增加权限控制，需要明确它与本地客户端的关系。
- 会话映射和 Claude 转录缺少自动清理策略，不能直接按空闲时间删掉用户还需要的记录。

## 已有的基础

- API 0.10.0：按模型名路由到 Claude Code、Antigravity 或 Muse Code，每个回合启动真实 CLI；
  新增 Responses API（流式、函数工具、`previous_response_id` 续接 CLI 会话、推理强度）。
  Claude 的启动参数、stdin 和子进程环境有逐字节回归测试；agy 和 muse 用替身 CLI 及真实输出记录测试。
- API 0.9.0：Chat Completions / Messages 的客户端工具调用、结果回传、SSE、重试、取消和参数校验。
  等待本地工具时释放计算名额，允许本地子任务继续请求 API。
- 公网接口：直连 HTTPS、反向代理后端配置、强 key 检查、错误鉴权限流和指定来源的 CORS。
- Body 1.3.0：文件和命令工具、跨回合进程（可设空闲租约）、本机 MCP 转发；Windows 上的 cmd、Windows PowerShell、
  pwsh、Git Bash 和 WSL，PowerShell 包装带 UTF-8 输出和 bash 的退出码规则；输出编码回退、截断和信号终止状态有回归测试。
- 工程统一放在 `Komputer/`，内部名称统一为 `komputer`，Claude 工作目录默认在仓库外。
- Linux CI 覆盖 Node.js 22 和 24，包括严格证书验证的 HTTPS 测试。

安装从 [SETUP.md](SETUP.md) 开始，旧配置迁移见 [迁移说明](docs/komputer-migration.md)。
具体问题和方案可以在 [Issues](https://github.com/DDD0s/Kloud-Kode/issues) 讨论。
