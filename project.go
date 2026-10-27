package main

// project.go: 上游 project id 的渲染。
//
// 背景（opencode2api v1.4.5，2026-10-26 发布，实测结论）：
// 免费层按「project id + 出口 IP」双维度记额度。旧做法给所有没声明 project
// 的调用方发同一个常量，等于所有人共用一个桶——热门出口上那个桶早被烧穿，
// 于是同一份请求换个出口能过、原出口一直 429。
//
// 对策：每次请求现签一个 40 位小写 hex。官方 CLI 发的是 git root commit，
// 正好是这形状；上游只校验形状，不校验值。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// gitCommitPattern 是官方 CLI 发的 project id 形状：git root commit，40 位小写 hex。
var gitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// upstreamProjectID 把 project 渲染成上游要的形状：
//
//	"" / "global"   -> 现签一个 40 位 hex（默认；"global" 是公共桶，不共用）
//	40 位小写 hex   -> 原样（真实 git root commit）
//	其余            -> sha256 成 40 位 hex，稳定映射同一个上游 project
//
// 之所以要哈希而不是原样发：上游 v2 门禁拒绝 prj_* 这类带前缀的 project id
// （403 FreeTierError），形状不对的值同样有风险。
func upstreamProjectID(project string) string {
	switch {
	case project == "":
		return randomProjectID()
	case project == "global":
		// "global" 不是官方 CLI 的项目身份，只是「没有仓库」的占位，上游把它
		// 当成一个所有人共用的桶。这里改签新号，避免继承被烧穿的公共桶。
		return randomProjectID()
	case gitCommitPattern.MatchString(project):
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
