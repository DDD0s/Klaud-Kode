# 安装 Kloud Kode

这份文档按“远端 Linux 主机运行 Claude Code，本地电脑发消息和执行工具”来写。
远端可以是云服务器，也可以是另一台自己的电脑。下面的远端命令使用 Bash。

目前没有一键安装器，也没有附带聊天客户端。先把 API 配好，再接你自己的本地客户端。
如果客户端不会执行工具，可以加装 Body，步骤在本文后半部分。

## 准备

远端需要 Git、Bash、curl，以及已经登录的官方 Claude Code。不需要安装 Go：`start.sh` 会按 `Komputer/VERSION` 从 GitHub Releases 下载对应平台的预编译二进制，并校验 SHA-256。
只有从源码开发或设置 `KLOUD_KODE_BUILD=1` 时才需要 Go 1.27 以上。旧的 JavaScript 实现仍留在仓库里，`KOMPUTER_IMPL=node` 可以回退运行它，那时才需要 Node.js 20 以上。

Claude Code 按 [官方安装说明](https://code.claude.com/docs/en/setup) 安装。
用准备运行服务的普通用户执行下面两条命令，确认能启动并完成登录，不要用 root 跑服务：

```bash
claude --version
claude
```

先在 CLI 里试一条普通对话。CLI 自己还不能工作时，不必继续排查 Komputer。
如果你要用订阅登录，检查运行环境里是否留着 `ANTHROPIC_API_KEY`；它会影响 CLI 使用哪种认证方式。

本地客户端需要支持 Chat Completions 或 Messages 接口。要让客户端自己执行工具，
还需要支持 `tools` 及工具结果回传。能填 Base URL 不代表它具备这些能力。

## 1. 远端：下载代码

```bash
git clone https://github.com/DDD0s/Kloud-Kode.git
cd Kloud-Kode/Komputer/komputer-api
cp env.example env
```

`start.sh` 第一次启动时下载预编译二进制到 `komputer-api/bin/`，校验 SHA-256，并在 `Komputer/VERSION` 变化时换用新版本；旧版本文件保留，正在运行的进程不会被覆盖。用 `KLOUD_KODE_VERSION=vX.Y.Z` 可以固定版本，`KLOUD_KODE_BIN` 可以指定已有二进制，`KLOUD_KODE_BUILD=1` 则改为从当前源码构建。
下载需要 `curl`，Windows 上解压还需要 `unzip`（Git for Windows 自带）。

下面的远端命令都在 `komputer-api` 目录执行。已有安装不要直接覆盖 `env`，升级说明见
[命名迁移](docs/komputer-migration.md)。

## 2. 远端：创建 API key

API key 是本地客户端连接这套服务的密码，不是 Claude 登录凭据。
下面生成随机 key，保存到仅当前用户可读写的 `KEYS.txt`；文件已经存在时会报错，不会覆盖旧 key。

```bash
openssl rand -hex 32 | sed 's/^/api-key: /' > KEYS.txt
chmod 600 KEYS.txt
```

客户端要填的是文件中 `api-key:` 后面的字符串。不要发到 Issue 或截图里。

## 3. 远端：先启动 API

编辑刚才复制的 `env`，加入：

```bash
KOMPUTER_API_HOST=127.0.0.1
KOMPUTER_API_PORT=18888
KOMPUTER_SKIP_TUNNEL=1
KOMPUTER_TUNNEL_UP=/bin/true
```

后两行适用于客户端自己执行工具的接法：不检查 Body，也不让启动脚本尝试建立 Body 隧道。
以后改用 Body 时要换掉，下面有说明。

`env` 会被 Bash 读取，带空格的值要加引号。远端需要出网代理才设置 `HTTPS_PROXY`，不需要就留空。
如果 `claude` 不在服务的 PATH 中，用 `CLAUDE_BIN` 指定它的完整路径。
模型的工作目录默认在 `~/.komputer/workspaces`；家目录受安全软件保护、或想放到数据盘时，用 `KOMPUTER_WORKSPACE_ROOT` 指定。

启动并检查：

```bash
./start.sh
KEY=$(awk -F': ' '/^api-key:/{print $2; exit}' KEYS.txt)
curl -fsS http://127.0.0.1:18888/v1/models \
  -H "Authorization: Bearer $KEY"
```

返回模型列表，说明服务和 key 能用。这一步还没有调用 Claude。
再试一条不使用工具的对话；这次会产生真实模型用量：

```bash
curl -fsS http://127.0.0.1:18888/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"komputer-claude","tools":[],"tool_choice":"none","messages":[{"role":"user","content":"请只回复 OK"}]}'
```

这里显式设置 `tool_choice: none`，只测试对话。不带工具设置的普通请求会走 Body 模式，
不能用来判断客户端工具是否接通。

启动失败先看 `logs/komputer-api.log`。停止和重启：

```bash
./stop.sh
./start.sh
```

`start.sh` 会放到后台运行，但不是开机自启或崩溃重启服务。首次搭建先手动跑通，再配置进程管理器。
它默认运行 Go 二进制；需要回退到旧实现时用 `KOMPUTER_IMPL=node ./start.sh`，两条实现共用同一份 `env`、`KEYS.txt`、`sessions.json` 和 `responses.json`。

## 4. 让本地能连到 API

上面的 `127.0.0.1` 只能在远端主机自身访问，本地客户端不能直接填它。

- 走公网：准备域名和证书，按 [公网 HTTPS 配置](docs/public-api.md) 设置直连 HTTPS 或反向代理。
  不要直接把明文 HTTP 端口开放出去。
- 已有私网或 VPN：使用私网中实际可达的地址。确认监听地址、路由和防火墙，不要假定 VPN 会自动转发到 `127.0.0.1`。
- 临时测试可以转发 API 端口。在本地电脑运行下面的命令，并保持这个终端打开：

```bash
ssh -N -L 18888:127.0.0.1:18888 your-user@your-server
```

### 用虚拟内网时怎么填

推荐用 Tailscale、ZeroTier 或 WireGuard：两台机器各自安装并加入同一网络，之后的地址用内网地址，
不需要公网 IP、路由器端口映射或 SSH。

远端 `env`：

```bash
KOMPUTER_API_HOST=0.0.0.0
```

本地客户端填大脑的内网 IP，例如 `http://100.64.0.7:18888/v1`。
有防火墙就只放行内网网段，不要顺带打开公网。

Body 也可以走同一个内网，不再需要 SSH 隧道：Body 的 `config.json` 把 `host` 改成内网地址（或设 `KLOUD_KODE_BODY_HOST`），
远端 `env` 设置 `KOMPUTER_BODY_URL`，MCP 配置会自动生成：

```bash
KOMPUTER_BODY_URL=http://100.64.0.8:8787/mcp/YOUR_BODY_TOKEN
```

设置了 `KOMPUTER_BODY_URL` 时不再使用 `tunnel-up.sh`，`KOMPUTER_SKIP_TUNNEL` 和 `BODY_SSH` 都可以不写。
token 也可以放在 `KOMPUTER_BODY_TOKEN` 里，地址就写成 `http://100.64.0.8:8787/mcp`。

最后一种情况下，本地客户端可以填 `http://127.0.0.1:18888/v1`。
这是后台传输，发消息仍在本地客户端里，不是让你每天 SSH 上去操作 Claude。

## 5. 本地：连接客户端

在客户端的自定义连接里填：

| 设置 | 内容 |
| --- | --- |
| 接口类型 | Chat Completions；或客户端支持的 Messages |
| Base URL | 例如 `https://api.example.com:18888/v1` |
| API key | 第二步的 key |
| 模型 | `komputer-claude` |

Messages 的完整请求地址是 `/v1/messages`。不同客户端是否自动添加 `/v1` 不一样，
按实际请求地址检查，避免变成 `/v1/v1/messages`。

启用客户端的本地工具后，先让它运行 `hostname`，再读取一个你事先准备的测试文件。
核对工具记录和实际结果，确认操作发生在本地电脑，而不是只看模型说“完成了”。
客户端工具请求的响应头应有 `X-Komputer-Mode: harness`。

具体客户端还没有逐一联调过。工具格式、重试和取消的要求见 [客户端工具协议](docs/harness-api.md)。
只支持 Responses API 的客户端可以使用 `/v1/responses`，同样支持函数工具和 `previous_response_id`。

## 可选：用 Body 提供本地工具

仅在不使用客户端工具时做这一段。Body 在你要操作的电脑上运行，提供文件、命令和进程工具。
一条请求里，客户端工具与 Body 不会混用。

### 本地电脑

按 [Body 安装步骤](kloud-kode-body/README.md) 配置并启动服务，默认监听 `127.0.0.1:8787`。
记下 Body token。它和 API key 是两把不同的钥匙。

### 远端到本地的连接

Body 不要裸露到公网。内置隧道脚本要求远端能 SSH 到本地电脑，通常需要先接入私网。
用你已有的 SSH 配置，把本地电脑设成 `body`，然后在远端检查：

```bash
ssh -o BatchMode=yes body echo ok
```

应直接输出 `ok`，不能等密码或主机指纹确认。首次连接请先手动核对主机指纹、配置密钥认证；
不要为了省事关闭 SSH 主机验证。如果远端不能主动连回本地，这个隧道方案就不能直接用，
可以改用前面的客户端工具接法。

### 远端 MCP 配置

仍在 `komputer-api` 目录，把模板复制到仓库外：

```bash
mkdir -p "$HOME/.config/komputer"
cp ../komputer-session/.mcp.json "$HOME/.config/komputer/mcp.json"
chmod 600 "$HOME/.config/komputer/mcp.json"
```

编辑复制后的文件，将 token 换成自己的：

```json
{
  "mcpServers": {
    "komputer_use": {
      "type": "http",
      "url": "http://127.0.0.1:18787/mcp",
      "headers": { "Authorization": "Bearer YOUR_BODY_TOKEN" }
    }
  }
}
```

不要在仓库里的模板中填写真实 token；模板本身是被 Git 跟踪的，`.gitignore` 不会保护它。

如果两台机器在同一个虚拟内网里，用上面的 `KOMPUTER_BODY_URL` 更省事：不用 SSH，也不用下面的模板。

否则（要用 SSH 隧道时）修改 `env`：删掉前面设置的 `KOMPUTER_TUNNEL_UP=/bin/true`，把 `KOMPUTER_SKIP_TUNNEL` 改成 `0`，
并补上另外两行：

```bash
KOMPUTER_SKIP_TUNNEL=0
BODY_SSH=body
KOMPUTER_MCP_CONFIG="$HOME/.config/komputer/mcp.json"
```

重启 API。客户端不发送 `tools` 或 `tool_choice` 时，就会使用这份 Body 配置。
也可以在远端测试：

```bash
curl -fsS http://127.0.0.1:18888/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -H "X-Conversation-Id: body-test" \
  -d '{"model":"komputer-claude","messages":[{"role":"user","content":"调用 system_info，告诉我电脑名和操作系统"}]}'
```

如果前面已经启用直连 HTTPS，把这里的地址换成证书对应的 HTTPS 域名。
返回的信息应属于本地电脑。Body 模式要保留多轮工具上下文，客户端需为每个聊天发送独立、稳定的对话 ID，
见 [API 会话说明](komputer-api/README.md)。

API 和 Body 在同一台机器时，不需要 SSH：MCP 地址改为 `http://127.0.0.1:8787/mcp`，
保留 `KOMPUTER_SKIP_TUNNEL=1` 和 `KOMPUTER_TUNNEL_UP=/bin/true`。

## 权限和数据

API 当前用 `--dangerously-skip-permissions` 启动 Claude，不会把本地的 permission 设置同步到云端。
客户端工具是否执行由本地客户端决定；Body 没有逐次审批功能，命令按运行 Body 的系统用户权限执行。
不要因为配置了 `roots` 就认为 shell 也被隔离了。

所有 API key 都能访问同一套会话和工具，没有不同用户之间的隔离。
不要提交 `KEYS.txt`、`env`、Body 配置、带 token 的 MCP 配置或证书私钥。
工具结果会返回远端，必要时包括文件内容；这不是“数据永远不离开本地”的方案。

## 常见问题

| 现象 | 先检查什么 |
| --- | --- |
| `401` | API key 是否填错，是否误填了 Body token |
| `502 upstream_auth_error` | 在运行服务的同一用户下启动 `claude`，检查登录 |
| `503 body tunnel unavailable` | 请求是否误走 Body 模式；确实用 Body 时检查本地服务和 SSH |
| `429` | 看错误类型和 `Retry-After`，不一定是 Claude 额度问题；也可能是会话上限或错误 key 尝试过多 |
| 本地连接超时 | 先测远端 `/v1/models`，再检查监听地址、路由、防火墙和客户端代理 |
| 只能聊天，不能操作电脑 | 客户端是否真的发送并执行工具；否则需要配置 Body |
| 工具结果返回 `409` / `410` | 是否重复提交了不同结果，调用是否过期，服务是否重启过 |
| 中文或日文输出乱码 | 保留原始输出，并提供 Body `system_info` 中的 `consoleEncoding` |

测试文件和临时项目比真实工作目录更适合第一次试用。
