# Klaud Kode

把远端服务器上的官方 AI 编程 CLI（Claude Code、Antigravity、Muse Code）转成标准 API，实现**云端跑模型、本地干活**。

## 它是干嘛的

官方 CLI 部署在远端服务器上时，默认只能操作远端服务器的文件。Kloud Kode 解决两个问题：

1. **转成标准接口**：调用真实的官方 CLI（非逆向协议），对外提供兼容 OpenAI（Chat Completions / Responses）和 Anthropic（Messages）的 API。
2. **打通本地环境**：让远端的 AI 能读写你本地电脑的文件、跑本地终端命令、调用本地 MCP。

```text
本地客户端 ── 聊天请求 ──> 远端 Kloud Kode API ──> 远端官方 CLI (Claude / agy / muse)
本地电脑   <────────────── 工具调用 / 执行结果 ────┘
```

## 两种接法

- **本地客户端自带工具（推荐）**：直接把客户端接入远端 API。远端下发工具调用，本地客户端执行后回传结果，无需额外部署组件或打隧道。
- **本地客户端只会纯聊天**：在本地电脑运行 `kloud-kode body`（提供文件读写、命令执行、进程管理和本地 MCP 桥接），通过私网或 SSH 隧道让远端 CLI 远程调用本地电脑。

## 特性与现状

- **单二进制部署**：Go 编写的单一可执行文件 `kloud-kode`，包含服务端（`api`）与本地端（`body`）。
- **多后端支持**：按模型名路由到官方 Claude Code、Antigravity（`agy`）或 Muse Code（`muse`）CLI。
- **待完善**：目前需手动配置；服务重启后未完成的工具调用不会自动恢复；暂无原生 GUI 桌面控制工具。

## 安全提醒

- **单用户设计**：无多用户隔离，请将 API Key 和 Body Token 视为电脑最高权限凭据。
- **权限确认**：远端启动 CLI 时默认跳过交互式确认。使用自带工具的客户端时，由本地客户端拦截审批；使用 `body` 模式时**无逐次审批弹窗**，切勿将 Body 端口直接暴露到公网。

## 文档导航

- [安装步骤](Komputer/SETUP.md) | [公网 HTTPS 配置](Komputer/docs/public-api.md)
- [API 配置与会话](Komputer/komputer-api/README.md) | [客户端工具协议](Komputer/docs/harness-api.md) | [CLI 后端说明](Komputer/docs/backends.md)
- [Body 安装与工具](Komputer/kloud-kode-body/README.md)
- [开发说明](Komputer/DEVELOPING.md) | [后续计划](Komputer/ROADMAP.md) | [旧版迁移](Komputer/docs/komputer-migration.md)
