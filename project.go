package main

// project.go: 上游 project id 的渲染。
//
// 背景（opencode2api v1.4.5，2026-10-26 发布，实测结论）：
// 免费层按「project id + 出口 IP」双维度记额度。旧做法给所有没声明 project
// 的调用方发同一个常量，等于所有人共用一个桶——热门出口上那个桶早被烧穿，
// 于是同一份请求换个出口能过、原出口一直 429。
//
// 对策：没有 project 可用时，每次请求现签一个 40 位小写 hex。官方 CLI 发的
// 是 git root commit，正好是这形状。
//
// 注意：是否现签取决于「客户端声明了没有 / 运维配了没有」——
//   - 客户端声明了 X-Opencode-Project：按形状归一后发送（保留客户端身份）
//   - OPENCODE_ZEN_PROJECT 显式配置：原样发送（运维的明确选择，可回退 global）
//   - 两者都没有：现签一个新号

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// gitCommitPattern 是官方 CLI 发的 project id 形状：git root commit，40 位小写 hex。
var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// renderedProjectID 渲染客户端声明的 project：
//
//	40 位小写 hex 或 "global" -> 原样（官方 CLI 的两种真实取值）
//	其余（如 prj_*）          -> sha256 成 40 位 hex，稳定映射同一个上游 project
//
// 之所以要哈希而不是原样发：上游 v2 门禁拒绝 prj_* 这类带前缀的 project id
// （403 FreeTierError），形状不对的值同样有风险。
func renderedProjectID(project string) string {
	if project == "" || project == "global" || gitCommitPattern.MatchString(project) {
		return project
	}
	sum := sha256.Sum256([]byte("opencode-project\x00" + project))
	return hex.EncodeToString(sum[:20])
}

// randomProjectID 现签一个新 project id（40 位小写 hex）。
// crypto/rand 失败时 panic：没有随机源就没有可用的身份，硬着头皮发常量
// 只会把请求打进公共桶。
func randomProjectID() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
