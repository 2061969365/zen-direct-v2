# 上游免费层门禁笔记（2026-10-27）

> 这份文件记录「上游到底看什么」以及每次结论对应的证据。
> 门禁由上游单方面改，任何结论都有保质期；日期和观测方式必须一起记。

## 0. 一句话现状

上游从 **2026-10-27** 起按 **客户端版本号** 放行。UA 里 `2.0.29` 直接 429，
`2.1.1` / 当前 CLI（2.2.0）正常 200。此前赖以过门禁的三件套（Bun ClientHello、
fetch 头序、官方 system 提示词）仍然有效，但**版本号是第四件、且会过期**。

## 1. 门禁判据（按重要性）

| # | 判据 | 现状 | 位置 |
|---|---|---|---|
| 1 | TLS ClientHello = Bun（uTLS 复刻） | 有效 | `bun_hello.go` |
| 2 | fetch 的头顺序 / 大小写 / HTTP-1.1 only | 有效 | `bun_wire.go` |
| 3 | `messages[0]` = 官方 system 提示词 | 有效（v1 风格提示词仍过） | `official_prompt.go` |
| 4 | **UA 里的客户端版本 ≥ 门禁线** | **2026-10-27 新增，2.0.29 被拒** | `ua.go` |
| 5 | 匿名通道只走 v2 `/inference` | 我们本来就只走 | `OPENCODE_ZEN_BASE` |
| 6 | project id 形状 | 40 位 hex / `global`；`prj_*` 会被 403 | `project.go` |

## 2. 2026-10-27 的关键实验

### 2.1 现象

`gateway-e2e` / `native-gate` 全绿跑了一整天后，10-27 中午突然三个 shard 全 429；
同一批 IP 上官方 CLI（`opencode run`）全部成功。

先怀疑的是当时刚落地的「project id 每次现签」（opencode2api v1.4.5）。
用 `project-ab` 在三个独立 IP 上分别测 `global` / 固定 40hex / 每次现签：

| 策略 | IP | 结果 |
|---|---|---|
| `global`（旧行为，基线） | 52.230.180.0 | **429** |
| 固定 40 位 hex | 20.65.155.52 | 429 |
| 每次现签（两发） | 20.189.179.84 | 429 / 429 |

三种全灭，**包括旧的 `global` 基线** → 不是 project id 的锅。

> 注意：这里必须一 IP 一策略。已知一次 free-limit 429 会把出口连坐十几分钟，
> 同一 IP 上先跑的会污染后面的对照。

### 2.2 同 IP 同轮序

`binary-ab`（一个 IP，依次执行）：

| 步骤 | 实现 | 结果 |
|---|---|---|
| C0 | 官方 opencode CLI 2.2.0 | ok |
| O1 | **opencode2api v1.4.5 二进制**（匿名层） | **200** |
| Z1 | zen-direct v2（原生 uTLS 传输） | **429** |

同一 IP `172.184.210.50`：上游没变规则，是**我们的请求**落后了。

### 2.3 分离提示词与 UA

`prompt-ua-ab`：同一份抓包 body、真 session、同一个传输层，只换提示词和 UA，
每个变体独占一个 IP。

| 变体 | 系统提示词 | UA | 结果 |
|---|---|---|---|
| `p1u211` | 我们的（v1 风格，2.0.29 抓） | `opencode/latest/2.1.1/cli` | **200**（591ms） |
| `p2u211` | opencode2api 的 v2 提示词 | `opencode/latest/2.1.1/cli` | 429（163ms） |
| `p2u029` | opencode2api 的 v2 提示词 | `opencode/latest/2.0.29/cli` | 429（140ms） |

结论：

- **UA 版本是唯一变量**：2.0.29 全灭，2.1.1 就过。
- 系统提示词不用换（v2 提示词配 2.0.29 也照样 429）。
- 429 的延迟 140–180ms 是门禁直拒；200 是 591ms 的正常 SSE。

## 3. opencode2api v1.4.5 改了什么（2026-10-26 发布）

| 提交 | 内容 | 我们怎么处理 |
|---|---|---|
| `7d34bdd` | WARP 池预加载一半，注册不固定间隔 | 不适用（不用 WARP） |
| `5143e12` | 按 Cloudflare 节奏注册，停止探针抖动 | 不适用 |
| `5187184` | 首次注册遇限流等它过 | 不适用 |
| `9f59241` | ① 匿名层只走 v2 `/inference`<br>② 未声明 project 时每次现签 40 位 hex<br>③ 响应工具名按「大小写 → 分隔符」折叠 | ② 已跟进（`project.go`）；①本来就满足；③未照搬 |
| `fe49a02` | 池定时器改到锁内调度 | 不适用 |

**`internal/httpx/`（Bun ClientHello + fetch 头序）在这 5 个提交里零改动。**

## 4. 我们改了什么

| 文件 | 改动 |
|---|---|
| `ua.go` | 新增。UA 版本从写死改为「已验证值 2.1.1 + 后台跟 npm latest（同主版本才升）」 |
| `project.go` | 新增。project id：客户端声明 > `OPENCODE_ZEN_PROJECT`（原样）> 现签 40 位 hex |
| `main.go` | `setZenHeaders` 用 `uaString()`；project 不走 `setDefault`；启动 UA 刷新 |
| `probe.go` | 自检默认 UA 跟 `uaString()`；输出实际用的 project id |
| `.github/workflows/` | `project-ab` / `gate-recheck` / `binary-ab` / `prompt-ua-ab` 四个排查工作流 |

