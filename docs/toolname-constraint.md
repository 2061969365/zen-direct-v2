# 工具名约束与 12 通道方案（2026-11-16）

## 结论

上游 free 层只放行**精确官方工具名**，客户端自有工具名（`pwsh`、`team_task_*`、
`web_search` 等）一律 `FreeUsageLimitError` 429。

已落地解法：**ZEN_TOOL_CHANNEL 通道模式**——把 N 个客户端工具按语义映射到
12 个官方桩名，在参数里注入 `_tool` 选择器与 `_args`，出站只出现官方名；
响应侧从 `_tool` 还原真实工具名并展开 `_args`。GitHub Actions WARP 出口上
45 工具请求实测返回 **200**，工具调用正确还原。

## 环境

- 官方 CLI：`@opencode/cli@2.2.5`（本机与 GitHub Actions WARP runner 都已安装）
- 上游：`https://opencode.ai/inference/v1/chat/completions`
- 探针模型：`deepseek-v4.1-flash-free`

## 决定性对照（定位阶段）

| 出站工具形态 | 结果 |
|---|---|
| 14 个精确官方名（描述可自定义） | **200**，模型正确返回 tool_calls |
| 14 个官方名 + 1 个 `pwsh` | **429** |
| 45 个 DSH 工具原名 | 429 |
| 45 个 DSH 工具 + 零宽空格混淆（PR#179） | 429 |
| 12 官方工具桩（真实 prompt） | **200**，返回 `shell{"command":"ls"}` |
| 客户端 `read` 真定义 + 13 官方桩 | **200**，返回 `read{"path":"a.txt"}` |

结论：唯一变量是**工具名**，描述文本、参数 schema 都不是触发条件。

## 12 通道方案

### 出站（toolchannel.go）

- `toolChannelMap`：45 个已知工具按语义映射到 12 个官方通道
  （`pwsh`/`schedule_*`→`shell`，`web_search`→`websearch`，
  `team_task_*`/`spawn_teammate`→`subagent`，未知工具→`execute`）。
- `buildChannelTools`：每个官方通道的描述里列出它承载的真实工具，
  参数注入 `_tool`（选择器）与 `_args`（真实参数对象）。
- 由 `ZEN_TOOL_CHANNEL` 环境变量开启。

### 响应（toolchannel.go + streamrename.go）

- 非流：`restoreChannelCalls` 解析 `choices[].message.tool_calls` 与
  responses `output[]`，读 `_tool` 还原名称、`_args` 展开为顶层参数。
- 流式：分片被缓冲，等 `finish_reason` 帧发一条完整、已还原的 tool_call。
  通过 `NewRenameStreamReader` 在 renames 为空时也启用。

### 验证

- 本地 sink：出站 45→12 官方通道；响应 `read`→`read_image`、`_args` 展开正确。
- 流式 sink：分片 name/arguments 跨 chunk 缓冲，`finish` 帧还原为
  `web_search{"query":...,"limit":...}`。
- GitHub Actions WARP（run 43878017612）：45 工具请求 HTTP **200**，
  `TOOL_CALLS=1`，`name=web_search args={"query":"opencode"}`。

## TLS 指纹（另一层修复）

抓包对比官方 CLI 与网关的 ClientHello：`bun_hello.go` 补齐
`ed25519` / `ecdsa_secp521r1_sha512` 后，JA3 与官方 CLI 一致：

```
fc5d1fd2ab14d10c3e63b5125ef8a07d
```

## 已知边界

- `_args` 是自由对象，模型失去每个客户端工具独立 schema 的约束；
  通道描述里列出真实工具及原始描述作为补偿。
- 一个通道承载多个工具时靠 `_tool` 区分，需要模型按描述正确填写。
- 出口 IP 限流是另一层问题（`Retry-After` 可达 5-7 万秒），
  与本方案的工具名处理无关；那条需要 WARP IPv6 /64 出口改造。
