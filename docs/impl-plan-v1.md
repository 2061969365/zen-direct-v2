# 实施计划 v1：资产化 + 兜底 + 抓包流水线 + 诊断梯 + 定点轮换

依据 `docs/self-heal-and-drift-plan.md`。本文件是**并行施工的契约**：四个工作流各写各的文件，互不重叠，最后统一集成。

仓库：`F:\worker\zen-direct-v2`（单 Go 包，包名 `main`，根目录）。

---

## 0. 全局约束（所有人都要遵守）

1. **只改自己名下的文件**。需要改别人名下的文件时，**报告给 Lead**，不要自己动手。
2. **包必须随时可编译**。你提交的文件必须自成一体（只用标准库 + 已存在的符号）。`go build ./...` 必须过。
3. **不新增第三方依赖**（`go.mod` 谁的都不许改）。
4. **测试用 `_test.go`，放自己名下**。跑 `go test -run '你的测试名' ./...`。
5. **提交信息用中文**，一次任务一个提交。
6. 遇到设计冲突，**先按本文件的接口实现**，再把异议写进回报。

---

## 1. 工作流划分与文件所有权

| 工作流 | 负责人 | 独占写入范围 | 依赖 |
|---|---|---|---|
| **W1 资产核心** | `asset-core` | `assets.go`、`assets_test.go`、`assets/**`、`toolshape.go`、`toolshape*_test.go`、`main.go` | 无 |
| **W2 抓包流水线** | `capture-pipeline` | `tools/extract_assets.py`、`tools/README-assets.md`、`.github/workflows/refresh-assets.yml` | 只依赖 §2 的资产格式 |
| **W3 诊断梯** | `diagnostic-ladder` | `diag.go`、`diag_test.go` | 无（纯函数，自包含） |
| **W4 轮换状态机** | `rotation-policy` | `rotate.go`、`rotate_test.go` | 无（纯状态机，自包含） |

W1 动的 `toolshape.go` / `main.go` 是既有文件，**其他三人一律不许碰**。
W3/W4 只**新增**文件，引用不到 W1 的任何符号。

集成（把 W3/W4 接进 `main.go`、把 W4 接到 `toolshape.go` 的 prefix/desc 策略）由 **Lead** 在四个流全部交付后统一做。

---

## 2. 资产格式（W1 定义、W2 生产，双方必须一致）

目录 `assets/`，五个文件，全部是 UTF-8：

### `assets/official_tools.json`
```json
{
  "cli_version": "2.3.0",
  "captured_at": "2026-11-17T00:00:00Z",
  "tools": {
    "read":  {"description": "...", "parameters": {"type":"object","properties":{}}},
    "shell": {"description": "...", "parameters": {"type":"object","properties":{}}}
  }
}
```

### `assets/prompt.txt`
官方 system prompt 原文（纯文本，无 JSON 包装）。

### `assets/tool_policy.json`
```json
{"prefix":"mcp_","desc_mode":"blacklist"}
```
`desc_mode` 取值：`blacklist`（现在的行为）/ `digest`（首句摘要）/ `neutral`（全中性）。

### `assets/headers.json`
```json
{"order":["content-type","accept","user-agent"],"values":{"user-agent":"{{ua}}"}}
```

### `assets/tls_hello.json`
```json
{"ja4":"t13d1713h1","note":"Bun 2.2.5 / BoringSSL","ciphers":["..."],"extensions":["..."]}
```
W1 第一版只需要把现有常量**序列化出来**做可见性，不要求 `bun_hello.go` 真从它加载（那是 v2 的事，本计划不做）。

**W2 产出的 `official_tools.json` / `prompt.txt` / `headers.json` 必须逐字段符合以上结构**，否则集成时会失败。

---

## 3. W1：资产核心（`asset-core`）

### 3.1 要交付的接口（`assets.go`）

```go
package main

type OfficialToolDef struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolPolicy struct {
	Prefix   string `json:"prefix"`
	DescMode string `json:"desc_mode"`
}

type AssetVersion struct {
	SHA        string `json:"sha"`         // 前 8 位
	CapturedAt string `json:"captured_at"`
	Source     string `json:"source"`      // "embed" | "file"
}

type AssetSet struct{ /* 自己的字段 */ }

// LoadAssets 按 dir → embed 的顺序加载。dir 为空串表示只用 embed。
// 任何单个文件解析失败都回退到 embed 并记一条日志，绝不 panic。
func LoadAssets(dir string) *AssetSet

func (a *AssetSet) Prompt() string
func (a *AssetSet) OfficialTools() map[string]OfficialToolDef
func (a *AssetSet) ToolPolicy() ToolPolicy
func (a *AssetSet) Versions() map[string]AssetVersion
```

