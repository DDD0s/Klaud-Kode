# Komputer

这里是 Klaud Kode 的代码目录。项目介绍在 [仓库首页](../README.md)，第一次使用看 [安装步骤](SETUP.md)。

## 目录

| 目录 | 做什么 | 运行位置 |
| --- | --- | --- |
| `komputer-api/` | 接收本地客户端的请求，调用官方 Claude Code（可选 Antigravity、Muse Code） | 远端主机 |
| `klaud-kode-body/` | 文件、命令、进程和本机 MCP 工具 | 要操作的电脑；客户端自带工具时不必安装 |
| `komputer/` | Body 用的 SSH 隧道脚本 | 远端主机；仅 Body 模式需要 |
| `komputer-session/` | Body 模式的 MCP 配置模板 | 复制到仓库外再填写 token |
| `deploy/` | 更新已经配置好的远端安装 | 管理这套服务的电脑 |
| `docs/` | 接口说明、迁移记录和设计讨论 | — |

`deploy/` 不是首次安装器。使用前要有能工作的远端安装，并检查自己的部署目标。
不要把真实配置、API key、Body token 或证书私钥提交到仓库。

## 本地测试

在 `Komputer/` 目录下，用 Bash 执行：

```bash
(cd komputer-api && npm ci --ignore-scripts && npm test)
(cd klaud-kode-body && npm ci --ignore-scripts && npm test)
```

PowerShell 下分别进入两个目录，运行 `npm ci --ignore-scripts` 和 `npm test`。
测试会启动本机测试进程；API 使用替身 Claude、agy 和 muse CLI，不发真实模型请求。

开发前请看 [DEVELOPING.md](DEVELOPING.md)。
