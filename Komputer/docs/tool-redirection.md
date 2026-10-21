# 工具重定向评估（2026-09-26）

目标：Claude Code 的决策在 A，用户文件、命令和应用在 B 执行。改工具名字或提示里的 OS
不能改变实际执行位置。本次保留默认 `--tools ""` + `komputer_use`；没有上线原生工具转发原型。

## 各方案的边界

| 方案 | 能做什么 | 为什么还不等于全工具透明转发 |
| --- | --- | --- |
| shell 包装器 | 有机会把 shell 命令发往工作站 | 不覆盖原生 Read/Write/Edit/Glob/Grep；cwd、引号、取消、退出码、后台进程及 Windows shell 语义仍需适配 |
| PreToolUse | 修改输入或允许/阻断调用 | 修改参数不等于更换工具实现；拒绝理由是失败反馈，不是成功工具结果 |
| PostToolUse | 当前文档支持替换模型看到的输出 | 在工具执行之后才发生，不能撤销已在 A 发生的写文件/命令副作用 |
| 网络文件系统挂载 | 暴露 B 的文件 | 在 A 上跑搜索仍使用 A 的 CPU，且会传输文件内容；不符合计算也在 B 的目标 |
| 路径映射 | 把一套路径翻译为另一套 | 必须建立在真正的执行转发之上；不是转发机制本身 |
| 工作站 MCP | 已有 shell、文件、进程与嵌套 MCP 在 B 执行 | 工具名/协议不同于 CLI 原生工具；需要继续补齐具体应用能力 |

Hooks 的输入修改和输出替换语义来自 [官方 Hooks 参考](https://code.claude.com/docs/en/hooks#pretooluse-decision-control)
及 [PostToolUse 说明](https://code.claude.com/docs/en/hooks#posttooluse-decision-control)。这些是文档核查，
不代表已验证生产机器安装版本支持全部字段。

## 环境信息

`system_info` 现在提供工作站 OS 版本、shell、工作根目录、UTC 时间、本地日期、时区与偏移。
默认工作提示引导模型按需读取，没有新增匿名环境接口，也没有自动抓取或缓存 Claude 的内部提示模板。

完整 prompt 替换会丢掉默认指导；追加只增加说明。当前文档的
`--exclude-dynamic-system-prompt-sections` 是把机器信息移动到首条用户消息，不是隐藏或替换它。
因此不能把它当成“环境替换已实现”。见 [官方 CLI 参考](https://code.claude.com/docs/en/cli-reference#system-prompt-flags)。

当前 CLI 文档还描述了续接时的系统提示快照：是否立即采用新的 system 文本与版本及参数有关。
正式集成前应固定 CLI 版本，在独立会话验证 system 更新、Windows 文件路径、超时与恢复；
本仓库的 fake CLI 测试不能证明这些上游行为。

## 下一步建议

先保持实际执行位置正确，再做原生工具外观兼容。若要研究 shell 包装器，应在独立实例中
只开放 Bash，并把原生文件工具保持关闭。验收要分别检查 A/B 的副作用和进程，覆盖读写、搜索、
Unicode 路径、超时、断线、取消与长进程；不能只看模型声称自己在哪台机器。

若要求包括 GUI、浏览器和各应用交互在内的“本机能做什么就全都能做”，还需对应的工作站适配器。
当前通用 shell/文件/进程工具与嵌套 MCP 是基础，并不证明全部 GUI 能力已经实现。

## 多后端

agy 和 Muse 沿用这里的结论：CLI 自己的工具关闭，用户电脑经 `komputer_use`（Body）或客户端工具 MCP（harness）到达，
环境写在说明里。各后端的接法见 [CLI 后端](backends.md#与-claude-共用的设计)。agy 另有一个默认关闭的实验功能，
用 bash 替身、FUSE 挂载和 PreToolUse hook 把它的内置工具转到 POSIX 的 Body 上，相当于这里评估过的 shell 包装器加挂载；
它没有扩展到 Claude，也不支持 Windows。
