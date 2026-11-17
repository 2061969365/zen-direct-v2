# 实施计划 v2（经三方审核后重写）

取代 `docs/impl-plan-v1.md`。v1 保留只作对照，不再执行。

审核来源：Go 接口/并行契约、CI/发布、对抗性三份独立审核。三方共同结论：**v1 的 W4（轮换）与 W3（五级诊断梯）应当砍掉**，资产化方向对但抓错了维度，W2 不能直推 main。

---

## 一、v1 被推翻的三个判断（记录在案，避免重犯）

| v1 的判断 | 事实 | 证据 |
|---|---|---|
| T3「宽松兜底」= 1 小时、单项收益最高 | 伪需求。`renderOfficialTool` 永远返回合法条目；`experimental_image_generate` **今天每发请求都在走兜底**（通用描述 + 空 schema），生产 24/24 全 200 | `toolshape.go:206-234`、`:962`、`v2OfficialToolOrder` 14 项 |
| 资产化搬运"描述文本" | 抓错维度。承重的是**名字集合**，描述文本可脱敏 | `toolshape.go:960`「非官方名 pwsh 一律 429——放行的不是描述，是名字集合」 |
| 诊断梯五级能定位到层 | step2/3/4 是**已知必 429** 形态（纯第三方名、不带官方集），健康态也会假阳性 | 同上 + `shaped = append(shaped, official...)` |

**另外三个事实错误**（审核抓出，已核实）：

1. `tools/opencode_tools_2_2.json` 是 **list / 12 项 / 无 patch**；`tools/opencode_tools.json` 是 **dict / 13 项 / 含 patch**。生产 embed 的是后者（`toolshape.go:17`）。v1 §3.2 让资产从前者转换 → `patch` 丢真定义，基线 13→12。
2. 名字集合现在**三份不一致**：CLI 抓包 12 / 生产 embed 13 / 代码 `v2OfficialToolOrder` 14。
3. `prompt` 不是稳定资产：抓到的 system 含 `Today's date`、`Working directory: /home/runner/work/...`、`<env>`、AGENTS.md、Memory、available_skills，四个来源正文 94860 / 6396 / 8314 / 88342 字符。逐字 diff 永远不收敛。

---

## 二、v2 范围：两件半，串行

| 序 | 交付 | 为什么是它 | 工时 |
|---|---|---|---|
| **F** | 前置修复 4 项 | 不做则后面全是带病施工 | 0.5 天 |
| **A** | 官方工具名集合单权威化 + 对账 | 唯一被实测证明承重、且存在"某天全灭无人发现"的维度 | 1.5 天 |
| **B** | 429 三分器 + 免费对照 + 探测预算 | 替代整个诊断梯的触发与基线判断，且**不花额度** | 1 天 |
| **C** | 资产变更走 PR + 闸门（W2 降级） | 没有闸门的自动改指纹 = 夜里自己把系统改坏 | 1 天 |

**串行**，因为 F/A/B 都碰同一个 Go 包。C 的 workflow 部分可与 A 并行（只写 `.github/` 与 `tools/*.py`）。

---

## 三、F：前置修复（0.5 天，一人）

**F1 `ToolRenames` 存 prefix 快照**
`ClientToolPrefix()` 现在在**恢复路径**上被调用（`ResolveFuzzy` 的 `HasPrefix` 门、`Resolve` 的 `CutPrefix`、`RenameResponseTool` 的 `bytes.Contains` 早退）。任何"运行时换前缀"的改动都会让在途流出现"请求建立用旧前缀、恢复读新前缀"，客户端拿到未声明工具。`-race` 抓不到。
做法：`NewToolRenames()` 里取一次 `ClientToolPrefix()` 存进结构体，恢复侧全读快照，nil 回落全局。
> 即使本轮不做轮换，这也是潜在缺陷，先修掉。

**F2 并行改分支隔离**
单包是全量编译，真正的共享面是**包级命名空间**（符号重名 → 全包编译失败，四流同时停摆）、`gofmt -w .`、`git add -A`（HEAD 直接在 main）。
规则：每流一条分支；只 `git add` 自己名下文件；新增符号带流前缀；`gofmt -w` 只作用于自己文件；集成单独一条流并继承所有权。

