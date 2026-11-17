# zen-direct 架构、能力边界与工具扩展方案

2026-11-17。与 opencode2api v1.4.8 的对比结论来自本仓库 `docs/head2head-zen-vs-opencode2api.md` 的实测数据。

---

## 一、和 opencode2api v1.4.8 的区别

### 1.1 相同的地方

两边都在做同一件事：让上游免费层相信"这是官方 OpenCode CLI 在发请求"。

| 手段 | zen-direct | opencode2api |
|---|---|---|
| uTLS 复刻 Bun ClientHello（JA4 `t13d1713h1`） | `bun_hello.go` | `internal/httpx/bun_dial.go` |
| 官方 system prompt 注入 | `tools/opencode_prompt_v2.txt`（102 KB） | `internal/gateway/opencode_prompt.go` |
| 会话 id 铸造（descending 48bit + base62） | `identity.go` | `internal/identity` |
| 工具改名 | `mcp_` + sha256 前 12 hex | `mcp_` + sha256 前 12 hex |
| 补全官方工具集 | 追加 14 个官方工具 | 追加官方工具（真定义来自抓包 JSON） |

### 1.2 真正的差异

| # | 维度 | zen-direct | opencode2api | 影响 |
|---|---|---|---|---|
| 1 | 工具描述脱敏 | 有（`bannedDescriptionPhrases`） | 无 | **决定性**：真实 DSH 描述下 16/16 vs 12/12 |
| 2 | 参数级描述 | 随工具改名而躲过 | 随工具改名而躲过 | 两边都没有专门处理，都靠改名绕过 |
| 3 | DSML 文本工具调用恢复 | 有（`texttool.go`） | 无 | DeepSeek 写文本工具调用时只有我们能救 |
| 4 | UA 版本策略 | npm 跟版 + 兜底 2.3.0，只升不降 | 硬编码 2.1.1 | 对手已被打死（0/42） |
| 5 | 出口层 | 只直连 | 内置 WARP(WireGuard) + 代理池 + 出口游标 | 我们无出口管理能力 |
| 6 | 平台化 | 单进程隧道 | WebUI、多账号、模型目录/定价/可用性探测、模型轮转、Go 订阅 OAuth | 我们零运维面 |
| 7 | 官方工具定义来源 | 2.2 抓包 + 合成兜底 | 抓包原件 | 他们更忠实 |
| 8 | 协议面 | /v1/{chat,responses,messages} + /ip | 上述 + /v1/systemone | 各有侧重 |

**一句话**：他们是多租户 API 网关，我们是单点隧道。功能面他们宽得多，但两个字节级指纹他们都没有防线。

---

## 二、代码结构

87 个 Go 文件 / 20,897 行（含测试）。分六层。

### 2.1 入口层 — `main.go`（1696 行）

```
/health   /ip   /v1/models
/v1/chat/completions   /v1/responses   /v1/messages
```

`post(fmt)` → `buildUpstream()` 整形 → 转发 → 流式/非流式回写。

自愈逻辑全在这一层：

| 函数 | 作用 |
|---|---|
| `isCallerMismatch` | 调用方不匹配 → 剥私货重试 |
| `isFreeUsageLimit` / `nextFreeReset` | 免费额度窗口计算与冷却 |
| `egressIPChanged` | 出口 IP 漂移检测 |
| `logForensics` | 出站取证转储 |
| `isOfficialUA` | 官方 UA 直通分支（探针不能用 `opencode/` 开头，否则走这条） |

### 2.2 协议转换层 — `convert_req.go` / `convert_stream.go` / `convert_resp.go`（3140 行）

三种入口格式 × 三种上游格式 = **6 条流式路径** + 2 条非流式落定。全仓最复杂处，跨路径对照测试在 `convert_stream_crosspath_test.go`。

### 2.3 工具整形层 — 2633 行

| 文件 | 职责 |
|---|---|
| `toolshape.go` | 改名 `mcp_<12hex>`、追加官方集、顶层描述脱敏、透传门槛 `completeOfficialToolset`、历史 `tool_calls` 改写 |
| `toolchannel.go` | 12 官方桩名通道复用（`ZEN_TOOL_CHANNEL=1` 备用方案），参数用 `_tool` 选择器 |
| `texttool.go` | DSML 文本工具调用恢复：`Feed` / `Flush` / `<|DSML| tool_calls>` 解析 |
| `streamrename.go` | 流内工具名还原、DSML 汇入、多 choice 键归一（`callKey`） |

