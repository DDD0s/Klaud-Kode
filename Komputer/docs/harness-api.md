# 本地 harness 接口（0.10.0）

日常主线是本地 harness → Komputer API → 远端官方 Claude Code。工具在本地执行，SSH 直连只是备选。

| 请求 | 执行路径 |
| --- | --- |
| 提供非空 `tools` 或显式 `tool_choice` | 真实 MCP 调用通过 API 返回，本地 harness 执行并回传结果；不需要 Body 隧道 |
| 不提供客户端工具 | 保留官方 CLI → 本地 `klaud-kode-body` 的 MCP 路径，需要私网或后台隧道 |

同一个客户端工具不会同时在两条路径执行。harness 模式关闭远端内置工具，MCP 配置只包含本次客户端工具。

## OpenAI Chat Completions

向 `/v1/chat/completions` 提交 `tools: [{type: "function", function: {name, description, parameters}}]`。
`parameters` 必须是 object JSON Schema。选中工具后，响应包含 `message.tool_calls`，
`finish_reason` 为 `tool_calls`；`function.arguments` 是 JSON 字符串，名称保持客户端声明的名称。

harness 检查权限并执行，随后追加 `role: tool` 消息，其 `tool_call_id` 匹配返回 ID，`content` 为执行结果。
下一次 POST 继续同一个 Claude 进程，不重跑已提交的工具结果；最终响应为 `finish_reason: stop`。

- 支持 `tool_choice: auto / none / required` 及 `{type: function, function: {name: ...}}`。
- `parallel_tool_calls: false` 每次最多交付一个工具；约束本地交付/执行，不等于改变上游内部规划。
- 流式结果使用 `delta.tool_calls`，包含 index、ID、名称与 JSON 参数；以 finish reason 和 `[DONE]` 结束。
- 结果可为字符串、文本块或 base64 图片块；不自动下载图片 URL。

## Anthropic Messages

向 `/v1/messages` 提交 `tools: [{name, description, input_schema}]`。
选中工具后返回 `tool_use` 块及 `stop_reason: tool_use`。
回传 user 消息中的 `tool_result` 块，使用对应 `tool_use_id`，失败可设置 `is_error: true`。
同一批结果必须完整回传，不能在这条结果消息中夹带新的用户指令。

支持 `tool_choice` 的 auto / none / any / tool 及 `disable_parallel_tool_use`。
SSE 包含 message_start、content_block_start/delta/stop、input_json_delta、message_delta 和 message_stop。
当前适配器未透传可验证的 thinking 签名，因此不输出伪造的 Anthropic thinking 块；
OpenAI 模式可返回 reasoning_content。

## 状态、权限与恢复

- 实际操作由本地 harness 的权限机制决定，不是同步修改两端的 permission 设置。原 Body 模式的权限默认值未改变。
- 活跃期相同初始请求重试得到相同工具 ID；相同结果重试重放同一响应。冲突结果返回 409，缺失/未知/过期 ID 返回 410。
  客户端仍应按工具 ID 去重；不要将此理解为所有外部操作都有事务或 exactly-once 保证。
- 必须一次提交上个响应中每个调用的结果。工具定义、模型、系统指令或 effort 不可在同一活跃回合中改变。
- 等待结果默认 5 分钟，可设置 `KOMPUTER_CLIENT_TOOL_TIMEOUT_MS`。整个 Claude 进程仍受 `KOMPUTER_CLAUDE_TIMEOUT_MS` 限制。
- 等待本地工具时释放推理名额，恢复时重新排队，避免父任务占满名额后阻塞本地子任务回调。
  `KOMPUTER_MAX_IN_FLIGHT` 限制计算中的回合；`KOMPUTER_MAX_CLIENT_TOOL_RUNS` 默认 16，另限活跃/缓存回合。
- 响应头为 `X-Komputer-Mode: harness` 和 `X-Komputer-Run`。
  已认证的 `DELETE /v1/tool-runs/<run-id>` 显式取消；断网不会自动取消，短暂断网可以重试。
- 活跃进程和有界重试缓存只在内存中，服务重启不能续接未完成的工具调用。先确认本地操作结果，再携带完整历史开启新用户回合，
  不要自动重复有副作用的操作。长任务宜用异步进程工具。
- 新用户回合由 harness 提供历史。`/v1/sessions` 的持久 UUID 映射属于原 Body 模式，不替代 harness 的工具状态。

## 范围

Responses API 的 `function` 工具和 `function_call_output` 走同一套 harness 流程，
响应头和取消方式相同；接口说明见 [API README](../komputer-api/README.md#responses-api)。
后端为 Antigravity 或 Muse 时，客户端工具同样通过本回合的临时 MCP 交给对应 CLI，见 [CLI 后端](backends.md)。

支持上述常用工具协议子集，不是所有模型 API 参数的完整替代品。不支持旧 functions/function_call、Responses 内置工具、
供应商内置 server tools、n > 1、外部 schema 引用或异步 schema 验证。
工具名为 1–64 个字母、数字、下划线或连字符；支持常见 draft-07 / 2020-12 object schema。
参数先校验再交付；required/named 选择无法满足会明确报错，不假造成功调用。
用量来自 Claude 的最终结果，工具阶段不能提供完整统计。

语义参考：[OpenAI function calling](https://developers.openai.com/api/docs/guides/function-calling)、
[Claude tool use](https://platform.claude.com/docs/en/agents-and-tools/tool-use/implement-tool-use)、
[MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-06-18/basic/transports)。
回归使用真实 HTTP/MCP 往返及假 CLI；具体 harness 与真实 Claude 的联调留到安装阶段。