**F3 统一诊断开关**
仓库已有 `DIRECT_DIAG`（`stats.go:15` 的 `diagOn()`，CI 里显式设 `DIRECT_DIAG=0`），不要再引入 `ZEN_DIAG`。

**F4 修运行时路径腐化**
`main.go:1241` 把取证文件写死到 `F:/worker/zen-direct-v2/last_429_upBody.json`（绝对 Windows 路径）。改成进程工作目录 + `ZEN_STATE_DIR` 覆盖，写盘失败只记日志。

---

## 四、A：官方工具名集合单权威化（1.5 天）

### A1 对账三方，定唯一权威

| 来源 | 名字数 | 用途 |
|---|---|---|
| CLI 抓包 | 12 | **唯一权威**（上游真实接受过） |
| `tools/opencode_tools.json` | 13（含 `patch`） | 定义文本来源，**保留 `patch` 真定义** |
| `v2OfficialToolOrder` | 14 | 现状硬编码，改为从资产读 |

产出 `assets/official_tools.json`：

```json
{
  "cli_version": "2.2.5",
  "captured_at": "2026-11-17T00:00:00Z",
  "source": {"workflow": "refresh-assets", "run_url": "...", "sha256": "..."},
  "order": ["edit","glob","grep","question","read","shell","skill","subagent","webfetch","websearch","write","execute","patch"],
  "tools": {
    "read":  {"description": "...", "parameters": {}, "strict": false},
    "patch": {"description": "...", "parameters": {}, "strict": false}
  }
}
```

规则：
- **`order` 必须是显式有序数组**。`tools` 是 JSON object，Go map 无序；顺序一旦从 map 派生，出站顺序会在请求间抖动，既与抓包顺序不一致，又让 prompt cache 失效。`order` 缺失按错误处理，**不许回退到 map 迭代**。
- `strict` 必须能表达。抓包里 12/12 个工具都带 `strict:false`；现在 chat 下是硬编码补的（`toolshape.go:224`），responses 下压根不带。格式表达不了就等于永远改不动。
- `source{}` 必须记来源。`docs/upstream-gate-notes.md` §8.12 的教训："抓来的官方样本必须记来源和日期"。
- `order` 取"CLI 抓包 ∪ 我们有真定义"的稳定子集，**不再追加抓包里没有的名字**
- `patch` 必须用 `tools/opencode_tools.json` 里的真定义，逐字节保留（加测试断言）
- `experimental_image_generate`：抓包里没有、定义也没有 → **移出出站集合**

### A1b `official_miss_mode` 默认 `omit`，不是 `stub`

现在缺定义的官方名会拿到 `"Agent tool X"` + **空 schema**（`{"type":"object","properties":{}}`）。这不是"通用参数"——R3 实测过的通用参数是 `{"properties":{"path":{"type":"string"}}}`，**空 schema 没有被任何实测覆盖**。

真实代价也不是"模型理解退化"，而是**官方名 + 空 schema 变成诱饵工具**，和改名后的 `mcp_<digest>` 真工具抢注意力。45 工具时就已经开始错选。

规则：
- `omit`（默认）：出站官方集 = `keys(tools) ∩ order`，缺定义的官方名**不出站**，只在 `/health.official.missing_names` 记一条并触发 drift 告警
- `stub`（仅在客户端自己声明了完整官方集、必须原样透传时用）：描述 `"Tool <name>."`，参数给 `{"type":"object","properties":{},"additionalProperties":true}`（留自由参数位，别给空对象）
- 验收：断言 13 个有定义的官方名全部出站、`experimental_image_generate` 不出站、且 `/health` 里出现该名字

### A1c 代码里的硬编码全部改为读资产

### A2 `/health` 暴露对账结果

```json
"official": {
  "source": "file|embed",
  "names": ["..."],
  "names_sha": "a1b2c3d4",
  "count": 13,
  "missing_defs": ["experimental_image_generate"],
  "cli_version": "2.2.5",
  "ua_version": "2.3.0"
}
```
`ua_version` 与 `cli_version` 必须能一眼比对——"A 版本 prompt + B 版本 UA"这种错配现在没人发现。

### A3 加载期语义下限（防止坏资产静默生效）

prompt ≥2KB、names ≥8 且 ⊆ 已知集、`captured_at` 不超前构建时间 30 天、五键齐全。不满足 → 记日志 + 回落 embed，并在 `/health.official.source` 标 `embed`。