## 5. 运维要点

| 场景 | 做法 |
|---|---|
| 怀疑又是版本门禁 | 看 `ua.go` 的 `uaDefaultVersion`，或临时 `DIRECT_UA_VERSION=2.2.0` 试一发 |
| 想钉死 UA | `DIRECT_UA=opencode/latest/2.1.1/cli` |
| 关掉 UA 自动跟随 | `DIRECT_UA_REFRESH=0` |
| 想回到公共 project 桶 | `OPENCODE_ZEN_PROJECT=global`（不推荐：热门出口会撞墙） |
| 排障用标准库 TLS | `ZEN_TLS=go`（会 429，只用于确认「是不是传输层」） |

## 6. 验证结果（2026-10-27 修复后）

### 6.1 同 IP 同轮序（`binary-ab`，IP 134.33.67.127）

| 步骤 | 实现 | 结果 |
|---|---|---|
| C0 | 官方 CLI 2.2.0 | ok |
| O1 | opencode2api v1.4.5 | 200 |
| Z1 | zen-direct v2（现签 project + UA 自动跟到 2.2.0） | **200** |

### 6.2 网关端到端（`gateway-e2e`，3 个独立 IP）

| shard | IP | 流式 | 第二发（换 project 桶） | project id 数 | 标准库对照 |
|---|---|---|---|---|---|
| 1 | 20.64.206.x | 200（11 chunk，含 `[DONE]`） | 200 | 2 | 429 |
| 2 | 20.64.206.179 | 200（5 chunk） | 200 | 2 | 429 |
| 3 | — | 200（5 chunk） | 200 | 2 | 429 |

结论：**同一 session 连续两发、每次换 project 桶，仍然 200** —— 现签 project id
不会打断多轮对话。

### 6.3 本机常驻

- 二进制：`v0.9.1-dev`（`dist/zen-direct-v0.9.1-dev-{windows-amd64.exe,linux-amd64}`）
- 实测日志：`ua: client version 2.1.1 -> 2.2.0 (npm latest)`（走系统代理拉到的）
- 注意：常驻进程重启后 project id 每次现签，会话池不受影响

## 7. 图片 / 多模态（2026-10-28）

### 7.1 改动

- 新增 `tool_images.go`（自 opencode2api v1.4.5 `internal/protocol/` 移植：
  `decodeOpenAIBlocksChecked` / `chatToolImages` / `encodeChatBlocks`）。
- 新增 `image_hoist.go`：Chat tool 消息里的图片抬成独立 user 消息，
  tool 消息只留文本 + `[tool_image call_id=.. index=N]` 标记；
  顺序保持 `assistant(tool_calls)` → `tool...` → `user(images)`。
  无图请求一个字节都不改。
- 删除 `clampMediaURL`（原 64KB 截断会把大图换成 `[omitted N bytes]` 坏 data URL），
  改为原样透传；结构非法的 data URI 由上面两个函数返回 400。
- `DIRECT_DUMP_SHAPE=1` 打开请求形状日志（只打角色/块类型/字节数，不打内容）。

### 7.2 云端对照（`image-ab`，5 个独立 IP）

| shard | 通道 | 形状 | 状态 | prompt_tokens |
|---|---|---|---|---|
| 1 | 官方 CLI `--file red.png` | 官方上传 | 200「Red」 | — |
| 2 | 网关新 | 图在 user 消息 | 200「Red」 | 2669 |
| 3 | 网关新 | 图在 tool 消息（抬升后） | 200「Red」 | 2751 |
| 4 | 网关旧（HEAD） | 图在 tool 消息（不抬升） | 200「Red」 | 2722 |
| 5 | 网关新 | 1.05MB 大图，不截断 | 200 | 2669 |

shard 3 的形状日志：`assistant[] tool[text:56] user[text:37,image:466] user[text:60]`
—— tool 消息只剩文本+标记，图片进了独立 user 消息。

### 7.3 多轮缓存（`image-cache`，2 个独立 IP）

| 组 | 第 1 轮 | 第 2 轮 | 第 3 轮 |
|---|---|---|---|
| 带图 | prompt 2654 / cached 2432 | 2669 / **2560** | 2684 / 2560 |
| 纯文本 | 2470 / 2432 | 2485 / 2432 | 2500 / 1664 |

图片进上下文后**没有破坏前缀缓存**（第 2 轮 cached 反而涨到 2560），
每轮新增只有 ~120 token。

### 7.4 结论（修正之前的猜测）

- 图片本身**不是** 429 的成因：4 种形状全部 200，token 成本与纯文本同级，
  1MB 大图也不触发限流。
- shard 4 说明「图放在 tool 消息里」上游本来就认，A 方案属于**协议对齐**
  （与 opencode2api / 官方客户端一致），不是 429 修复。
- 真正烧额度的是**非缓存输入**：10-27 单日 `input` 合计 1 605 万 token
  （`cacheRead` 2.68 亿），有 88 次单请求非缓存输入 > 50k。免费额度是
  按 token 计的，长时间跑大上下文就会撞上 `FreeUsageLimitError`，
  和是否带图无关。

## 8. 还没验证的

- `/v1/messages`、`/v1/responses` 端到端（10-26 只跑过 chat）
- 代理链路（CONNECT / SOCKS5）
- 上游下次会换什么判据（无法预测，只能保留排查工作流）
- 免费额度的准确口径（按 token 还是按请求、窗口长度）仍只能从
  `FreeUsageLimitError` 的时点反推
