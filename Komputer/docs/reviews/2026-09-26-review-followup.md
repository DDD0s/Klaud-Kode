# Klaud Kode review 核对与修订（2026-09-26）

核对对象：`Klaud-Kode-debug-2026-09-25.md`。原文保留不动，本文件为修订结论。
代码基线：`8a75a39bbb01dd08b88f49845874a3d220b5942a`，API 0.7.0 / Body 1.2.0。
本次仅修改本地工作树；未执行原文中的推送、reset、部署、远端重启或权限设置。

## 总结

B1–B8 的核心问题成立，已修。用户进一步确认 A1 是强制要求，现已实施全面命名迁移，
不保留此前名称的兼容别名。A2–A4 仍需要按能力分阶段验证。
产品目标有两个：尽可能接近原生电脑体验，同时尽最大努力收紧“小岛”内可见的环境信息。
实际操作位置与上下文隔离都要验收，不将“无法百分之百”当作停止改进的理由。
API 是客户端到官方 CLI 的交互入口，本次不改变这一架构。

## 逐项裁决

| 原项 | 裁决 | 本次处理与证据 |
| --- | --- | --- |
| A1 全部改成 komputer | 用户确认的强制要求，已实施 | 工程移入 `Komputer/`；内部目录、KOMPUTER 环境变量、模型 ID、HTTP/JSON 标识、临时文件、日志与文档全部迁移；不保留此前命名别名 |
| A2 默认恢复全部原生工具 | 目前不适合 | 原生工具仍在 CLI 所在机器执行；保留关闭默认值及显式 `default` 选项，未弱化现有边界 |
| A3 替换整份默认提示的环境段 | 环境应以工作站为准；模板抓取方案尚无可靠实现依据 | 为 Body `system_info` 补 OS 版本和时间信息；保留可选完整 prompt，不自动抓模板，不承诺消除全部服务端环境信息 |
| A4 透明转发研究 | 值得研究，但 hooks/挂载不等于完整工具替换 | 见 [工具重定向评估](../tool-redirection.md)；未上线不完整原型 |
| B1 Git ceiling | 已复现并修复 | 临时父仓库中真实执行 `git rev-parse`：旧 cwd ceiling 可找到仓库，新 canonical cwd 的父目录 ceiling 阻止向上发现；不隐藏 cwd 自身的 `.git` |
| B2 环境过度过滤 | 成立并修复 | 恢复证书、Windows 登录路径、CLAUDE 配置/OAuth、MCP 超时、shell/terminal、遥测设置；新增精确名 `KOMPUTER_CHILD_ENV_EXTRA`，保留 wrapper 排除规则 |
| B2 API key 建议 | 需要兼容性修正 | 取消 `ANTHROPIC_*` 通配放行，但不突然删除已支持的 API_KEY/AUTH_TOKEN；发现已配置 API key 时警告计费来源。不会生成或代填 key |
| B3 小写代理 | 成立并修复 | 大小写代理归一化，大写优先；保留 ALL_PROXY 和显式空 NO_PROXY；不虚构默认代理。下游对 SOCKS/ALL_PROXY 的支持仍需实机核验 |
| B4 resume 丢失 | 缺失会话后的 502 已复现并修复 | 仅明确 missing-session 且无模型/工具活动时，用客户端历史重建一次；没有历史时明确缺失，不能恢复不存在的数据。换 cwd 是否导致丢失取决于真实 CLI 版本/存储 |
| B5 提示文件缺失 | 成立并修复 | 缺失、目录、不可读文件在启动失败；相对路径先转绝对路径。无效 cwd 也不再警告后继续运行 |
| B6 部分监听失败 | 成立并修复 | 任意绑定失败退出 1；真实占用端口回归覆盖第二监听失败。必须配合进程管理器重启；未声称已测试 Tailscale 开机时序 |
| B7 内部错误外泄 | 成立，范围不止 SSE | JSON、OpenAI SSE、Anthropic SSE 统一中性公开错误；状态和类型仍保留；服务端保留诊断并遮蔽常见 Bearer、URL 认证和 key 格式 |
| B8 掉线反复重建 | 成立并修复 | 失败默认缓存 30 秒，成功缓存仍 15 秒；保留 single-flight；503 的 Retry-After 与剩余退避匹配，可配置禁用失败缓存 |
| B9 health/token | 合理并已做 | 认证 health 增加缓存态 body_ok/时间/跳过标记；匿名不变。模板用 Authorization header，原 URL token 路由保留且原有真实 MCP 客户端测试通过 |
| B9 systemd / env | 部署建议，不是已确认仓库 bug | 仓库没有线上 service 文件，不能确认实际 EnvironmentFile/ExecStartPre。systemd 正确配置 EnvironmentFile 时，ExecStartPre 也可继承变量，不必靠脚本自行 source；未改线上单元 |
| B10 skills / 提示痕迹 | 不能从本地代码证实运维结论 | 没有读取远端 HOME 或核验所谓“已移走”。MCP 名称与默认提示仍可能可见；不能把“改名”当成隔离保证 |