### A4 验收

- 单测：assets names 与 `v2OfficialToolOrder` 去重后的预期一致；`patch.description` 与旧 embed 逐字节相同；`missing_defs` 正确
- 单测：坏资产（空 prompt / 截断 json / 0 工具 / 名字集为空）全部回落 embed，不得把空官方集发出去（空集会让 `completeOfficialToolset` 行为翻转）
- `ZEN_ASSET_DIR` **生效性**验证：放 sentinel 资产（prompt 带唯一 marker），mock 上游断言出站 body 里出现 marker —— 否则资产加载静默失效没人知道

---

## 五、B：429 三分器 + 免费对照（1 天）

替代 v1 的整个 W3。核心洞察来自对抗性审核：**别花额度去诊断，用已有证据**。

### B1 三分

复用现有 `isFreeUsageLimit` / `isPlainThrottle` / `freeLimitBlocked` / `egressIPChanged`，补一个显式判定：

| 类 | 判据 | 动作 |
|---|---|---|
| `quota` | body 含 `FreeUsageLimitError` 或 `Retry-After > 600s` | 等窗口，**不诊断、不轮换、不探测** |
| `throttle` | 429 但无 free-limit 特征 | 短暂退避后重试一次 |
| `fingerprint` | 429 且 `quota`/`throttle` 都不成立 | 才允许进入探测 |

### B2 免费对照（不发请求）

记录"最近一次真实 200"的：时间戳、`egress_ip`、形态指纹。

诊断时先读它：
- 若最近 200 在同一 `egress_ip`、N 分钟内、形态相同 → **对照成立**，可以判定"这一发 429 不是出口/额度问题" → 指向 fingerprint
- 若跨 IP 或无 200 → 对照作废，不发探针，直接返回"unknown，等下一发真实请求"

这样 v1 的 step0 从"发一发基线"变成"读已有记录"，**零额度成本**，且不会被"诊断自己把出口打死"污染。

### B3 探测预算（只在必要时花）

只有"超过 N 分钟无任何 200 且对照作废"才允许 1 发真探针：
- 预算 1 发 / 30 分钟，落盘且重启不清零
- 探针 payload **必须由生产同一套整形函数构造**（`buildUpstream`），否则测的不是生产流量
- 探针结果若为 `quota` → 只更新额度窗口，不产生任何"层坏"结论

### B3b 字面量会老化，必须带来源与期望方向

这是第四份审核的核心补充，v2 草稿漏了。

任何硬编码的 fixture（真实 DSH 描述、真实参数文本）都会随客户端版本老化。仓库里就有先例：`toolshape_desc_default_test.go:39-40` 抓的是 2026-11-16 那一代 DSH 的 `read` 描述，而现在 harness 的 `read` 描述已经不是那句了。

规则：
- 每个 fixture 带 `fixture_sha256` + `derived_from ∈ {capture, failing_body, literal}` + `captured_at`
- `literal` 超过 30 天 → `/health` 报 `probe_stale`，**绝不允许报 healthy**
- 绝不用"payload 里含某个子串"当验收（那只证明常量打对了），要么与抓包定义逐字节相等，要么标注"未经上游验证"
- 探针出现 400 且 body 是校验错误 → 判 `probe_invalid` 并中止，**不许**当成指纹变化

### B4 层归属只接受配对证据

不配对就不归因。这条即使不做轮换也要写进代码注释，避免下一个人重新发明五级梯：

| 层 | 需要的证据 |
|---|---|
| prefix | 同一 body、只换前缀的 2a/2b 分歧（2a 429 且 2b 200） |
| desc | canary 429（裸发真实描述，期望仍被拦）+ 整形后 200 |
| params | 第三方名和官方名两组，各只改参数一个维度 |
| 拿不到配对 | `Broken = "undetermined"`，**不猜** |


### B4 `/diag` 端点

- 只允许 loopback，或要求独立 token（生产 `main.go:1759` 监听 `:PORT` 全接口、无任何入站鉴权）
- 同进程 10 分钟最多 1 次
- 返回：三类计数、最近 200 对照状态、探测预算余额、当前生效配置
- **不返回"该轮换哪一层"**——没有轮换机制了

### B5 验收

