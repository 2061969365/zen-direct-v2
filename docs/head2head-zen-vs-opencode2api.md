# zen-direct vs opencode2api v1.4.8：同一出口 IP 上的对跑

2026-11-17。目标不是评优，而是把"哪些字节会让上游免费层 429"从猜测变成可复现的成对数据。

## 方法

GitHub Actions 矩阵里每个 job 是一台全新 runner，也就是一个全新的公网出口 IP。同一个 job 内同时起两个网关，打**逐字节相同**的请求：

| 端口 | 进程 | 版本 | 上游 |
|---|---|---|---|
| 8081 | `zen-direct` | v0.9.37 / v0.9.38 / v0.9.39 | `https://opencode.ai/inference/v1` |
| 8080 | `opencode2api` | v1.4.8（自建，commit `159d7fd`） | `https://opencode.ai/inference`（anonymous lane，direct） |

三个关键控制：

1. **同 IP 成对**：两个二进制共享一个出口，差异只能来自二进制本身。
2. **顺序对调**：`zen_first` / `o2a_first` 各占一半。免费层按出口记账，先打的那一方占"干净额度"，顺序对调后若结论翻转，就说明看到的是额度效应而不是形态效应。
3. **首槽位单独统计**：只看每个 IP 上的第 1 发（`seq=1`），那是完全未被消耗的出口。

测试脚本在 `2061969365/zen-warp-cli`：`scripts/boot.sh`（起网关）、`scripts/h2h.sh`（打一发并把结果压成一行 `H2H_RESULT {...}`）、`.github/workflows/h2h-*.yml`（矩阵）。

合计 **142 个独立出口 IP、284 发成对请求**。

## 第一轮：真实 DSH 形态（24 IP / 48 发）

载荷四种：`plain`（无工具）、`real_dsh`（read/edit/pwsh/web_search，真实 DSH 描述 + 真实参数 + stream）、`multi45`（45 工具）、`multi3`（45 工具三轮）。

| 形态 | zen-direct | opencode2api |
|---|---|---|
| plain | 6/6 200 | 6/6 200 |
| multi45 | 6/6 200 | 6/6 200 |
| multi3 | 6/6 200 | 6/6 200 |
| **real_dsh** | **6/6 200（SSE）** | **0/6 429** |

opencode2api 在 real_dsh 上拿到的是 `FreeUsageLimitError`，而且**它在首槽位上也一样 429**（3 次），同一 IP 上后打的 zen 反而是 200。所以不是额度，是这份 body 被拒。

第一轮结论：DSH 的工具**顶层描述文本**是硬变量。zen 会脱敏，opencode2api 一个字节都不改。

## 第二轮：拆描述 / 拆流式（28 IP / 56 发）

| 形态（工具数 / 描述 / 参数 / stream） | zen | o2a |
|---|---|---|
| `rd_stream` 4 / 真实 / 真实 / T | 4/4 200 | 0/4 429 |
| `rd_nostream` 4 / 真实 / 真实 / F | 4/4 200 | 0/4 429 |
| `rd1_read` 1(read) / 真实 / 真实 / T | 0/4 429 | 0/4 429 |
| `rd1_read_clean` 1(read) / 中性 / 真实 / T | **0/4 429** | 4/4 200 |
| `rd1_edit` 1(edit) / 真实 / 真实 / T | 0/4 429 | 0/4 429 |
| `rd_stream_clean` 4 / 中性 / 真实 / T | 4/4 200 | 4/4 200 |
| `rd_stream_nodesc` 4 / 无 / 真实 / T | 4/4 200 | 4/4 200 |

两条结论：

- **stream 与否无关**（`rd_stream` 与 `rd_nostream` 同结果）。
- **描述文本有决定性**：中性描述或无描述 → 全 200；真实 DSH 描述 → 全 429。opencode2api 在这一点上 16 发全灭、12 发全活，没有任何噪声。
- 但 zen 也在 `rd1_read_clean` 上 0/4，说明 zen 自己还有第二个洞——见第四轮。

## 第三轮：官方名子集（24 IP / 48 发）—— 排除"官方名子集本身就是罪"

| 形态（官方工具数，中性描述，通用参数，stream=F） | zen | o2a |
|---|---|---|
| 1（read） | 4/4 200 | 4/4 200 |
| 2（read+edit） | 4/4 200 | 4/4 200 |
| 2（read+pwsh） | 4/4 200 | 4/4 200 |
| 4 | 4/4 200 | 4/4 200 |
| 13 | 4/4 200 | 4/4 200 |
| 14（完整官方集） | 4/4 200 | 4/4 200 |

所以"声明了官方工具名"本身不会被拒。

## 第四轮：参数级描述（24 IP / 48 发）—— 找到 zen 的洞

| 形态（工具 / 参数结构 / stream） | zen | o2a |
|---|---|---|
| `q1_st_realp` 1(read) / **真实 DSH 参数** / T | **0/4 429** | 4/4 200 |
| `q1_st_genp` 1(read) / 通用参数 / T | 4/4 200 | 4/4 200 |
| `q1_ns_realp` 1(read) / **真实 DSH 参数** / F | **0/4 429** | 4/4 200 |
| `q1_ns_genp` 1(read) / 通用参数 / F | 4/4 200 | 3/4 200 |
| `q4_st_realp` 4(官方) / 真实 DSH 参数 / T | **0/4 429** | 2/4 200 |
| `q4_st_genp` 4(官方) / 通用参数 / T | 4/4 200 | 2/4 200 |

