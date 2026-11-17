# 阶段性总结 — 2026-11-17

本轮的主线：把"上游免费层为什么 429"从猜测变成**成对可复现的实测**，并顺手把对手 opencode2api v1.4.8 拉到同一个出口上对打。

---

## 一、结论（三条，互相独立）

### 1. 工具顶层描述文本是硬指纹 —— 这条我们做对了，对手没做

同一出口 IP、同一份 body、同一 UA，只有工具描述不同：

| 载荷 | zen-direct | opencode2api v1.4.8 |
|---|---|---|
| 真实 DSH 描述（read/edit/pwsh/web_search） | 200 | **429 ×16** |
| 中性描述 | 200 | 200 ×12 |
| 完全不带 description | 200 | 200 ×4 |

对手全仓没有客户端描述改写逻辑，一个字节都不改。我们把 `bannedDescriptionPhrases` 命中的短语换掉，所以过。

**这条是承重的**：2026-11-17 之前误判过一次（v0.9.36 关掉脱敏 → 生产双节点 429），根因是用了一份"出口额度已耗尽"的无效 A/B 样本。现在的结论来自 24 个独立出口的成对数据。

### 2. 参数级描述 + 官方工具名是第二个指纹 —— 这条我们原来有洞，已修

只声明**官方工具名子集**（例如只有 `read`）时，`allOfficialToolNames` 返回真，于是整份 payload 走"官方集原样透传"，把 DSH 的**参数属性描述**漏了出去：

```
"Path to edit, resolved by the filesystem backend"
```

上游判定"自称官方、schema 却不是官方原文" → zen **0/8 全 429**；同一份参数交给对手的改名路径 → **8/8 全 200**。这一格是我们输的。

修复（v0.9.38）：透传条件收紧为 `completeOfficialToolset`——必须**恰好**是完整官方集，子集一律走"改名 + 补全官方集"。修完 28/28 全绿。

### 3. 客户端 UA 版本是第三个变量，且会随上游发版跳变 —— 对手被这条打死

2026-11-17 02:17Z ~ 02:28Z 之间，上游把客户端版本下限抬过了 2.1.1。

| 二进制（同 IP、同请求、顺序对调，8 个出口） | 结果 |
|---|---|
| opencode2api v1.4.8 原版（`v2ClientVersion = "2.1.1"` 硬编码） | **0/8 429** |
| 同一份源码，只把这个常量改成 `2.3.0` | **8/8 200** |

截止之后原版在 **42 个全新出口上 0/42**，连 `plain` 都 429，`Retry-After` ≈ 22 小时。

我们：启动时从 npm 拉 latest（`primeUAVersion`，只升不降），实测跟着升到 2.3.0，毫发无伤。v0.9.39 又把硬编码兜底从 2.2.0 抬到 2.3.0，避免"npm 拉不到时踩旧版本"。

**对手只改 UA 也不够**：改完 UA 后中性描述 2/2 过，真实 DSH 描述仍 **0/8 全灭**。两个坑得一起填。

---

## 二、数据

**162 个独立 GitHub Actions 出口 IP，324 发成对请求。** 每台 runner = 一个全新出口；同 job 内两个二进制打逐字节相同的请求；`zen_first` / `o2a_first` 各占一半抵消额度顺序效应；另单独统计首槽位（全新出口的第一发）。

| 轮次 | run | 出口 IP | 发数 | 内容 | 结果 |
|---|---|---|---|---|---|
| R1 四形态 | 44000876261 | 24 | 48 | plain / real_dsh / multi45 / multi3 | zen 24/24；o2a 18/24（real_dsh 0/6） |
| R2 描述/流式拆分 | 44001190736 | 28 | 56 | 7 形态 × 双顺序 × 2 | 描述是真凶，stream 无关；暴露 zen 单工具洞 |
| R3 官方名子集 | 44001578606 | 24 | 48 | 1/2/4/13/14 个官方名 | 全 200，排除"官方名子集本身是罪" |
| R4 参数级描述 | 44001890014 | 24 | 48 | 参数结构 × stream 2×2 | 锁定参数属性描述，zen 0/8 |
| R5 修复验证 | 44003165383 | 28 | 56 | v0.9.38 | zen **28/28**；o2a 0/28（UA 已过期） |
| o2a 体检 | 44003839629 | 6 | 12 | plain | zen 6/6；o2a 0/6 |
| UA 隔离 | 44004394558 | 8 | 16 | 只改 UA 常量 | 2.1.1 → 0/8；2.3.0 → 8/8 |
| 发版闸门 | 44004954158 | 10 | 20 | v0.9.39 五形态 | zen **10/10** |
| 改 UA 后仍失败 | 44006938394 | 10 | 20 | 真实 DSH 描述 | 中性 2/2；真实描述 0/8 |

生产验证：v0.9.39 上线后双节点（后扩到 5 台）四形态 × 3 轮 = **24/24 全 200**。