全局当前实例：`var currentAssets atomic.Pointer[AssetSet]` + `func Assets() *AssetSet`（未初始化时惰性加载 embed）。
启动时由 `main.go` 调 `LoadAssets(env("ZEN_ASSET_DIR",""))` 并 `currentAssets.Store(...)`。

### 3.2 `official_tools.json` 的初始内容来源

现有两个 embed 文件：`tools/opencode_tools.json`（v1 老集）和 `tools/opencode_tools_2_2.json`（2.2 集）。
`stubs.go` 现在 embed 的是后者。W1 需要写一个**一次性**的 `go run`-able 小工具或 `TestMain` 里的生成逻辑，把 `opencode_tools_2_2.json` 转成 §2 的格式落到 `assets/official_tools.json`。

> 转换规则：读现有 json 的 `{name: {description, parameters}}`（保持现状结构即可），外面包一层 `cli_version:"2.2.0"` + `captured_at`。

### 3.3 T3 宽松兜底（`toolshape.go`）

`renderOfficialTool` 现在未命中时用 `"Agent tool "+name` + 空 schema。改成按 `ToolPolicy.DescMode` 三档：

| desc_mode | 未命中定义时的描述 | schema |
|---|---|---|
| `blacklist`（默认） | `"Agent tool "+name` | `{"type":"object","properties":{}}` |
| `digest` | `"Agent tool "+name` | 同上 |
| `neutral` | `"Tool "+name+"."` | 同上 |

**命中真定义时三档都用真定义**，策略只影响"未命中"的兜底。
`neutral` 的形态已被实测（14 个官方名 + `"Tool read."` + 通用参数 → 4/4 200）。

### 3.4 S5 资产版本上报（`main.go`）

`/health` 的 JSON 增加：
```json
"assets": {
  "prompt":  {"sha":"a1b2c3d4","captured_at":"2026-11-17T00:00:00Z","source":"embed"},
  "tools":   {"sha":"e5f6a7b8","captured_at":"2026-11-17T00:00:00Z","source":"embed"},
  "policy":  {"sha":"11223344","captured_at":"","source":"embed"},
  "headers": {"sha":"55667788","captured_at":"","source":"embed"},
  "hello":   {"sha":"99aabbcc","captured_at":"","source":"embed"}
}
```
只加字段，不动现有字段。

### 3.5 验收

```bash
go build ./... && go test -count=1 ./...
```
- 新增 `assets_test.go`：embed 加载、空 dir、坏 JSON 回退、`Versions()` 五个 key 齐全、`ToolPolicy` 默认值
- 新增 `toolshape_asset_policy_test.go`：三种 desc_mode 下"未命中定义"的兜底文本
- 现有全部测试必须仍然通过（尤其 `toolshape_official_subset_test.go`、`toolshape_desc_default_test.go`）
- `main_test.go` 里如果断言了 `/health` 完整 JSON，按新字段更新

---

## 4. W2：抓包流水线（`capture-pipeline`）

### 4.1 目标

定时自动从官方 CLI 抓一次真实请求，抽出资产，和仓库版本 diff。有变化就开 PR；没变化就什么都不做。

### 4.2 交付物

**`tools/extract_assets.py`**
```
输入：一个目录，里面是 tools/capture_proxy.py 产出的捕获 json（含 body）
输出：到指定目录，写 official_tools.json / prompt.txt / headers.json（格式见 §2）
用法：python3 tools/extract_assets.py --captures DIR --out DIR/assets --cli-version 2.3.0
```
要求：
- 只取**第一条**带 `tools` 且 `messages[0].role=="system"` 的 `/inference` 请求（CLI 的首发）
- tools 归一化成 `{name: {description, parameters}}`，保留原始字段顺序无关
- prompt 取 `messages[0].content`
- headers 取该请求的头名顺序
- 缺字段要给出明确错误信息并以非零码退出

