# 上游免费层门禁笔记（2026-10-27）

> 这份文件记录「上游到底看什么」以及每次结论对应的证据。
> 门禁由上游单方面改，任何结论都有保质期；日期和观测方式必须一起记。

## 0. 一句话现状

上游从 **2026-10-27** 起按 **客户端版本号** 放行。UA 里 `2.0.29` 直接 429，
`2.1.1` / 当前 CLI（2.2.0）正常 200。此前赖以过门禁的三件套（Bun ClientHello、
fetch 头序、官方 system 提示词）仍然有效，但**版本号是第四件、且会过期**。

**2026-10-28 追加第五条**：`messages[0]` 的**内容**也被校验了。
自造的 harness 提示词（哪怕格式像 v1 风格）开始被拒，必须发官方 2.x 正文 +
环境块 + skills。定位过程见第 8 节。

## 1. 门禁判据（按重要性）

| # | 判据 | 现状 | 位置 |
|---|---|---|---|
| 1 | TLS ClientHello = Bun（uTLS 复刻） | 有效 | `bun_hello.go` |
| 2 | fetch 的头顺序 / 大小写 / HTTP-1.1 only | 有效 | `bun_wire.go` |
| 3 | `messages[0]` = 官方 system 提示词 | **2026-10-28 起校验内容**：必须是官方 2.x 正文（`You are opencode, an interactive CLI tool…`）+ 环境块 + skills；自造 harness 提示词被拒 | `official_prompt.go` |
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

## 8. 抓包定位实录：10-28 全量 429 是怎么查出来的（2026-10-28）

这一节记方法，不只记结论：**上游判据变了以后，怎么在半小时内定位到具体字段**。
下次再变，照 8.11 的顺序走一遍即可。

### 8.1 现象

- 10-28 上午起网关（v0.9.2）几乎全量 429 `FreeUsageLimitError`；
  当天日志 `200=465 / 429=44`，14:14:51 之后没有一次成功。
- 同一台机器、同一个出口，官方 `opencode run` 正常返回 `ok`。
- 上游回的是 `FreeUsageLimitError`（"Free usage limit reached…"），**不是**
  403 shape gate，所以第一反应是"额度烧穿 / 出口被连坐"——这个第一反应是错的。

### 8.2 第一步：把「额度」和「请求形状」分开

两个工作流，都是**一个 IP 只跑一条链路**。原因：一次 free-limit 429 会把出口
连坐十几分钟，同 IP 顺序跑会污染后面的对照。

`binary-ab`（同 IP 依次执行）

| 步骤 | 实现 | 结果 |
|---|---|---|
| C0 | 官方 CLI 2.2.0 | ok |
| O1 | opencode2api v1.4.5（同为 Go + 匿名免费层） | **200** |
| Z1 | zen-direct v2 | **429** |

`gate-probe`（单 IP 顺序探针，IP `20.189.188.2`）

| 步骤 | 结果 |
|---|---|
| C0 官方 CLI | ok |
| Z1 我们 | 429 |
| C1 官方 CLI | **ok** |
| Z2 我们 | 429 |
| C2 官方 CLI | **ok** |

**CLI 在我们两次 429 之后仍然 200** → 出口没被连坐、额度没烧穿，
是**我们这条请求本身**被拒。

顺手否证了"路由变了"的猜测：`OPENCODE_ZEN_BASE=https://opencode.ai/inference/openai/v1`
（2.x 客户端二进制里确实有这条 family 路由）的 Z3 同样 429。

### 8.3 为什么改用抓包，而不是继续打上游

1. 每次 429 都连坐出口，云端对照只能回答"谁过谁不过"，答不出"差在哪个字节"。
2. 一轮 CI 要几分钟，改一次代码就得重跑，收敛太慢。
3. 我们手上有 opencode2api 的完整源码副本（`F:\worker\_opencode2api`）：
   同语言、同目标、当天 200，是理想的正对照。
4. 两个网关都支持自定义 base URL，可以把上游换成本机 echo ——
   **零额度、零连坐、逐字节可 diff**。

### 8.4 搭本地 echo（抓两个网关真正发出的 body）

脚本：`%TEMP%\opencode\echo.py`（约 60 行，只用标准库）。