---

## 三、代码与发布

| 位置 | 提交 | 内容 |
|---|---|---|
| `F:\worker\zen-direct-v2` | `e1b1e6d` | `completeOfficialToolset` 收紧透传；`uaDefaultVersion` 2.2.0→2.3.0；新增 `toolshape_official_subset_test.go`、`TestPrimeUAVersionNeverDowngrades`；改写 `TestOfficialToolsetPassesThroughVerbatim` 为完整集语义；文档 |
| `F:\worker\zen-console` | `8c7e623` | 三套模板二进制 → v0.9.39（sha `565586108D39D0F3`），`TEMPLATE_VERSION` → `.28`（本地仓库无 remote） |

版本沿革：

| 版本 | 变更 | 依据 |
|---|---|---|
| v0.9.37 | 恢复描述脱敏为默认 | 生产 A/B（脱敏开 200 / 关 429） |
| v0.9.38 | 透传条件收紧为完整官方集 | R4 + R5 |
| v0.9.39 | UA 兜底抬到 2.3.0 | UA 隔离实验 |

模板 `.28` 已推全部 11 台节点（op 333）。

---

## 四、当前生产状态

启用中的 5 台全部在 `.28` / v0.9.39：

| 节点 | 域名 | health |
|---|---|---|
| 1 | zen.uflash.ccwu.cc | 200 |
| 10 | zen3.uflash.ccwu.cc | 200 |
| 11 | zen4.uflash.ccwu.cc | 200 |
| 16 | zen10.uflash.ccwu.cc | 200 |
| 17 | zen11.uflash.ccwu.cc | 200 |

---

## 五、文件地图

```
F:\worker\zen-direct-v2\              网关源码（v0.9.39，提交 e1b1e6d）
  toolshape.go                        工具整形：改名/补全/描述脱敏/透传门槛
  toolshape_official_subset_test.go   子集不得透传的回归测试
  ua.go                               客户端版本：npm 跟版 + 只升不降
  texttool.go                         DSML 文本工具调用恢复
  docs\head2head-zen-vs-opencode2api.md   对跑完整数据（逐轮表格）
  docs\stage-summary-2026-11-17.md        本文件

F:\worker\zen-console\                控制面板 + 三套模板（提交 8c7e623）
  template{,-warp,-vpngate}\          模板 .28，bin\zen-direct = v0.9.39

F:\worker\opencode2api-v148\          对手源码 clone（v1.4.8 @ 159d7fd）
F:\worker\opencode2iori\              另一 fork（DSML 恢复的来源）

F:\worker\_cmp\                       本轮对跑工作区
  analyze.py                          把 H2H_RESULT 行聚合成表
  prod-sweep.ps1                      生产四形态扫描
  prod-sweep-v0939.json               生产 24/24 的原始记录
  o2a-v148-linux-amd64                对手原版（linux/amd64）
  o2a-ua230                           对手只改 UA 常量后的诊断版
  zen-direct-v0938 / v0939            本网关两版

F:\worker\_cmp\repo\                  测试仓库工作副本（2061969365/zen-warp-cli）
  bin\                                六个二进制（含两版对手）
  payloads\                           23 个载荷形态
  scripts\boot.sh / h2h.sh / ...      起网关 + 打一发 + 压成单行 JSON
  .github\workflows\h2h-*.yml         九套矩阵
```

---

## 六、遗留项

1. **node 12（`xia35480/zen5`）的 run `44007881073`** 卡在 GitHub 侧 `queued`，`cancel` / `force-cancel` 回 409「尚未排队」，`DELETE` 回 403。等它真进入运行态或自行过期后再收。
2. **node 3 / 14 面板里挂着 stale `in_progress`**（run `42845950543` / `43604136547`），是推送前就存在的僵尸记录，与本次无关，需要时可以清一次。
3. **`template-warp` 下两个未跟踪文件**（`.github/workflows/cli-capture.yml`、`scripts/sink.py`）是早前 CLI 抓包实验的产物，会随模板一起推到节点，尚未决定是否入库。
4. **对手的结论只对"当时的 UA 门槛"成立**：他们改完 UA 之后描述问题依旧，但若上游再抬门槛，这组数据的时间戳需要重新标定。

---

## 七、方法上的教训（写给下一次的自己）

- **A/B 必须同节点、同请求、几分钟内、只改一个变量**。出口额度会随时间自行恢复，跨时段的两组数据不可比。
- **先确认出口健康，再下结论**。v0.9.36 的误判就是拿了一份"两边都 429"的样本。
- **首槽位单独统计**。免费层按出口记账，先打的那一方天然占便宜；顺序对调后结论翻转就说明看到的是额度不是形态。
- **不要在没跑过真实上游的情况下发版**。v0.9.38 / v0.9.39 都是先在 Actions 过闸门（28/28、10/10）才推生产。