后续命名迁移还将默认 Claude cwd 移到工程外的 `.komputer/workspaces/<实例摘要>`，
减少默认提示中的工程路径和父仓库信息；新会话提示要求先读工作站环境。
迁移步骤见 [komputer 迁移说明](../komputer-migration.md)。

`ANTHROPIC_API_KEY` 的优先级依据 [官方环境变量文档](https://code.claude.com/docs/en/env-vars)。
Header 形式依据 [官方 MCP 配置说明](https://code.claude.com/docs/en/mcp#environment-variable-expansion-in-mcp-json)。
`EnvironmentFile` 的含义参考 [systemd 官方执行环境文档源文件](https://github.com/systemd/systemd/blob/main/man/systemd.exec.xml)；
这不构成对实际线上 service 配置的验证。

## 本次补充发现并修复

1. **请求错误合并**：原 `turnKey` 对会话只取最后一句，并忽略独立 system。
   不同上下文可能接到同一回合。现在哈希完整消息、system 与工具模式；相同请求的断线续取行为仍保留。
2. **SSE 开场事件缺失**：心跳定时器先开 HTTP 流后，代码误以为已发过 assistant role/message_start。
   现在分别记录“流打开”和“协议消息开始”，两种协议均有延迟首包回归。
3. **恢复状态不一致**：失败的 reseed 必须清除 started；响应头按发送时的会话记录读取，
   不再预先捕获旧 UUID。新增 `X-Komputer-Session-Recovered` 提示上下文来源。
4. **相对提示路径错误**：API 能找到文件，不等于换 cwd 后 CLI 能找到。现在启动时规范成绝对路径。
5. **文档过度保证/旧默认值**：去掉“永远拿不到大脑证据”的保证，纠正代理默认值，写明 Git ceiling 的边界。

## 仍需明确的限制

- **permission 没有客户端同步。** 当前 `claudeArgs()` 仍固定 `--dangerously-skip-permissions`。
  本次没有改变已有权限默认值；Body 的 roots/能力开关与 OS 权限是另一层，尤其 roots 不是 shell 沙箱。
  若要同步 plan/bypass/逐次确认，需要单独定义端到端协议与界面，不是多转发一个字符串即可。
- 环境白名单不是安全沙箱；HOME/config 可加载 CLI 自身设置、插件、hooks 等。
  自定义额外变量的值会交给 CLI，应仅添加可信且必需的配置。
- 健康状态是有时效的缓存，不证明每个 MCP 工具可用；跳过/未查/过期时 body_ok 为 null。
- 日志是运维敏感资料；格式遮蔽并非通用 secret scanner，不能据此公开日志。
- 新 CLI 可能缓存 system prompt。实际版本的恢复、prompt 更新和跨 OS 行为仍需独立实例验收，
  不应根据 fake CLI 测试就宣称线上完成。
- 独立隧道 systemd 服务合理，但应另做部署变更，并验证重连、停止、配置继承和回滚；本次未执行。

## 验证记录

本机 Windows，Node v24.21.0。API 使用独立临时目录、假 Claude 与假 Body health，
Body 使用真实本机进程和 MCP SDK 客户端；没有消耗真实模型额度，也没有访问生产主机。

- 修改前：API **32/32**，Body **20/20**。
- 新故障回归在旧实现上：**11 失败 / 3 通过**，包括真实 Git 与端口占用，不是只断言变量名。
- 修改后新增/扩展回归：环境单测与可靠性测试 **21/21**；Body **20/20**。
- 命名迁移前完整 API 回归：**53/53**；迁移后的验证另记录在迁移说明中。
- `git diff --check` 与三个运行时 JS 文件的 `node --check` 通过。

未验证：真实 Claude 登录/计费、Linux/macOS 运行、真实 SSH 重建、生产 Tailscale/systemd、
默认系统提示内容或原生工具跨机转发。不要把本地通过等同于已部署。