### 2.4 传输层 — 1400 行

| 文件 | 职责 |
|---|---|
| `bun_hello.go` | 按抓包重建 BoringSSL ClientHello：17 cipher、13 扩展、无 GREASE、ALPN 只报 http/1.1、X25519MLKEM768 首位。JA3 `fc5d1fd2ab14d10c3e63b5125ef8a07d` |
| `bun_wire.go` | 复刻 Bun fetch 的头序与头值 |
| `bun_transport.go` | 基于上述 TLS 的 HTTP transport |
| `sidecar.go` | Bun 子进程方案（生产 `DIRECT_SIDECAR=off`，已不用） |

### 2.5 身份层 — 456 行

`identity.go`（会话 id）/ `project.go`（项目 id）/ `ua.go`（版本跟版）/ `official_prompt.go`（提示词注入 + env 渲染）。

### 2.6 稳定性层 — 约 1000 行

`session_pool.go`（上游会话池）、`circuit.go`（5xx 熔断 Closed/Open/HalfOpen）、`egress.go`（出口追踪）、`probe.go`（探活）。

### 2.7 边角修补（每个对应一次实测踩坑）

`strip.go`（剥客户端私货）、`image_hoist.go`（图片消息重排）、`empty_content.go`、`badids.go`、`title.go`（标题请求识别）、`truncation`、`stubs*.go`、`table_refresh.go`（模型表）、`tool_images.go`、`acp.go`/`svc.go`（本机 opencode 服务发现，生产未用）。

---

## 三、能力边界

### 3.1 已验证跑通

- 三协议双向互转，6 条流式路径全部有测试 + 跨路径对照
- 45 工具多轮 + 流式 + `finish_reason=tool_calls` 闭环（生产实测）
- 真实 DSH 描述 / 参数级描述 / 官方名子集 / 完整官方集透传，四种形态全 200
- DeepSeek 把工具调用写成 `<|DSML| tool_calls>` 文本时还原为结构化调用
- 多出口并发（162 个出口做过成对测试）

### 3.2 局限

| # | 局限 | 说明 |
|---|---|---|
| 1 | 只有匿名免费层 | `OPENCODE_ZEN_API_KEY` 只是一个字符串出口，无 key 池、无轮转、无订阅兜底 |
| 2 | 无出口管理 | 不内置 WARP/代理池；出口被拉黑只能等或换 runner（由 zen-console 外部完成） |
| 3 | 无入站鉴权 | `setZenHeaders` 直接删客户端 `Authorization` 换成上游 key，任何 Bearer 都放行 |
| 4 | 无持久化 | 会话池、额度冷却、熔断状态全在内存，重启即丢 |
| 5 | 被动跟随 | 前缀、描述黑名单、官方工具定义、官方 prompt、TLS hello 全是抓包后写死 |
| 6 | UA 兜底会过期 | 现在 2.3.0；上游抬高下限后靠启动时 `primeUAVersion` 从 npm 取新值，npm 不通就退回兜底 |
| 7 | 单进程 | 无排队、无限流、无水平扩展；`DIRECT_CONCURRENCY=16` 只是信号量 |
| 8 | 官方定义会漂移 | `opencode_tools_2_2.json` 按 2.2 抓包，官方加新工具后我们补的是过期定义 |
| 9 | 模型目录是死的 | 无定价、无可用性探测、无模型轮转 |
| 10 | DSML 只在 chat 协议开 | `dsmlOn` 条件是 `protocol == FmtChat`；responses / messages 入口救不回来 |

---

## 四、以后增加更多工具怎么办

### 4.1 机制上已经是"无上限"的

| 环节 | 现状 | 是否会随工具数增长而失效 |
|---|---|---|
| 工具改名 | `mcp_` + sha256 前 12 hex，`used` map 冲突则 `_2`/`_3` | 不会。碰撞概率 2^-48 量级 |
| 名字长度 | 上限 64 字节，实际用 16 字节 | 不会 |
| 哈希稳定性 | 纯 sha256(名字)，与进程无关 | 不会，重启后名字不变，prompt cache 友好 |
| 官方集追加 | 固定 14 个 | 不会，占比随工具数下降 |
| 响应还原 | per-request `ToolRenames` map，O(1) | 不会 |
| 历史改写 | `rewriteToolReferences` 按消息遍历 + map 查表 | 不会 |

**所以客户端从 45 个工具涨到 120 个，网关不需要改一行代码。**

### 4.2 真正会先撞的三堵墙

