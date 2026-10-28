package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// bridgeBlock 是跨协议转换用的中间块。这里只保留图片/文本路径需要的字段。
type bridgeBlock struct {
	Kind      string
	Text      string
	URL       string
	MediaType string
	Data      string
	FileID    string
	Filename  string
	ID        string
	Name      string
	Arguments any
	CallID    string
	Result    any
}

// jsStringAt 取 path 上的字符串，缺失或类型不符返回 ""。
func jsStringAt(object map[string]any, path ...string) string {
	var cur any = object
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	s, _ := cur.(string)
	return s
}

// jsMapAt 取 path 上的对象，缺失或类型不符返回 nil。
func jsMapAt(object map[string]any, path ...string) map[string]any {
	var cur any = object
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[key]
	}
	m, _ := cur.(map[string]any)
	return m
}

// jsFirstString 返回第一个非空（去空白后）字符串。
func jsFirstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// decodeOpenAIBlocksChecked 解析 OpenAI 风格 content（字符串、单块或块数组）。
func decodeOpenAIBlocksChecked(value any) ([]bridgeBlock, error) {
	switch value := value.(type) {
	case string:
		if value == "" {
			return nil, nil
		}
		return []bridgeBlock{{Kind: "text", Text: value}}, nil
	case map[string]any:
		return decodeOpenAIBlocksChecked([]any{value})
	case []any:
		blocks := make([]bridgeBlock, 0, len(value))
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("OpenAI content block must be an object")
			}
			switch jsStringAt(part, "type") {
			case "text", "input_text", "output_text":
				blocks = append(blocks, bridgeBlock{Kind: "text", Text: jsStringAt(part, "text")})
			case "image_url":
				blocks = append(blocks, bridgeBlock{Kind: "image", URL: jsFirstString(jsStringAt(part, "image_url", "url"), jsStringAt(part, "image_url"))})
			case "input_image":
				blocks = append(blocks, bridgeBlock{Kind: "image", URL: jsStringAt(part, "image_url")})
			case "file", "input_file":
				file := jsMapAt(part, "file")
				blocks = append(blocks, bridgeBlock{
					Kind:      "file",
					FileID:    jsFirstString(jsStringAt(part, "file_id"), jsStringAt(file, "file_id")),
					Filename:  jsFirstString(jsStringAt(part, "filename"), jsStringAt(file, "filename")),
					MediaType: jsFirstString(jsStringAt(part, "media_type"), jsStringAt(file, "media_type")),
					Data:      jsFirstString(jsStringAt(part, "file_data"), jsStringAt(file, "file_data")),
				})
			default:
				return nil, fmt.Errorf("unsupported OpenAI content block type %q", jsStringAt(part, "type"))
			}
		}
		return blocks, nil
	default:
		if value == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported OpenAI content value")
	}
}

// decodeChatToolImages 解析 Chat tool 消息里带图片的 content。
// 无图片时原样返回；有图片时每个块必须是 text 或 image_url，且 data URI 合法。
func decodeChatToolImages(value any) (any, error) {
	parts, ok := value.([]any)
	if !ok {
		return value, nil
	}
	hasImage := false
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		hasImage = hasImage || jsStringAt(part, "type") == "image_url"
	}
	if !hasImage {
		return value, nil
	}
	for i, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content[%d] must be an object", i)
		}
		switch jsStringAt(part, "type") {
		case "text", "input_text", "output_text":
			if _, ok := part["text"].(string); !ok {
				return nil, fmt.Errorf("content[%d] requires text", i)
			}
		case "image_url":
			if jsFirstString(jsStringAt(part, "image_url", "url"), jsStringAt(part, "image_url")) == "" || part["file_id"] != nil {
				return nil, fmt.Errorf("content[%d] requires an image URL without a file reference", i)
			}
		default:
			return nil, fmt.Errorf("content[%d] contains an unsupported block alongside tool images", i)
		}
	}
	blocks, err := decodeOpenAIBlocksChecked(value)
	if err != nil {
		return nil, err
	}
	for i := range blocks {
		block := &blocks[i]
		if block.Kind != "image" || !strings.HasPrefix(block.URL, "data:") {
			continue
		}
		header, data, ok := strings.Cut(strings.TrimPrefix(block.URL, "data:"), ",")
		media, isBase64 := strings.CutSuffix(header, ";base64")
		if !ok || !isBase64 || !strings.HasPrefix(media, "image/") || data == "" {
			return nil, fmt.Errorf("content[%d] has an invalid image data URI", i)
		}
		block.URL, block.MediaType, block.Data = "", media, data
	}
	return blocks, nil
}

// bridgeArgumentsJSON 把参数编码成 JSON；nil 视为 {}。
func bridgeArgumentsJSON(block bridgeBlock) string {
	if block.Arguments == nil {
		return "{}"
	}
	data, err := json.Marshal(block.Arguments)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// chatToolImages 把 tool 结果拆成 Chat tool 消息能装的文本，以及必须放进
// 独立 user 消息的图片块。每张图在文本里用 [tool_image call_id=.. index=N]
// 标记，图片列表以同样的标记文本块开头，保证两者仍然关联。
func chatToolImages(result bridgeBlock) (string, []bridgeBlock) {
	parts, ok := result.Result.([]bridgeBlock)
	if !ok {
		return "", nil
	}
	var text strings.Builder
	var images []bridgeBlock
	n := 0
	for _, part := range parts {
		if part.Kind == "text" {
			text.WriteString(part.Text)
			continue
		}
		n++
		marker := fmt.Sprintf("[tool_image call_id=%s index=%d]", bridgeArgumentsJSON(bridgeBlock{Arguments: result.CallID}), n)
		text.WriteString("\n" + marker + "\n")
		images = append(images, bridgeBlock{Kind: "text", Text: marker}, part)
	}
	return text.String(), images
}

// encodeChatBlocks 把块渲染成 Chat content：单个文本块直接成字符串，
// 其余成 text / image_url 数组。
func encodeChatBlocks(blocks []bridgeBlock) any {
	if len(blocks) == 1 && blocks[0].Kind == "text" {
		return blocks[0].Text
	}
	parts := make([]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			url := block.URL
			if url == "" && block.Data != "" {
				url = "data:" + block.MediaType + ";base64," + block.Data
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		}
	}
	return parts
}