变量是**参数对象里的属性级 description**，不是 stream，也不是工具数量。触发文本：

```
"Path to edit, resolved by the filesystem backend"
```

zen 的脱敏只处理工具顶层描述，不碰 `parameters.properties.*.description`；而这条请求又走了"官方工具集原样透传"分支——那条分支按设计什么都不改，于是这行文本带着官方名 `read` 一起出站，上游判定"自称官方、schema 却不是官方原文"。

根因在 `toolshape.go`：

```go
// 旧：官方名子集也算"官方原样"
if len(ranked) > 0 && allOfficialToolNames(ranked, protocol) {
    return renames, false   // 整份透传
}
```

`allOfficialToolNames` 对 `{read}` 这种子集返回真，于是 1 个官方工具就触发全量透传。opencode2api 之所以躲过，是因为它无条件把客户端工具改名成 `mcp_<digest>`，从不对上游做"官方声明"。

## 第五轮：修复验证（28 IP / 56 发）

v0.9.38 把透传条件从 `allOfficialToolNames` 收紧为 `completeOfficialToolset`（必须**恰好**是完整官方集），子集一律走改名 + 补全官方集。

| 形态 | v0.9.37 | v0.9.38 |
|---|---|---|
| `q1_st_realp` | 0/4 | **4/4 200** |
| `q1_ns_realp` | 0/4 | **4/4 200** |
| `q4_st_realp` | 0/4 | **4/4 200** |
| `rd1_read` | 0/4 | **4/4 200** |
| `real_dsh` | 6/6 | 4/4 200（无回归） |
| `multi45` | 6/6 | 4/4 200（无回归） |
| `r14_official_clean`（完整官方集） | 4/4 | 4/4 200（仍走透传） |

zen 合计 **28/28 200**。

## 第五轮里 opencode2api 突然 0/28 —— 另一个独立的坑

第五轮 opencode2api 在全部 28 个新出口上 429，连第一轮 12/12 通过的 `multi45` 也 429。同样的字节、同样的 runner、同样的直连出口，只换了时间。

补测（6 个新出口，最朴素的 `plain`）：zen 6/6 200，opencode2api 0/6 429，`Retry-After: 78735`（≈21.9 小时），首包 136 ms 就拒。

隔离手段：从同一个 v1.4.8 源码构建第二个二进制，**只改一个常量** `internal/httpx/client.go` 的 `v2ClientVersion`（2.1.1 → 2.3.0），其余字节不变。

| 二进制（同 IP、同请求、顺序对调，8 个出口） | 结果 |
|---|---|
| opencode2api v1.4.8 原版，UA 版本 2.1.1 | **0/8 429** |
| 同一份源码，UA 版本 2.3.0 | **8/8 200** |

其中 4 发是"改版先打"、4 发是"原版先打"，两边首槽位都各占 4 次——所以不是额度，是 UA 版本。

即 2026-11-17 02:17Z ~ 02:28Z 之间，上游免费层把客户端版本下限抬过了 2.1.1。opencode2api v1.4.8 把版本号硬编码，直接全灭；zen-direct 启动时从 npm 拉 latest，跟着升到 2.3.0，毫发无伤。

这条顺带解释了为什么第一轮 real_dsh 的 0/6 不是 UA 造成的：同一轮里同一 UA 下 `multi45` 12/12 全 200。两个变量互相独立。

## 结论

1. **工具顶层描述文本是硬指纹**。真实 DSH 描述出站必 429（opencode2api 16/16 全灭），中性化后必 200（12/12 全活）。zen 的脱敏是承重的。
2. **参数级描述 + 官方工具名是第二个指纹**。子集透传时 zen 会漏出 `"Path to edit, resolved by the filesystem backend"`，0/8 全灭；改名路径下同一份参数不触发（opencode2api 8/8 全 200 即证）。v0.9.38 收紧透传条件后修复。
3. **客户端 UA 版本是第三个、且会随上游发版跳变的变量**。硬编码 = 定时炸弹；从 npm 跟版 = 自动续命。
4. 三个变量互相独立，必须分开测。混在一起测就会得出"工具数太多"这类错误结论（见 `docs/opencode2api-v148-comparison.md` 的记录）。

## 复现

```bash
# 仓库：2061969365/zen-warp-cli
gh workflow run head2head   --ref master   # 第一轮：四形态 × 双顺序 × 3
gh workflow run h2h-desc    --ref master   # 第二轮：描述/流式拆分
gh workflow run h2h-subset  --ref master   # 第三轮：官方名子集
gh workflow run h2h-params  --ref master   # 第四轮：参数级描述
gh workflow run h2h-fix     --ref master   # 第五轮：修复验证
gh workflow run h2h-release --ref master   # 发版闸门
```

结果全部是 `H2H_RESULT {...}` 单行 JSON，聚合脚本在本机 `_cmp/analyze.py`。

## 附：本轮落地的代码

| 版本 | 变更 | 依据 |
|---|---|---|
| v0.9.38 | 透传条件 `allOfficialToolNames` → `completeOfficialToolset` | 第四/五轮 |
| v0.9.38 | 新增 `toolshape_official_subset_test.go`，改写 `TestOfficialToolsetPassesThroughVerbatim` 为完整集语义 | 同上 |
| v0.9.39 | `uaDefaultVersion` 2.2.0 → 2.3.0（仅兜底；启动仍从 npm 刷新，且只升不降） | UA 隔离实验 | 
| v0.9.39 | 新增 `TestPrimeUAVersionNeverDowngrades` | 同上 |
