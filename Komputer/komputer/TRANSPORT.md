# 链路

```
本地电脑 B：harness（发指令、执行工具）
   ⇅  OpenAI / Anthropic 协议（公网 HTTPS + API Key，或私网）
远端 A：komputer-api  ──spawn──>  官方 Claude Code CLI ──> Anthropic
```

这是日常主用法，本地 harness 不装在远端。客户端工具和结果走 API 往返，不需要云端主动连接本地。
不带客户端工具的请求仍使用官方 CLI → 工作站 `klaud-kode-body` 的 MCP 路径，
这条可选路径需要后台 SSH 隧道或私网。SSH 直连官方 CLI 也可选，不是日常操作要求。

- **大脑**不持有任何 Claude 凭据：每一轮都是真正的 `claude` 自己发请求。
- **身体**只认一个 token，默认只监听 `127.0.0.1`，靠隧道或 Tailscale 暴露。
- 身体上还能挂载本机其他 stdio MCP，云端通过 `call_mcp_tool` 调用，
  不必在大脑那边再装一份。见 `klaud-kode-body/README.md`。
