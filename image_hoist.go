package main

import (
	"fmt"
	"os"
	"strings"
)

// shapeOn 控制请求形状诊断日志（只打角色/块类型/字节数，不打内容）。
func shapeOn() bool { return os.Getenv("DIRECT_DUMP_SHAPE") == "1" }

// describeShape 渲染出站 messages 的形状，用于排查图片到底落在哪条消息里。
func describeShape(u map[string]any) string {
	var b strings.Builder
	for _, raw := range asArr(u["messages"]) {
		m := asMap(raw)
		b.WriteString(asStr(m["role"]))
		b.WriteString("[")
		switch c := m["content"].(type) {
		case string:
			fmt.Fprintf(&b, "text:%d", len(c))
		case []any:
			for i, p := range c {
				if i > 0 {
					b.WriteString(",")
				}
				pm := asMap(p)
				switch t := asStr(pm["type"]); t {
				case "text":
					fmt.Fprintf(&b, "text:%d", len(asStr(pm["text"])))
				case "image_url":
					fmt.Fprintf(&b, "image:%d", len(jsFirstString(jsStringAt(pm, "image_url", "url"), jsStringAt(pm, "image_url"))))
				default:
					b.WriteString(t)
				}
			}
		}
		b.WriteString("] ")
	}
	return b.String()
}

// hoistChatToolImages 把 Chat tool 消息里的图片抬成独立 user 消息。
//
// Chat 协议的 tool 消息只能装文本：图片留在 tool 消息里上游不会按图片处理。
// 这里按 opencode2api 的做法重写 —— tool 消息只留文本（图片位置写
// [tool_image call_id=.. index=N] 标记），图片放进紧跟在 tool 消息组之后的
// 一条 user 消息，顺序保持 assistant(tool_calls) -> tool... -> user(images)。
// 没有图片的请求原样不动。
func hoistChatToolImages(in map[string]any) error {
	msgs := asArr(in["messages"])
	if len(msgs) == 0 {
		return nil
	}
	out := make([]any, 0, len(msgs)+1)
	var attachments []any
	flush := func() {
		if len(attachments) > 0 {
			out = append(out, attachments...)
			attachments = nil
		}
	}
	for _, raw := range msgs {
		m := asMap(raw)
		if asStr(m["role"]) != "tool" {
			flush()
			out = append(out, raw)
			continue
		}
		content, ok := m["content"].([]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		parsed, err := decodeChatToolImages(content)
		if err != nil {
			return err
		}
		blocks, ok := parsed.([]bridgeBlock)
		if !ok {
			out = append(out, raw)
			continue
		}
		callID := asStr(m["tool_call_id"])
		text, images := chatToolImages(bridgeBlock{Result: blocks, CallID: callID})
		if len(images) == 0 {
			out = append(out, raw)
			continue
		}
		rewritten := make(map[string]any, len(m))
		for k, v := range m {
			rewritten[k] = v
		}
		rewritten["content"] = text
		out = append(out, rewritten)
		attachments = append(attachments, map[string]any{
			"role":    "user",
			"content": encodeChatBlocks(images),
		})
	}
	flush()
	in["messages"] = out
	return nil
}
