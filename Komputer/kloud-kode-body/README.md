# Kloud Kode Body

Body 跑在你想操作的电脑上，提供文件、命令、进程和本机 MCP 工具。
远端 Claude Code 通过它操作这台电脑。

如果本地 harness 已经负责执行工具，就不需要额外安装 Body。
整套服务的配置顺序见 [安装步骤](../SETUP.md)。

## 安装

需要 Git 和 Go 1.27 以上。服务是 `Komputer/cmd/kloud-kode` 编出来的单个二进制，
启动脚本在第一次运行时自动编译；旧的 JavaScript 实现仍可用 `KOMPUTER_IMPL=node` 回退，那时才需要 Node.js 20 以上。
在要操作的电脑上打开终端：

```bash
git clone https://github.com/DDD0s/Kloud-Kode.git
cd Kloud-Kode/Komputer/kloud-kode-body
```

已有代码就直接进入 `Komputer/kloud-kode-body`。

生成一个随机 token：

```bash
openssl rand -hex 32
```

（Windows 没有 `openssl` 时，用 `python -c "import secrets;print(secrets.token_hex(32))"`。）

配置文件默认在用户目录下。第一次安装时，复制模板：

Windows PowerShell：

```powershell
New-Item -ItemType Directory -Path "$env:USERPROFILE\.kloud-kode-body" -Force
Copy-Item config.example.json "$env:USERPROFILE\.kloud-kode-body\config.json"
```

Linux / macOS：

```bash
mkdir -p "$HOME/.kloud-kode-body"
cp config.example.json "$HOME/.kloud-kode-body/config.json"
chmod 600 "$HOME/.kloud-kode-body/config.json"
```

已有配置不要覆盖。编辑复制后的 `config.json`，把 `token` 换成刚才生成的值，
把 `roots` 改成实际存在的工作目录。例如 Windows：

```json
{
  "port": 8787,
  "host": "127.0.0.1",
  "token": "YOUR_RANDOM_TOKEN_AT_LEAST_32_CHARS",
  "roots": { "project": "D:\\Projects\\demo" },
  "defaultRoot": "project",
  "files": true,
  "shell": true,
  "processes": true
}
```

路径要换成你自己的。Linux / macOS 可以用 `/home/you/projects/demo` 这类绝对路径，或以 `~` 开头的路径。
不要直接把占位 token 当成密码使用。

启动：

Linux / macOS：

```bash
./start.sh http
```

Windows PowerShell：

```powershell
.\start.ps1 http
```

两个脚本都会在需要时编译二进制，然后启动 `kloud-kode body http`；
也可以先 `go build -o kloud-kode ./cmd/kloud-kode`（在 `Komputer/` 目录）再直接运行 `./kloud-kode body http`。

看到监听 `127.0.0.1:8787` 的日志后，保持进程运行。另开终端访问 `http://127.0.0.1:8787/healthz`，
确认服务能响应。健康检查不需要 token，也不代表 MCP 鉴权已经通过。

可用 `KLOUD_KODE_BODY_CONFIG` 指定其他配置文件，`KLOUD_KODE_BODY_TOKEN` 覆盖配置里的 token。
`./start.sh stdio`（或 `kloud-kode body stdio`）提供给同机 MCP 客户端使用，不启动 HTTP 端口。
HTTP 模式下 token 也可以写在路径里（`http://主机:端口/mcp/<token>`），给只能配置 URL、不能加请求头的客户端用。

## 远端怎么连接

默认只监听本机。按 [安装步骤](../SETUP.md) 建立 SSH 隧道后，远端 MCP 地址是
`http://127.0.0.1:18787/mcp`，配置如下：

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

这份带 token 的配置保存在仓库外，通过 `KOMPUTER_MCP_CONFIG` 告诉 API 它的位置。
`/mcp/<token>` 地址仍可用于不支持请求头的客户端，但 token 会出现在 URL 中，不要把这类地址写进日志或截图。

## 能做什么