要点（全是踩过的坑）：

- 每个 POST 把原始 body 落到 `req-NN.json`，把 path + headers 落到 `req-NN.meta`。
- **必须回合法 SSE**：网关为了过门禁强制 `stream:true`，客户端要非流时由网关
  自己聚合 SSE。echo 要回三段：一个 `delta` 事件、一个带 `usage` 的事件、
  一个 `data: [DONE]`。少了 `[DONE]`，网关会一直等到超时。
- 回 `Content-Type: text/event-stream` 和正确的 `Content-Length`。
- 端口避开常用值（我们用 18099）。

```powershell
python %TEMP%\opencode\echo.py %TEMP%\opencode\cap 18099
```

### 8.5 把我们的网关指过去

```powershell
$env:OPENCODE_ZEN_BASE = 'http://127.0.0.1:18099/inference/v1'
$env:PORT              = '18095'
$env:DIRECT_DEBUG      = '1'
$env:DIRECT_DIAG       = '1'
$env:LOG_FILE          = ''
Start-Process .\zen-direct.exe -WindowStyle Hidden

# 同一个请求体，两条链路都用它
curl.exe -X POST http://127.0.0.1:18095/v1/chat/completions `
  -H 'Content-Type: application/json' --data-binary @req.json
```

得到 `req-02.json`（10511 字节）。

> base 必须带 `/inference/v1`：网关只替换 origin，协议路径仍由它自己拼。

### 8.6 把 opencode2api 指过去（坑最多的一段）

opencode2api 的 config 是**扁平结构**（不是 `server.host` 那种嵌套），
`config.example.json` 就是模板：

```json
{
  "listen": "127.0.0.1:18096",
  "server_keys": ["k"],
  "anonymous": true,
  "proxies": ["direct"],
  "upstream": { "anonymous": "http://127.0.0.1:18099/inference" },
  "retry": { "max_attempts": 1, "timeout_seconds": 30 },
  "logging": { "dump_request_bodies": true },
  "webui": { "enabled": false, "password": "abcdefghij" }
}
```

三个坑：

1. `webui.password` **少于 10 字符直接拒绝启动**：
   `configuration error: webui.password must contain at least 10 characters`。
2. `/healthz` 返回 **503 是正常的**：它要去探测上游模型目录，而我们把上游指到了
   echo。**503 不影响转发**，请求照样 200 —— 别看到 503 就以为起失败了。
3. 必须带 `Authorization: Bearer <server_keys 里的值>`，否则 401。

得到 `req-03.json`（25115 字节）。

> opencode2api 自带 `logging.dump_request_bodies`，也能直接把出站 body 写日志；
> 但为了两边用同一把尺子（同一次抓包、同一份 echo），统一走 echo 更可靠。

### 8.7 逐字段 diff

脚本 `%TEMP%\opencode\diff.py`：读两个 JSON，打印顶层 keys、关键标量、
工具名清单、messages 形状，以及 `messages[0]` 的 unified diff。

输出（关键部分）：

```
ours keys   : ['messages', 'model', 'stream', 'stream_options', 'tools']
o2a  keys   : ['max_completion_tokens', 'messages', 'model', 'reasoning_effort',
               'stream', 'stream_options', 'tools']

ours tools (3): ['Read', 'shell', 'read']
o2a  tools (14): ['clint_Read', 'edit', 'execute', 'glob', 'grep', 'question', 'read',
                  'shell', 'skill', 'subagent', 'webfetch', 'websearch', 'write', 'patch']

ours messages: [('system', 'str:8314', 8547), ('user', 'str:30', 61)]
o2a  messages: [('system', 'str:9587', 9747), ('user', 'str:30', 61)]

