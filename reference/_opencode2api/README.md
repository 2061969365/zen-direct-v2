# reference/ — 只读对照材料，不参与编译

这个目录里是 **opencode2api** 项目的源码副本，用来对照我们自己的实现，排查差异时看。

- 上游项目：https://github.com/jasonxu114514/opencode2api
- 抓取版本：**v1.4.5**（tag `db30ff3`，2026-10-26 发布）
- 许可证：**GPL-3.0-or-later**，全文见同目录 `LICENSE`

## 为什么不参与编译

1. zen-direct 自身不采用 GPL；把 GPL 代码链接进二进制会传染许可证。
2. Go 工具链忽略以 `_` 开头的目录，所以 `go build ./...`、`go vet ./...`、
   `go test ./...` 都不会碰这里。

## 使用边界

- 只用于阅读、比对协议事实（cipher/扩展顺序、头顺序、project id 形状、解压行为）。
- **不要**从这里 import 任何代码，**不要**把文件移到参与编译的目录。
- 分发本仓库时，此目录下的文件仍按 GPL-3.0-or-later 授权，与 zen-direct 其余代码无关。

## 对照要点

| 事实 | 我们的位置 | 参考位置 |
|---|---|---|
| Bun ClientHello（17 cipher / 13 扩展 / 仅 h1） | `bun_hello.go` | `httpx/bun_dial.go` |
| fetch 头顺序与大小写 | `bun_wire.go` | `httpx/bun.go` |
| 连接池 / 复用 / 解压 | `bun_transport.go` | `httpx/bun.go` |
| 代理（CONNECT / SOCKS5） | `bun_transport.go` | `httpx/bun_dial.go` |
| project id 形状（40 位 hex / global / 哈希） | `project.go` | `identity/request.go`、`gateway/upstream.go`（未收录，见下） |
| 会话 id / 子会话 / 项目信号 | `session_pool.go`、`main.go` | `identity/request.go` |
| 响应工具名折叠（精确 → 前缀 → 大小写 → 分隔符） | `alias.go`（我们是 `zd_` 前缀方案，未照搬） | `protocol/tools.go`、`protocol/response.go`、`protocol/stream.go` |

## 本版（v1.4.5）收录范围

`internal/httpx` 是完整副本；`identity/request.go` 与 `protocol/{tools,response,stream}.go`
是这次 v1.4.5 真正改动的文件，收进来做校对。

`internal/gateway/upstream.go`（1500+ 行，含 v1/v2 lane 头渲染与 `projectID()`）
没有整份收录；需要时按下面的命令临时取：

```powershell
git -C F:\worker\_opencode2api show v1.4.5:internal/gateway/upstream.go
```

## v1.4.5 相对我们上次抓取（b8b1454）的变化

| 范围 | 变化 | 我们是否跟进 |
|---|---|---|
| `internal/httpx` | **零改动** | 无需动 |
| 免费层 lane | 只走 v2 `/inference`，v1 `/zen` 回退废弃 | 我们本来就只走 v2，无需动 |
| project id | 客户端未声明时，**每次请求现签 40 位 hex** | 已跟进：`project.go` |
| 工具名 | 响应侧按 精确 → `clint_` 前缀 → 大小写 → 分隔符 折叠 | 未照搬（我们无 `clint_` 改名；见 `alias.go`） |
| warp 池 | 预加载、注册节流、锁内定时器 | 不适用（我们不用 WARP） |