| 工具 | 用途 |
| --- | --- |
| `system_info` | 电脑名、系统、shell、时间、工作目录和功能开关 |
| `run_command` | 执行命令，分别返回 stdout、stderr 和退出码 |
| `start_process` / `read_process` / `write_process` / `stop_process` / `list_processes` | 管理跨回合运行的进程，例如开发服务器；按游标读取新增输出 |
| `read_file` / `write_file` / `edit_file` | 读写文件；按行读取大文件，按唯一匹配的原文编辑 |
| `list_directory` / `find_files` / `search_files` / `stat_path` | 列目录、查找文件和内容、读取文件信息 |
| `create_directory` / `delete_path` / `move_path` | 创建目录、删除和移动文件 |
| `list_mcp_servers` / `list_mcp_tools` / `call_mcp_tool` | 调用这台电脑上的其他 stdio MCP 服务 |

命令输出先尝试严格 UTF-8 解码，失败后尝试系统控制台编码，并报告所用编码。
日志超过上限时保留开头和结尾，注明省略的字节数。默认输出上限为 256 KiB，文件读取上限为 8 MiB。

`run_command` 和 `start_process` 的 `shell` 可选 `default`（Windows 上是 cmd）、`cmd`、`powershell`、`pwsh`、
`bash`、`sh`、`wsl`；`system_info` 的 `availableShells` 列出这台电脑上有的。Windows 上 `bash` 指 Git for Windows 的 bash，
不会用到 System32 里 WSL 的 `bash.exe`；`wsl` 运行 `wsl.exe -e bash -c`。
PowerShell 命令不需要再套一层引号：pwsh 经 `-EncodedCommand` 传入（很长的命令写成临时 `.ps1`），
Windows PowerShell 5.1 总是写成临时 `.ps1`，因为它在 `-EncodedCommand` 下把错误输出成 CLIXML。
错误是纯文本，没有进度条，表格按 4096 列输出、不在 80 列处折行，退出码与 bash 一样取最后一条语句
（失败的外部程序带回自己的退出码）。`consoleUtf8`（默认 `true`）让 PowerShell 和 cmd 的输出用 UTF-8：
cmd 先执行 `chcp 65001`，再在新启动的 cmd 里运行命令，因为 cmd 只在启动时读取代码页。
设为 `false` 时保留系统代码页，比如 GBK，再按上面的方式解码。
`start_process` 可传 `idleTimeoutMs`：超过这么久没有人读写，进程就被结束，调用方断开后不会一直留着。

## 连接本机的其他 MCP

先在本机安装并测试好对应 MCP，再把它的启动命令加入 Body 配置的 `mcpServers`。
下面的命令名只是占位，要替换成实际的可执行文件和参数；把这一项合并进自己的配置，不要覆盖其他设置。

```json
{
  "mcpServers": {
    "my_app": {
      "command": "your-mcp-server",
      "args": []
    }
  }
}
```

通过 `list_mcp_servers` 和 `list_mcp_tools` 检查，再用 `call_mcp_tool` 调用。
应用和 MCP 都装在要操作的电脑上，不用在远端再装一份。
是否能操作 GUI、Office 或其他应用，取决于你接入的工具；Body 自己没有完整的桌面控制功能。

## 权限

Body 没有弹窗审批功能。拿到 token 并能连到端口的人，可以使用所有已开启的工具。
命令按启动 Body 的系统用户权限运行，不会同步其他客户端的 permission 设置。

`roots` 限制文件工具的路径，不限制 shell 命令。关闭 `shell` 可以关闭命令和进程工具，
但另外接入的 MCP 仍有自己的能力和权限，需要单独检查。

不要直接把 Body 端口开放到公网，也不要用管理员权限运行，除非你明确需要并接受这些权限。
要操作桌面应用时，还要考虑登录会话；在后台服务账户下启动，并不等于能操作当前用户的桌面。

## 测试

在本目录执行 `npm test`。测试会启动真实 Body 进程，检查 HTTP 鉴权、路径限制、
输出解码和截断、进程读取和终止等行为。
