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

## 6. 还没验证的

- `/v1/messages`、`/v1/responses` 端到端（10-26 只跑过 chat）
- 代理链路（CONNECT / SOCKS5）
- UA 自动跟随在长期运行下的行为（定时刷新、跨主版本不跟）
- 上游下次会换什么判据（无法预测，只能保留排查工作流）