**`.github/workflows/refresh-assets.yml`**
```
on: workflow_dispatch + schedule(cron '0 3 * * *')
steps:
  checkout
  npm install -g @opencode/cli@latest
  python3 tools/capture_proxy.py --port 19100 --out "$W/captures" &
  跑一次 CLI（OPENCODE_DISABLE_AUTOUPDATE=1，最小 prompt，指向 sink）
  python3 tools/extract_assets.py --captures "$W/captures" --out assets_new
  diff assets_new/* assets/*
  有差异 → 覆盖 assets/ → 提交到 main（提交信息带 cli_version 与 sha）
  无差异 → 输出 "no drift" 并结束
  step summary 里打印：CLI 版本、工具数、prompt 字节数、差异摘要
```
**可以直接参考现成的** `zen-direct-v2/.github/workflows/capture-cli-vs-gateway.yml`（已经装 CLI + 跑 capture_proxy）。不要修改它。

### 4.3 约束

- **不许改任何 `.go` 文件**
- **不许改现有 `.github/workflows/*.yml`**，只能新增 `refresh-assets.yml`
- 用 `tools/sink.py` 或 `tools/capture_proxy.py` 现有能力，不要新造抓包器
- 工作流必须在没有 secrets 的情况下能跑（干净 runner）

### 4.4 验收

无法在本地跑 GitHub Actions，所以验收标准是：
1. `python3 -c "import ast;ast.parse(open('tools/extract_assets.py').read())"` 通过
2. 用仓库里现成的 `tools/cli_capture_body.json` 造一个假 captures 目录，跑 `extract_assets.py`，产出三个文件且格式符合 §2；把这一步写成 `tools/README-assets.md` 里的可复现命令
3. 把一个字段改掉再跑，diff 能看出来
4. `python -c "import yaml;yaml.safe_load(open('.github/workflows/refresh-assets.yml'))"` 通过

---

## 5. W3：诊断梯（`diagnostic-ladder`）

### 5.1 目标

429 时用 5 发探针定位"坏在哪一层"。**纯函数 + 注入的发送器**，不碰网络、不碰网关状态。

### 5.2 接口（`diag.go`）

```go
package main

type DiagStep struct {
	ID      string `json:"id"`      // "0","1","2","3","4"
	Layer   string `json:"layer"`   // "egress","official_tools","prefix","desc","params"
	Title   string `json:"title"`
	Payload []byte `json:"-"`       // 完整请求体
}

type DiagOutcome struct {
	Step  string `json:"step"`
	Code  int    `json:"code"`
	Class string `json:"class"`     // ok | free_limit | other
	MS    int64  `json:"ms"`
	Note  string `json:"note"`
}

type DiagReport struct {
	Egress   string        `json:"egress"`
	At       string        `json:"at"`
	Steps    []DiagOutcome `json:"steps"`
	Broken   string        `json:"broken"`   // ""=全通；否则是第一个失败的 layer
	Verdict  string        `json:"verdict"`  // healthy | egress_dead | drifted | rotated_needed
}

// BuildLadder 生成 5 步探针。model 用传入的模型名；
// officialToolsJSON 是官方工具集数组的原始 JSON（由调用方从资产里取），
// descSamples 是真实 DSH 描述样本（内置几条即可，不要求外部输入）。
func BuildLadder(model string, officialToolsJSON []byte) []DiagStep

// SendFunc 由调用方注入：发一次上游请求，返回 (status, body, elapsedMS)。
type SendFunc func(ctx context.Context, payload []byte) (int, string, int64)

// RunLadder 顺序执行。step0 不通过立即停（egress_dead）。
func RunLadder(ctx context.Context, egress string, steps []DiagStep, send SendFunc) DiagReport

func (r DiagReport) String() string // 人类可读多行
```

### 5.3 五步定义（必须逐字实现）

| ID | layer | 载荷 | 断言 |
|---|---|---|---|
| 0 | `egress` | 无 tools，一条 user 消息 | 网关可用性基线 |
| 1 | `official_tools` | tools = 传入的官方工具集 | 官方集仍被接受 |
| 2 | `prefix` | tools = 2 个第三方工具（名 `probe_alpha`/`probe_beta`），描述中性 `"Tool <name>."` | 改名路径可用 |
| 3 | `desc` | tools = 2 个第三方工具，描述用**真实 DSH 原文**：<br>`"Read a UTF-8 text file and return line-numbered content."`<br>`"Edit an existing UTF-8 text file by replacing literal text."` | 描述黑名单够用 |
| 4 | `params` | tools = 1 个 `read`，描述中性，`parameters` 用真实 DSH 原文（含 `"Path to edit, resolved by the filesystem backend"`） | 参数级安全 |