system prompt equal: False  len ours=8314  o2a=9587
--- ours
+++ o2a
-You are an AI agent running in OpenCode, a coding agent harness. Help the user
- accomplish their goals using the tools you have available.
+You are opencode, an interactive CLI tool that helps users with software
+ engineering tasks. ...
```

**其余差异都不影响判定**：`max_completion_tokens` / `reasoning_effort` 缺省；
工具只有 3 个桩 vs 14 个官方工具。这两类差异此前已经验证过不影响放行
（`image-ab` 里形状完全不同也全 200）。

**唯一的内容级差异就是 `messages[0]`**：

| | 首条 system 内容 | 字节 |
|---|---|---|
| 我们 | `You are an AI agent running in OpenCode, a coding agent harness…` | 8314 |
| opencode2api | `You are opencode, an interactive CLI tool…`（正文 + env + skills） | 9587 |

### 8.8 为什么我们的提示词是错的

翻 `tools/official_system_prompt.txt` 的尾部：

```
<env>
  Working directory: /home/runner/work/zen-direct/zen-direct
  Workspace root folder: /home/runner/work/zen-direct/zen-direct
  Is directory a git repo: yes
  Platform: linux
```

`/home/runner/work/...` 是 **GitHub Actions 的路径**。也就是说这份"官方提示词"
不是从官方 CLI 抓的，而是**从我们自己跑在 CI 里的 harness 流量抓的**
（sidecar 时代为了过门禁随手存的快照）。

上游 10-27 只校验 UA 版本，v1 风格的提示词也能过（见 2.3 的 `p1u211`）；
**10-28 起开始校验 `messages[0]` 的内容**，这份自造提示词立刻被识别成非官方客户端。

### 8.9 修复

`official_prompt.go` 改成三段拼装，与 opencode2api 的 `prepareAnonymousBody` 同形：

1. **正文**：`tools/official_system_prompt.txt` 换成 opencode2api
   `internal/gateway/opencode_prompt_v2.txt`（8535 字节，`You are opencode, an
   interactive CLI tool…`）。
2. **环境块** `officialEnvironment(model)`：`You are powered by the model named …`
   + `<env>`（工作目录、git repo、平台、当天日期）。
3. **内置 skills** `officialSkills`。

判重前缀从"整段提示词前 80 字符"改成正文前 80 字符
（`You are opencode, an interactive CLI tool`），已注入过的不会重复插。

为什么不直接存整段静态文本：env 块里带工作目录和日期，静态存等于把抓包机器的
环境固化进二进制 —— 这次的坑就是这么来的。

### 8.10 验证

| 步骤 | 结果 |
|---|---|
| 同一请求体、同一出口，换提示词**前** | 429 `FreeUsageLimitError` |
| 换提示词**后**（本地直连真实上游） | **200**，正常返回 `ok` |
| `go test ./...` | 全绿（`official_prompt_test.go` 断言改成新前缀） |

云端多 IP 复核沿用 `binary-ab` / `gate-probe`：一 IP 一条链路，顺序固定。

### 8.11 可复用的排查顺序

1. **先分责**：同 IP 顺序探针（CLI → 我们 → CLI）。CLI 在我们之后仍然 200，
   就排除 IP / 额度。
2. **找正对照**：同语言、同目标、当天能过的另一个实现（这里是 opencode2api）。
3. **本地抓包**：两个实现都指向本机 echo，抓真正出站的 body。
4. **逐字段 diff**：顶层 keys → 工具清单 → messages 形状 → `messages[0]` 内容。
   先看形状，再看内容。
5. **只改一处**，本地直连真实上游验证一次（一发就够，别连发）。
6. **云端多 IP 复核**，一 IP 一条链路。

### 8.12 教训

- 云端 A/B 只能回答"谁过谁不过"；**逐字节 diff 才能回答"差在哪"**。
- 抓来的"官方样本"必须记来源和日期。这份提示词的真实来源是我们自己的 CI 流量，
  混进 `tools/` 之后没人再质疑过它。
- 上游判据是**逐条加**的：10-27 加 UA 版本，10-28 加提示词内容。每加一条，
  之前"能过"的旧证据就贬值一次。
- 带路径 / 日期 / 模型名的静态快照早晚过期；能动态拼的就动态拼。

## 9. 还没验证的

- `/v1/messages`、`/v1/responses` 端到端（10-26 只跑过 chat）
- 代理链路（CONNECT / SOCKS5）
- 上游下次会换什么判据（无法预测，只能保留排查工作流）
- 免费额度的准确口径（按 token 还是按请求、窗口长度）仍只能从
  `FreeUsageLimitError` 的时点反推
