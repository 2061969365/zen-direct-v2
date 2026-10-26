# reference/ — 只读对照材料，不参与编译

这个目录里是 **opencode2api** 项目的 `internal/httpx` 源码副本，用来对照我们自己的实现（`bun_hello.go` / `bun_wire.go` / `bun_transport.go`），排查差异时看。

- 上游项目：https://github.com/jasonxu114514/opencode2api
- 抓取版本：commit `b8b1454`（2026-10-25）
- 许可证：**GPL-3.0-or-later**，全文见同目录 `LICENSE`

## 为什么不参与编译

1. zen-direct 自身不采用 GPL；把 GPL 代码链接进二进制会传染许可证。
2. Go 工具链忽略以 `_` 开头的目录（以及 `reference/` 下的包），所以
   `go build ./...`、`go vet ./...`、`go test ./...` 都不会碰这里。

## 使用边界

- 只用于阅读、比对协议事实（cipher/扩展顺序、头顺序、解压行为）。
- **不要**从这里 import 任何代码，**不要**把文件移到参与编译的目录。
- 分发本仓库时，此目录下的文件仍按 GPL-3.0-or-later 授权，与 zen-direct 其余代码无关。

## 对照要点

| 事实 | 我们的位置 | 参考位置 |
|---|---|---|
| Bun ClientHello（17 cipher / 13 扩展 / 仅 h1） | `bun_hello.go` | `httpx/bun_dial.go:31` |
| fetch 头顺序与大小写 | `bun_wire.go` | `httpx/bun.go:486-587` |
| 连接池 / 复用 / 解压 | `bun_transport.go` | `httpx/bun.go:29-480` |
| 代理（CONNECT / SOCKS5） | `bun_transport.go` | `httpx/bun_dial.go:115-313` |