### 5.4 判定规则

```
step0 != 200                → Verdict=egress_dead, Broken="egress"（不往下跑）
step1 != 200                → Verdict=drifted,      Broken="official_tools"
step2 != 200                → Verdict=rotated_needed, Broken="prefix"
step3 != 200                → Verdict=rotated_needed, Broken="desc"
step4 != 200                → Verdict=rotated_needed, Broken="params"
全 200                      → Verdict=healthy,      Broken=""
```
`Class`：200→`ok`；429 且 body 含 `FreeUsageLimitError` 或 `Free usage limit`→`free_limit`；其余→`other`。

### 5.5 验收

`diag_test.go` 至少覆盖：
- 用假 `SendFunc` 构造 5 种场景（全通 / step0 挂 / step1 挂 / step2 挂 / step3 挂），断言 `Verdict` 与 `Broken`
- 断言 step0 挂时**只发了 1 发**（不等后续）
- 断言 `BuildLadder` 返回 5 步、ID 连续、每步 payload 是合法 JSON 且 `model` 字段正确
- 断言 step3 的 payload 里含真实 DSH 描述原文、step4 含 `Path to edit`

---

## 6. W4：轮换状态机（`rotate.go`）

### 6.1 目标

一个**纯状态机 + 落盘**，记录"当前用哪一档"，按诊断结果推进，支持回滚。不碰网络、不碰网关配置。

### 6.2 接口

```go
package main

type RotationState struct {
	PrefixIdx int       `json:"prefix_idx"`
	DescMode  string    `json:"desc_mode"`
	PromptIdx int       `json:"prompt_idx"`
	Advanced  map[string]string `json:"advanced,omitempty"` // layer -> RFC3339 最近切换时间
	UpdatedAt time.Time `json:"updated_at"`
}

var rotationPrefixPool = []string{"mcp_", "mcp__srv__", "tool_", "fn_"}
var rotationDescModes  = []string{"blacklist", "digest", "neutral"}

func DefaultRotation() RotationState
func LoadRotation(path string) RotationState            // 文件不存在/坏 JSON → DefaultRotation()
func SaveRotation(path string, s RotationState) error   // 原子写：tmp + rename
func (s RotationState) Prefix() string
func (s RotationState) DescMode() string

// Advance 推进一层；layer ∈ {"prefix","desc","prompt"}。
// 已在池尾时停在池尾并返回 note。
// 同一层 10 分钟内重复调用返回未变更状态（防抖）。
func (s RotationState) Advance(layer string, now time.Time) (RotationState, string)

// Reset 把一层重置回默认档。
func (s RotationState) Reset(layer string, now time.Time) RotationState
```

### 6.3 约束

- 落盘路径由调用方给，W4 不决定路径
- 只允许 stdlib
- `Advance` 必须是**值语义**（返回新状态，不改调用方）

### 6.4 验收

`rotate_test.go` 覆盖：
- 默认值 / 池内推进 / 池尾停留
- 10 分钟防抖：同一层连续两次调用，第二次不变
- 落盘往返：`SaveRotation` → `LoadRotation` 相等
- 坏文件、缺字段、空路径回退默认
- `Reset` 只重置目标层，其他层不动

---

## 7. 集成（Lead 在四个流交付后做）

1. `main.go` 启动：`LoadAssets(env("ZEN_ASSET_DIR",""))` + `LoadRotation(statePath)` → 用 rotation 的 prefix/desc_mode 覆盖 policy
2. `ClientToolPrefix()` 改为读 policy（保留 env 覆盖优先级：`ZEN_TOOL_PREFIX` > rotation > asset policy > `mcp_`）
3. `ZEN_DIAG=1` 时在 `/health` 旁边加 `/diag` 端点：跑 `BuildLadder` + `RunLadder`，返回 report
4. 全量 `go test`、`go build`（linux/amd64）
5. Actions 发版闸门 `h2h-release`：10/10 必须全 200
6. 推模板 → 生产验证

---

## 8. 交付顺序

```
W1 ──┐
W2 ──┼─→ Lead 集成 → 闸门 → 发版
W3 ──┤
W4 ──┘
```
W1–W4 完全并行，写入范围互不重叠。任何人发现必须改别人文件，停下来报告。
