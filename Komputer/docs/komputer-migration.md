# komputer 命名与目录迁移

产品仍叫 **Klaud Kode by Kosmolopic**。内部命名统一为 **komputer**，本次为 0.8.0。
这是强制迁移：不保留此前名称的环境变量、模型、请求头或 JSON 别名。

## 工程位置

完整工程位于仓库的 `Komputer/` 子目录：

```text
Komputer/
  komputer-api/       客户端入口与测试
  komputer-session/   MCP 配置模板
  komputer/           隧道脚本
  klaud-kode-body/     工作站执行器
  deploy/            部署脚本
  docs/              设计与验证说明
  README.md
  SETUP.md
```

工作区外层由用户保留原路径；Git 历史、外层插件索引和依赖缓存没有改写，也不属于此目录的交付内容。
本次没有替用户修改远端主机或重启现有服务。

## 新接口与配置

| 类型 | 现在使用 |
| --- | --- |
| 服务环境变量 | `KOMPUTER_*`，详见 `komputer-api/env.example` |
| 模型 | `komputer-claude`、`komputer-opus`、`komputer-sonnet`、`komputer-haiku`、`komputer-fable` |
| 通用会话 ID | `X-Conversation-Id`、`X-Chat-Id`、`conversation_id`、`chat_id` 等不带品牌的字段仍有效 |
| 项目专用请求头 | `X-Komputer-Session`、`X-Komputer-Ephemeral` 等 |
| 项目专用 JSON | `komputer_session`、`komputer_conversation_id`、`komputer_ephemeral` |
| 元数据 | `owned_by: komputer`、health 服务名 `komputer-api` |
| MCP 服务名 | `komputer_use`，保持不变 |
| 工作站执行器 | 目录/命令 `klaud-kode-body`，默认配置 `~/.klaud-kode-body/config.json` |
| 执行器环境变量 | `KLAUD_KODE_BODY_CONFIG`、`KLAUD_KODE_BODY_TOKEN` |
| 运行目录 | 默认 `~/.komputer/workspaces/<实例摘要>`；可显式设置 `KOMPUTER_CLAUDE_CWD` |

`BODY_SSH`、Body 的配置字段、官方 CLI 自身的环境变量与标准 OpenAI/Anthropic 协议字段不改名。
专用字段升级不意味着更改这些第三方约定。

执行器的此前默认配置目录不会自动搬迁，也不读取此前环境变量别名。请安全迁移原有配置到新的
`.klaud-kode-body/config.json`，或用 `KLAUD_KODE_BODY_CONFIG` 显式指定确认过的配置文件；
同时更新启动命令、任务路径和 token 环境变量。没有迁移凭据时服务会因缺少 token 拒绝启动。
本次未移动用户目录中的真实配置，也未重启已部署执行器。

## 已有部署的一次性迁移

1. 先在现有部署方式下停止 API，确认没有正在执行的回合。不要同时开两份并共享 sessions 文件。
2. 私下备份 `env`、`KEYS.txt`、`sessions.json`、真实 `.mcp.json`、Body 配置和现有服务启动配置；
   备份放在代码目录外，不要提交凭据或会话历史。
3. 将代码更新到新布局。把保存的密钥、会话映射与 MCP 配置放入新目录；环境变量按新示例逐项迁移，
   不要直接照用此前变量名。配置内容中的路径也要改，不能只改文件夹名。
4. 修改客户端的模型和项目专用字段。采用通用 conversation/chat ID 字段的客户端可继续使用同一 ID。
5. 更新 systemd、计划任务或启动脚本的路径、工作目录与环境文件路径。
   `KOMPUTER_REMOTE_DIR` 应为远端仓库中的 `Komputer/komputer-api`；`deploy/target` 只保存目标配置，
   改它不会自动迁移远端目录。配置文件格式还须符合所用服务管理器的语法。
6. 默认 CLI cwd 现在独立于工程目录。路径变化可能使旧 Claude 转录无法直接续接；
   服务只在明确缺失且尚未产生输出/工具活动时重建一次。完整上下文依赖客户端传来的历史，
   不会自动复制、修改或删除 CLI 私有转录。
7. 先在独立测试实例验证工作站文件/命令、会话续接、流式回复和异常恢复，再切换正式服务。

正常部署与回滚脚本现在支持 Git 仓库子目录，会在 reset 前拒绝缺少新目录结构的提交。
若必须回到迁移前版本，应停服后按备份恢复整套代码与配置，不使用当前脚本跨布局回滚。

## 小岛约束的本次改进

- 当前工程源码、相对路径、模板和测试不含此前内部名称；新增自动回归防止它再次出现。
- 默认独立 cwd 不包含工程名称，Git 发现也在父目录前止步。
- 新会话通过 `system_info` 了解工作站环境，命令与路径以该环境为准。
- 继续保留通用 shell、文件、进程与嵌套 MCP；不会为了表面看起来原生而把操作放回执行主机。
- 这是最大努力的上下文约束，不是操作系统沙箱。完整 CLI 默认提示、用户主动提供的文件与外部内容，
  不能仅靠命名迁移完全隔绝；原生工具透明转发的后续方向见 [工具重定向评估](tool-redirection.md)。

## 验证

本次在新的工程目录中验证：API **59/59**、Body **21/21** 全部通过。
自有工程文件（含本机私有部署目标配置）的旧名称文本与相对路径扫描均为 **0**；
第三方依赖、Git 历史和用户明确保留的外层工作区不计入此扫描。
6 个 shell 脚本的 Git 可执行位保持 `100755`；它们已单独暂存，以免 Windows 下重新添加时丢失执行属性。
这些记录属于命名迁移阶段；0.9.0 进一步实现了 harness 工具协议与公网 HTTPS，未部署到实际主机。

运行 `cd Komputer/komputer-api && npm test` 以及 `cd Komputer/klaud-kode-body && npm test`。
覆盖：原有行为、命名零残留、对外元数据、独立默认 cwd、Git Bash 语法、跨布局部署/回滚的拒绝路径。
Body 命名调整另覆盖配置变量、token 覆盖、命令环境中的凭据移除，以及 npm 命令入口和锁文件一致性；
打包 dry-run 通过，没有发布包。日常入口确定为本地 harness 经 API 接入，SSH 直连仅作备选。
0.9.0 的工具支持与边界见 [harness 接口](harness-api.md)，公网能力见 [公网 API](public-api.md)。
新增 API 依赖后需运行 `npm ci`；具体 harness 与真实 Claude 联调仍留到安装阶段。
部署测试只使用临时本地 Git 仓库，不连接真实远端；真实 Claude、SSH、Tailscale 和 systemd 仍需实机验收。