- 单测：三类判定的输入输出矩阵（含 `Retry-After: 78735` 这种真实值）
- 单测：对照成立/作废/跨 IP 的三种分支
- 单测：预算落盘往返、重启后不清零、超预算拒绝
- 单测：非 loopback 请求被拒

---

## 六、C：资产变更走 PR + 闸门（1 天）

v1 让 cron 直推 main，三个审核都反对。降级为：

### C1 抓包用 `sink.py`，不用 `capture_proxy.py`

`capture_proxy.py` 是 Python TLS 直连上游，`sink.py` 的 docstring 自己写着"这条链路 ClientHello 变了必 429"；实测 `captures/` 里 6/6 全 429，CLI 90 秒内重试 5 轮。`sink.py` 不碰上游、零配额、CLI 不重试。
`upstream_status` 只作参考信息，**禁止作为判定条件**。

### C2 候选请求识别（现行判据不够）

```
按 rec.seq 排序
→ method == POST
→ path 匹配 ^/inference/v1/(chat/completions|responses)$
→ UA 匹配 ^opencode/latest/[\d.]+/cli$ 或存在 x-opencode-client（排除网关自产）
→ body 取 messages 或 input（/v1/responses 走 input）
→ 取 system/developer 消息
→ tools 数 ≥3 且名字 ⊆ 已知官方名（不认识的只报告，不静默收）
→ 候选里取 (工具数, system 长度) 最大者，而不是"第一条"
→ 按 body sha256 折叠重试
```
干扰项实测存在：标题生成请求先发（tools=0，system 1806B）、compaction、plan 模式子集。

### C3 prompt 必须 canonicalize

剥离 `<env>…</env>`、`Today's date:` 行、`Here is some useful information…`、available_skills 段、AGENTS.md/Memory 段。diff 只比 canonical sha256。运行时仍由 `opencodeEnvironment()` 拼尾。
补充：`official_prompt.go:84` 硬编码 `Prefer F:\dsh-temp\opencode`，抓包里是 `/tmp/opencode`、`Platform: linux` —— 逐字 diff 永远不会收敛，这就是必须规范化的直接证据。

**这条不是洁癖，是重放事故。** 线上 system 是"静态正文 + 动态 env 尾"（`official_prompt.go:90-99`）。抓包里的 `messages[0].content` 是渲染后的整体，含 CI 的 `/home/runner/work/...` 和抓包当天日期。写进 `assets/prompt.txt` 再叠 env 尾 → **env 段重复 + 把抓包机环境固化进资产**。`docs/upstream-gate-notes.md` §8.8 的事故正是这个（"静态存等于把抓包机器的环境固化进二进制，这次的坑就是这么来的"）。

硬规则：
- extractor **必须以 `Today's date: ` 为界切分并丢弃尾部**，不许存整段
- 丢弃的尾部要与 `opencodeEnvironment` 的模板逐字 diff，不一致就非零退出（说明上游改了 env 段的形状，这是重要信号，不能被静默吞掉）
- `assets/prompt.txt` 只允许静态正文，入库前断言不含 `Today's date:` 与 `Working directory:`


### C4 只开 PR，不推 main

- cron 只产出 `assets_candidate/` + 开 PR（label `asset-drift`），**`assets/**` 的写入权仍归 A**
- PR 上跑：`go test ./...`、`LoadAssets` 的 unmarshal 校验 job、工具数/名字集合 diff 摘要、per-field sha
- 自动合并仅限"仅描述文本变化 / 名字集合不变 / canonical prompt 变化 <5%"，其余人工
- 显式 `permissions: contents: write, pull-requests: write`（新仓库 GITHUB_TOKEN 默认只读）
- `concurrency` 防叠加、`timeout-minutes`
- 四态退出码：`NO_CAPTURE` exit 1 + `no-capture` 标签；`EXTRACT_FAIL` exit 1 打印候选 seq；`DRIFT` 开 PR；`NO_DRIFT` exit 0

### C5 脱敏四层

采集层头值 → `<redacted>`（保留 UA）；提取层只写头名 + 模板值，白名单 `{content-type, accept, user-agent}`；提交前 grep `Bearer |ses_|ghp_|sk-|[0-9a-f]{24}`；artifact 只传 `assets_candidate/`，原始 captures 不传。
实测头里有 `x-opencode-project`（稳定 24 hex）、`x-opencode-session`、`x-session-id`；`.gitignore:21,25-26` 已写明 captures 含凭证不入库，v1 让 extract 采 headers 正好绕过这条。