**墙一：上游工具数 / 字节上限 —— 实测到 1500 个工具都没撞到**
2026-11-17 生产实测（随机工具名 + 中性描述，纯计数压力，双节点）：

| 客户端工具数 | 载荷字节 | 出站工具数（含 14 官方） | 结果 |
|---|---|---|---|
| 60 | 16 KB | 74 | 200 |
| 80 | 22 KB | 94 | 200 |
| 120 | 33 KB | 134 | 200 |
| 200 | 55 KB | 214 | 200 |
| 400 | 109 KB | 414 | 200 |
| 600 | 165 KB | 614 | 200 |
| 800 | 220 KB | 814 | 200 |
| **1500** | **414 KB** | **1514** | **200**（finish=stop，6.5s） |

全程网关计数 `up429=0 up4xx=0 up5xx=0`，前后各插一发 `plain` 对照也都是 200——不是额度侥幸。

**结论：工具数量本身不是约束。** 至少到 1500 个工具 / 414 KB 上游照收。

**墙二：模型注意力稀释（产品体验先崩的地方）**
45 个工具时 DeepSeek 已经会出现错选工具、把调用写成 DSML 文本。工具数翻倍会显著放大这两类问题。这不归网关管，但网关能缓解（见 4.4）。

**墙三：新描述 = 新指纹（运维负担）**
`bannedDescriptionPhrases` 是**逐条实测加进去的**。每加一个新工具，它的描述文本就可能是新的指纹，未命中黑名单就会原样出站。工具越多，这个清单越难维护。

### 4.3 分阶段方案

**阶段 0：新增工具当下要做的三件事（零代码）**

1. 客户端直接加，网关自动改名 + 追加官方集，**不需要改网关**
2. 跑一次发版闸门确认没炸：
   ```bash
   gh workflow run h2h-release --repo 2061969365/zen-warp-cli --ref master
   ```
   新工具追加一个载荷形态（`payloads/<新形态>.json`），矩阵里加一行
3. 如果出现 429，把新工具的描述丢进 `bannedDescriptionPhrases` 再跑一次

> 注意工具名与官方名重合的情况：v0.9.38 之后，只有**完整 14 个官方名**才走透传，任何子集（包括新工具同名）都走改名路径，不会误触发。

**阶段 1：工具数增长时 —— 已不需要测上限**
1500 个工具 / 414 KB 实测 200，数量维度没有墙。所以**不要**为此改架构。真正需要跟进的是下面两件事：
- 新工具的描述文本有没有命中上游指纹（跑 `h2h-release` 闸门即可发现）
- 工具多了以后模型选得准不准（这是产品问题，网关只能在必要时做裁剪）

**阶段 2：撞到上限就切 12 通道模式（`ZEN_TOOL_CHANNEL=1`）**

`toolchannel.go` 把任意多客户端工具按语义归到 **12 个官方桩名**，真实工具靠参数里的 `_tool` 选择器区分。出站恒定 12 个官方工具，**与客户端工具数无关**——这是设计上唯一能"工具无限增长"的兜底。

需要补强的两点：

1. **通道描述总长没有上限**。现在是每个成员 300 字节 cap（`channelMemberDescCap`），但成员数无上限，通道描述会线性增长。工具到 100+ 时要给通道描述加**总长上限**，超出部分折叠成"还有 N 个同类工具"。
2. **两步决策的准确率**。模型要先选通道、再选 `_tool`，比一次选准更容易错。建议只对低频工具走通道，高频（read/edit/shell/write/grep/glob 这类）仍然直连官方名。

**阶段 3：工具上百且体验要保住时，做动态子集**

客户端声明全集，网关按会话意图/最近使用裁剪到 ~20 个下发。这是主流 agent 框架的做法，但**需要客户端配合**：模型调不到被裁掉的工具时要有"工具未下发"的可识别信号，否则会表现为莫名调错。

在客户端不配合的前提下，不建议先做这件事——裁剪会造成不可见的失败。

### 4.4 建议的优先级

```
现在：什么都不改（机制支持，且 1500 工具实测无墙）
  ↓ 新工具导致 429
把新描述丢进 bannedDescriptionPhrases，跑 h2h-release 复测
  ↓ 工具 > 100 且模型明显选错
和客户端一起做动态子集 / 低频工具走 12 通道
```

**不要预先优化。** 数量维度已经被证伪——1500 个工具照过。剩下的风险只有"新描述是新指纹"和"模型选不准"，两者都不需要现在动架构。
