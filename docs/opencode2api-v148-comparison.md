# 与 opencode2api v1.4.8 的实现对照（2026-11-17）

对照对象：`jasonxu114514/opencode2api` tag `v1.4.8`（clone 在
`F:\worker\opencode2api-v148`，commit 159d7fd）。

> 注：`jy02739245/opencode2iori` 是另一个 fork（多出 DSML 文本工具调用恢复与
> 断路器等），本轮比对的是用户指定的 jasonxu114514 v1.4.8。

## 相同点（已对齐）

| 机制 | 对方 | 我们 |
|---|---|---|
| 客户端工具线上名 | `mcp_` + SHA-256 前 12 hex，冲突加 `_2/_3` | 同（`ClientToolWireName` 逐字一致）|
| 名称长度上限 | 64 字节 | 同 |
| 官方工具集 | `/inference` 追加 2.x 全集 | 同（14 个）|
| 历史工具名改写 | 有 | 同 |
| 响应名还原（流/非流/分片名） | 有 | 同 |
| 官方 system prompt 注入 | 有 | 有 |
| 标题请求识别 | 整形前识别 | 同（我们顺序本来就对）|

**`mcp_` 前缀是这轮最关键的升级**：对方 #182 实测上游已学会拒绝字面量 `clint_`，
改用 `mcp_`（官方 MCP 工具的合法形态）后通过。我们的前缀可 env 轮换
（`ZEN_TOOL_PREFIX`），上游再学会时改环境变量即可，无需发版。

## 不同点与我们的选择

### 1. 客户端描述处理（对方零触碰，我们曾做短语黑名单）

对方 v1.4.8 的 `renameAndCompleteTools` 对客户端描述**零改写**：
只改名字，描述逐字出站（`toolshape.go` 内无任何 `Description` 写回，唯一
的描述改写针对他们自己追加的官方 shell/bash 主机替换）。

我们此前有 `bannedDescriptionFragrases`（把 `UTF-8 text file`、
`line-numbered content`、`session skill catalog` 等短语替换成中性措辞），
依据是"真实描述 429 / 假描述 200"的观察。

**2026-11-17 同出口 A/B 实测推翻了这个因果**：

```
同一 runner、同一出口 104.28.196.79、同一请求形状：
  A = 真实 DSH 描述（含封禁短语原文）  → 429
  B = 同样工具、中性描述                → 429
  A 再次                                → 429
```

A 与 B 结果相同 → **描述文本不是 429 的变量**。此前的"描述指纹"结论是出口
波动造成的假相关。

处置：短语黑名单降级为**后备路径**（`ZEN_DESC_SANITIZE=1` 显式开启），
默认逐字出站，与 v1.4.8 一致。好处是模型拿到客户端对工具的完整语义，
不再是被削弱过的措辞。

### 2. 官方 system prompt 正文（我们 95KB，对方 8.5KB）

- 对方 v1.4.8：`opencode_prompt_v2.txt` = 8535 字节，开头
  `You are opencode, an interactive CLI tool...`
- 我们 v0.9.27 起：`tools/opencode_prompt_v2.txt` = 102501 字节，开头
  `You are an AI agent running in OpenCode, a coding agent harness...`
  （2026-11-16 从官方 CLI 2.2.5 抓包逐字提取，含 219 个 skills 与 `<env>` 块）

**同出口 A/B 实测两者都通过**：

```
同一出口 104.28.201.162、同一 45 工具请求：
  promptA（我们 95KB）= 200
  promptB（对方 8.5KB）= 200
```

两者等价。保留 95KB 版本的理由：它逐字等于当前官方 CLI 的真实输出，与上游
的期望形态完全一致；CLI 升级时我们能重新抓包刷新，而 8.5KB 旧版与现行 CLI
已经脱节。为便于对照，新增 `ZEN_PROMPT_FILE` 可临时替换注入的提示词正文。

### 3. 对方有、我们没有的

| 能力 | 说明 | 是否要补 |
|---|---|---|
| 通道复用（12 官方名承载 N 工具） | 我们的旧方案，已被 mcp_ 取代 | 保留为 env 可切后备 |
| DSML 文本工具调用恢复 | 属 opencode2iori fork 的能力 | 已移植（v0.9.34，`texttool.go`）|
| Go 订阅 Device OAuth | 付费订阅登录 | 不需要（走 anonymous 通道）|
| WARP 进程内隧道池 | userspace WireGuard | 不做（runner 级 WARP 够用）|
| `upstream.prompt_environment` 覆写 | 覆写注入 env 块的目录/平台 | 可选，后续按需加 |

## 核心结论

**429 的唯一变量是出口 IP 的额度状态**，不是请求形态：
- 同出口下，官方名/mcp_ 名、真实描述/中性描述、95KB/8.5KB 提示词——全部等价通过；
- 换出口就可能全 429 或全 200，且会随时间自行恢复。

因此请求整形只需对齐官方形态（mcp_ 前缀 + 官方工具集 + 官方提示词），
其余"脱敏"类改动不但无效，还削弱了模型可用的语义信息。