### C6 已知的静默死亡

`schedule` 只在默认分支生效；仓库 60 天无活动会被自动停用 → 自愈静默死亡。工作流失败要追评 issue（label `asset-drift` / `no-capture`），不能只依赖邮件。

---

## 七、砍掉的内容（明确记录）

| 砍掉 | 理由 | 后果 |
|---|---|---|
| **W4 轮换（`rotate.go`）** | 10 分钟防抖 << 实测 `Retry-After ≈ 22h`，足以把 4 个候选全"试失败"并永久停在池尾；`fn_`/`tool_` 是自造形态，`mcp_`+12hex 恰好等于真实 MCP 客户端，轮换是往"独一份"走；5 台节点各自落盘会造成状态分叉 | 429 时仍需人判断。但没有"把能用的形态试坏且回不去"的风险 |
| **五级诊断梯** | step2/3/4 是已知必 429 形态，健康态也假阳性；定位到的层今天也没有可执行动作 | 只能答"额度还是指纹漂移"，答不了"坏在 prefix 还是 desc" |
| **T3 三档 desc_mode** | 打不到真变量（承重的是客户端描述，走 `sanitizeToolDescription`）；`digest` 与 `blacklist` 在 v1 里逐字节相同 | 无。真正的兜底今天已在跑且 200 |
| **`headers.json` / `tls_hello.json`** | 无消费者，`AssetSet` 没有 accessor，只有 sha 是装饰 | 无。v2 再上 |
| **prompt 多版本池 / `PromptIdx`** | `assets/` 只有一个 `prompt.txt`，推进后无处消费 | 无 |

---

## 七b、顺手要清的文档/代码不一致（0.5 小时，挂在 F 里）

第四份审核查出的既存矛盾，不清掉会把下一个实施者带偏：

| 位置 | 现状 | 实际 |
|---|---|---|
| `toolshape.go:182` 注释 | "`ZEN_DESC_SANITIZE=1` 时启用（默认关闭）" | 开关是 `ZEN_DESC_VERBATIM`，且默认**开启**（`:1136`、`toolshape_desc_default_test.go:18`） |
| `docs/head2head-*.md` | 142 个出口 / 284 发 | `docs/stage-summary-*.md` 写 162 / 324（后续两轮加进来的），统一为 162/324 |
| `stubs.go` | 被描述为"官方定义源" | `renderOfficialTool` 读的是 `toolshape.go` 的 `tools/opencode_tools.json`（13 个 dict）；`opencode_tools_2_2.json`（12 个 list）只喂 `mergeChatStubTools`，生产路径不用 |
| `docs/architecture-and-limits.md` §4 | "官方工具定义会漂移" | 漂移的**不是描述文本，是名字集合**（本轮实测），改写这一条 |

最后一条尤其重要：v1 的整条资产化路线就是被这个错误描述带偏的。

---

## 八、执行顺序

```
F (0.5d)  ──→  A (1.5d)  ──→  B (1d)  ──→  C (1d)
                              ↑
                    C 的 workflow 部分可与 A 并行
```

每步收口条件：`go build ./...` + `go test -count=1 ./...` 全绿 + 该步的验收单测齐备。
A/B 完成后必须过**实网闸门**才能推生产：至少 `plain`、`real_dsh`、`q1_st_realp` 三类各 4 发（v1 只做 `plain` 没有鉴别力——历史数据里 `plain` 6/6 全 200 同期 `real_dsh` 0/6）。

---

## 九、回滚（v1 完全缺失）

1. **运行时杀开关**：`ZEN_ASSET_SAFE=1` 只读 embed；一次 env 改动、不重编译
2. **资产按 sha 版本化**：`assets/<sha8>/` + `assets/current` 指针，回滚 = 改指针 + 重启
3. **加载期语义下限**（A3）：不满足自动回落 embed，`/health.official.source` 会显示 `embed`
4. **rotation 已砍**，无需回滚设计
5. **git 侧**：`git revert` + tag `assets-<sha8>`，embed 随构建回退
6. 四例负测进 CI：坏资产目录 / 空 prompt / 截断 json / 空名字集 → 必须仍 200 且 `source=embed`
