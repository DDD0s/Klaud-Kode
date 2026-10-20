# 开发说明

项目介绍见 [仓库首页](../README.md)，安装见 [SETUP.md](SETUP.md)。这里写代码结构、测试和更新方式。

## 先记住两件事

一是尽量接近在本地使用电脑：文件、命令和应用操作应当真的在目标电脑上执行，
路径、shell、退出码和输出都要按那台电脑的实际情况返回。

二是尽量减少云端环境混进模型上下文。默认关闭云端内置工具，使用独立工作目录，
筛选子进程环境。不能因为做不到完全隔离就放弃改进，也不能把这些措施当成操作系统沙箱。

日常入口是本地 harness。远端运行官方 Claude Code 和 API，不要求再装聊天客户端。
SSH 直连 CLI 是可选用法，后台工具隧道则是另外一回事。

## 代码在哪

| 文件或目录 | 内容 |
| --- | --- |
| `komputer-api/server.mjs` | HTTP 接口、CLI 进程、Body 会话和流式回复 |
| `komputer-api/client-tools.mjs` | 客户端工具定义、MCP 转接、结果续接和重试状态 |
| `komputer-api/public-http.mjs` | TLS、CORS 和错误 key 限流 |
| `komputer-api/backends/` | 各 CLI 后端：参数、stdin、输出解析、错误分类和按模型名路由 |
| `komputer-api/responses.mjs` | Responses API 的请求转换、事件流和回复存储 |
| `komputer-api/claude-env.mjs` | 各 CLI 子进程的环境变量名单 |
| `kloud-kode-body/server.mjs` | 本地文件、命令、进程和其他 MCP 服务 |
| `komputer/tunnel-up.sh` | Body 模式的 SSH 转发 |
| `komputer-session/` | Body 模式的配置模板 |
| `deploy/` | 更新已有远端安装的脚本 |

请求带客户端工具时，`client-tools.mjs` 建立临时 MCP 服务，把调用交回本地 harness。
等待工具结果期间保留 CLI 进程，但释放计算名额；结果回传后重新排队继续。
不带客户端工具时走 Body，会话 ID 保存在 `sessions.json`，后续用 `--resume`。
两种模式的状态和恢复方式不同，改动前看 [工具协议](docs/harness-api.md)。

## 改动时注意

- 产品名是 Kloud Kode by Kosmolopic，内部名称是 `komputer`，本地执行器是 `kloud-kode-body`。
  官方 Claude Code 和 `claude` 命令保持原名。不要恢复已移除的旧名称或别名。
- Claude 的正常工作目录在仓库外。不要为了省配置把 cwd 改回工程目录。
- API 调用官方 CLI，不自行读取、复制或转发它的登录文件，也不实现账号池。
  新后端同样必须启动真实 CLI，不模拟它的协议，也不伪造请求头。
- Claude 的参数、stdin 和环境由 `test/claude-regression.test.mjs` 固定。改动后端公共代码时不要改这个测试来迁就结果。
- 新后端要有替身 CLI 测试；能拿到真实 CLI 时，把它的输出记录放进 `test/fixtures/`，并在 [CLI 后端](docs/backends.md) 写明哪些已验证、哪些是假设。
- 这是单用户服务。新增 key 不会创建独立用户或独立会话空间，不要在文档里暗示多用户隔离。
- 当前 CLI 跳过交互式权限确认。更改权限行为要单独说明，不能把本地 harness 的确认设置当成 Body 的权限。
- 公网默认要求 TLS 和强随机 key。不要关闭证书验证或放宽测试来掩盖网络问题。
- 不提交实际的主机名、地址、密钥、证书、部署目标或带 token 的 MCP 配置。
  配置模板可以跟踪，真实配置放到仓库外或已有的忽略路径。
- 不把客户端错误详情直接透传出去。保留 HTTP 状态和错误类型，敏感诊断留在服务端并脱敏。

## 运行测试

在仓库的 `Komputer/` 目录，用 Bash 执行：

```bash
(cd komputer-api && npm ci --ignore-scripts && npm test)
(cd kloud-kode-body && npm ci --ignore-scripts && npm test)
```

PowerShell 下分别进入两个目录，运行相同的 npm 命令。
API 测试使用替身 Claude、agy 和 muse CLI，但 HTTP、MCP、HTTPS 和进程往返是真实的。
本机装有真实 agy 或 muse 时，可设置 `KOMPUTER_TEST_REAL_AGY` / `KOMPUTER_TEST_REAL_MUSE` 为其路径跑冒烟测试，默认跳过。
Body 测试会启动本机服务和测试命令。不需要真实 Claude 登录，不消耗模型额度。

GitHub Actions 在 Linux 的 Node.js 22、24 上运行完整测试。
本机 HTTPS 检查可能受安全软件替换证书影响，要记录实际失败原因，不能把它报成通过。
代码中的路径和名称改动还要通过 `test/naming.test.mjs`；文档改动至少检查命令、链接和命名。

自动测试不代替真实客户端联调。测试真实环境时，用单独端口、key、会话文件和测试目录，
不要复用正在工作的那套服务。

## 更新已有安装

`deploy/deploy.sh` 会运行测试，让远端拉取 `origin/main`，等待正在执行的回合结束，
然后更新代码、重启并检查服务；失败时尝试回滚。

它会改动远端安装，不是一次只读检查，也不是从空服务器开始的安装器。
先阅读 [目标配置示例](deploy/target.example) 和脚本，填写自己的目标，再考虑运行：

```bash
bash deploy/deploy.sh
```

`KOMPUTER_REMOTE_DIR` 应指向远端的 `Komputer/komputer-api`。
脚本不支持跨越旧目录结构的部署或回滚，旧安装先看 [迁移说明](docs/komputer-migration.md)。
文件使用 LF 换行，避免 Bash 脚本在 Linux 上因 CRLF 失败。

## 提交问题或改动

报告问题时写清楚使用的是 harness 还是 Body，附上复现步骤和删去密钥后的日志。
小修复可以直接提 PR。涉及协议、权限、状态恢复或架构的改动，先开 Issue 说明要解决的实际问题。
测试结果区分自动测试、真实联调和未验证项，不把模型回复当作操作成功的证据。
