# 工具名约束：实测结论（2026-11-16）

这一轮抓包与 A/B 测试的目的：定位 `FreeUsageLimitError` 429 的真实触发条件，
并判断「工具名伪装」是否可行。结论是**不可行**，本文记录全部实测数据，
避免后续重复尝试同一方向。

## 环境

- 官方 CLI：`@opencode/cli@2.2.5`（本机与 GitHub Actions WARP runner 都已安装）
- 上游：`https://opencode.ai/inference/v1/chat/completions`
- 探针模型：`deepseek-v4.1-flash-free`
- 抓包方法：CLI 的 baseURL 指向本地明文 sink，得到原始请求头/体；
  raw TCP relay + ClientHello 解析得到 TLS 指纹。

## 决定性对照

| 出站工具形态 | 结果 |
|---|---|
| 14 个精确官方名（描述可自定义） | **200**，模型正确返回 tool_calls |
| 14 个官方名 + 1 个 `pwsh` | **429** |
| 45 个 DSH 工具原名 | 429 |
| 45 个 DSH 工具 + PR#179 零宽空格混淆 | 429 |
| `clint_<digest>` 改名 + 官方集 | 429 |
| 12 官方工具桩（真实 prompt） | **200**，返回 `shell{"command":"ls"}` |
| 客户端 `read` 真定义 + 13 官方桩 | **200**，返回 `read{"path":"a.txt"}` |
| 3 非官方名 + 14 官方桩 | 429 |
| 14 官方名，重复名字 | 400 `tool names must be unique` |
| `read_1` / `read1` / `Read` 变体 | 429 |

## 结论

1. **唯一变量是工具名**，与描述文本、参数 schema、UA 无关（UA 只影响另一层门禁）。
2. 上游只接受**精确官方名**：`edit/execute/glob/grep/question/read/shell/skill/
   subagent/webfetch/websearch/write/patch/experimental_image_generate`，共 14 个。
3. 名字必须唯一。所以 14 个名字是**硬容量上限**。
4. 零宽空格混淆（opencode2api PR#179）对此无效——它防的是描述指纹，
   而这里的门禁在工具名上。
5. 因此 45 个工具无法全部伪装通过；强行映射到 14 个名字会导致多个工具合并、
   语义与 schema 冲突，属于功能损坏，不是解决方案。

## 已落地修复

- 恢复 12 官方工具集透传路径（不再把所有工具改成 `clint_*`）。
- ClientHello 补齐 `ed25519` / `ecdsa_secp521r1_sha512`，JA3 已与官方 CLI 一致。
- 第三方工具仍保留原有改名路径；官方形态工具整份原样透传。

## 可行的产品方向

- DSH 侧把工具面收敛到官方 14 个语义等价工具
  （`read/write/edit/shell/glob/grep/skill/subagent/webfetch/websearch`）。
- 其余工具通过 MCP 挂给官方 OpenCode 客户端，由官方客户端承担身份。
- 或更换为不限制客户端类型的服务商，现有网关链路无需改动。
