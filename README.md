# Kloud Kode

CAPTURED
KERNEL SENSOR:
EMURATED...

## 怎么用

日常入口是本地的 agent 客户端，也就是下文说的 harness。它负责聊天界面、工具执行和本地权限确认。
远端只需要官方 Claude Code 和这里的 API 服务，不用再装一套聊天软件。

```text
本地客户端 ── 消息 ──> Komputer API ──> 远端 Claude Code
           <─ 工具调用 ────────────────┘
本地执行   ── 结果 ──────────────────> 继续回答
```

工具有两种接法：

- 客户端能执行工具：通过 API 收到调用，在本地执行后回传结果。不需要 Body，也不用开 SSH 隧道。
- 客户端只负责聊天：在要操作的电脑上运行 `kloud-kode-body`，让远端 Claude Code 通过 MCP 调用它。
  这条路需要私网或 SSH 隧道。

从 [安装步骤](Komputer/SETUP.md) 开始。公网接入看 [HTTPS 配置](Komputer/docs/public-api.md)。
本地客户端和云端的一键安装还没做，目前需要手动配置。

## 做到哪了

API 和 Body 已经用 Go 重写成单个二进制 `kloud-kode`（`api` / `body` / `bridge` / `hook` / `shim` 子命令），部署默认运行它；
仓库里保留的 JavaScript 实现可以用 `KOMPUTER_IMPL=node` 回退，两种实现共用同一份配置和数据文件。

API 已支持 Chat Completions、Messages 和 Responses 三种请求格式，包括流式回复、客户端工具调用和结果回传。
除 Claude Code 外，还可以按模型名把请求交给 Antigravity（`agy`）或 Muse Code（`muse`）。
每个回合都启动真实的官方 CLI，不模拟它们的协议，见 [CLI 后端](Komputer/docs/backends.md)。
Body 提供文件读写、搜索、命令执行、长进程管理，也能连接本机的其他 MCP 服务。

还需要继续解决的事情：

- 具体客户端的联调。接口测试通过不等于每个客户端都能直接用；只会聊天的客户端不会因此获得本地工具。
- 更接近原生的电脑操作。GUI、浏览器和各类应用需要相应的本地工具，目前没有自带一套完整的桌面控制。
- 云端环境泄露。内置工具默认关闭，但 Claude Code 自带的提示和配置仍可能带入云端信息。
- 重启后的任务恢复。harness 的未完成工具调用存在内存里，服务重启后不能原地续上。

CI 在 Linux 上运行 Go 测试（含 race）、在 Windows 上运行 Go 测试，另外跑 Node.js 22 和 24 上的 JavaScript 实现作为行为对照。API 测试用替身 CLI，
不消耗模型额度；公网域名、真实 Claude、登录后的 agy / muse 和具体 harness 的完整联调还没完成。

## 用之前看一下

这是单用户工具，没有用户之间的权限隔离。API key 和 Body token 都应当当作电脑的访问凭据保管，
公网 API 要用 HTTPS，Body 端口不要直接暴露到公网。

当前 API 启动 Claude 时会跳过 CLI 的交互式权限确认。
使用 harness 时，本地是否执行仍由 harness 决定；使用 Body 时，Body 没有逐次审批窗口。
Body 的 `roots` 只限制文件工具，不能把 shell 命令关在指定目录里。

## 想一起看看？

这个项目最初是为自己用的。现在公开，是想请大家帮忙看代码、找问题，或者试试还有什么更简单的做法。

遇到问题可以提 [Issue](https://github.com/DDD0s/Kloud-Kode/issues)，最好带上系统、Go 版本和 Claude Code 版本、
使用的客户端、复现步骤，以及删掉密钥后的日志。修小问题可以直接提 PR；改架构的话，先开个 Issue 聊聊。
安装文档里哪一步看不懂、照着做没成功，也值得报。

## 文档和代码

代码都在 [`Komputer/`](Komputer/README.md)，内部名称用 `komputer`。

- [安装步骤](Komputer/SETUP.md)
- [API 配置和会话](Komputer/komputer-api/README.md)
- [客户端工具协议](Komputer/docs/harness-api.md)
- [CLI 后端](Komputer/docs/backends.md)
- [Body 安装和工具](Komputer/kloud-kode-body/README.md)
- [开发说明](Komputer/DEVELOPING.md)
- [后续工作](Komputer/ROADMAP.md)
- [旧版本迁移](Komputer/docs/komputer-migration.md)
