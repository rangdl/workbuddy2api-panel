package responses

import (
	"encoding/json"
	"strings"
)

// 工具结果媒体搬迁（对齐 cc-switch tool_media.rs）。
//
// Responses/Anthropic 的 tool output 可能携带结构化媒体块（图片/文件/音频），
// 而 Chat Completions 的 tool 消息是纯文本的。协议桥需要把这些块剥离出来，
// 改以合成 user 消息重新投递，否则严格上游会 400（tool 消息不接受媒体）。

const (
	// toolMediaMovedMarker 媒体被搬离 tool 结果后的替换文本。
	toolMediaMovedMarker = "[tool result media moved to the following user message]"
	// wholeDataURLMinBytes 整个字符串被识别为图片 data URL 的最小字节数
	// （避免把短文本误判为图片）。
	wholeDataURLMinBytes = 8 * 1024
	// maxMediaTraversalDepth 递归遍历 tool output 的最大深度。
	maxMediaTraversalDepth = 32
)

// planToolOutputMedia 分析 tool output，剥离其中的媒体并返回：
//   - toolContent：剥离后的 tool 结果（Chat tool 消息的字符串 content）
//   - mediaParts：收集到的 Chat user 媒体 part（image_url / file / input_audio）
//
// 无媒体时 mediaParts 为 nil，toolContent 与直接序列化等价（零改动）。
func planToolOutputMedia(output any) (string, []any) {
	var media []any
	newOutput, replaced := stripMediaFromValue(output, &media, 0)
	if replaced == 0 {
		return toolOutputString(output), nil
	}
	return toolOutputString(newOutput), media
}

// stripMediaFromValue 递归剥离 value 里的媒体块，返回处理后的值与替换计数。
// map/slice 原地修改，string 返回替换后的新值。depth 超限时停止（防御畸形深嵌套）。
func stripMediaFromValue(value any, media *[]any, depth int) (any, int) {
	if depth > maxMediaTraversalDepth {
		return value, 0
	}
	switch v := value.(type) {
	case string:
		// 整个字符串就是一张图片 data URL → 收集，替换为标记文本。
		if part, ok := wholeStringImageDataURL(v); ok {
			*media = append(*media, part)
			return toolMediaMovedMarker, 1
		}
		// 字符串里嵌 JSON（tool output 常见）：解析后递归，确有替换才回写。
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return value, 0
		}
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) != nil {
			return value, 0
		}
		newParsed, n := stripMediaFromValue(parsed, media, depth+1)
		if n == 0 {
			return value, 0
		}
		raw, err := json.Marshal(newParsed)
		if err != nil {
			return value, 0
		}
		return string(raw), n
	case []any:
		total := 0
		for i := range v {
			nv, n := stripMediaFromValue(v[i], media, depth+1)
			if n > 0 {
				v[i] = nv
			}
			total += n
		}
		return v, total
	case map[string]any:
		// 该对象本身就是媒体块 → 收集，替换为标记块。
		if part, ok := chatMediaPartFromToolPart(v); ok {
			*media = append(*media, part)
			return map[string]any{"type": "text", "text": toolMediaMovedMarker}, 1
		}
		// 否则递归 content 字段。
		if content, ok := v["content"]; ok {
			nv, n := stripMediaFromValue(content, media, depth+1)
			if n > 0 {
				v["content"] = nv
			}
			return v, n
		}
		return v, 0
	default:
		return value, 0
	}
}

// wholeStringImageDataURL 判定整个字符串是否是图片 base64 data URL。
func wholeStringImageDataURL(value string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) < wholeDataURLMinBytes || !isImageBase64DataURL(trimmed) {
		return nil, false
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": trimmed}}, true
}

// isImageBase64DataURL 判定字符串是否为 data:image/...;base64,... 形态。
func isImageBase64DataURL(value string) bool {
	idx := strings.IndexByte(value, ',')
	if idx < 0 {
		return false
	}
	header := strings.ToLower(value[:idx])
	return strings.HasPrefix(header, "data:image/") && strings.HasSuffix(header, ";base64")
}

// chatMediaPartFromToolPart 把 tool output 里的媒体块转成 Chat user content part。
func chatMediaPartFromToolPart(part map[string]any) (map[string]any, bool) {
	switch stringField(part, "type") {
	case "input_image", "image_url":
		if url, ok := normalizedImageURL(part); ok {
			return map[string]any{"type": "image_url", "image_url": url}, true
		}
	case "input_file":
		if f, ok := responsesInputFileToChatFile(part); ok {
			return map[string]any{"type": "file", "file": f}, true
		}
	case "input_audio":
		if a, ok := part["input_audio"]; ok {
			return map[string]any{"type": "input_audio", "input_audio": a}, true
		}
	}
	return nil, false
}

// normalizedImageURL 归一化 image_url 字段（对象或裸字符串）。
func normalizedImageURL(part map[string]any) (map[string]any, bool) {
	img, ok := part["image_url"]
	if !ok {
		return nil, false
	}
	if m, ok := img.(map[string]any); ok {
		return m, true
	}
	if s, ok := img.(string); ok && s != "" {
		return map[string]any{"url": s}, true
	}
	return nil, false
}

// queueToolMedia 把某次 tool 结果的媒体排入待投递队列（前置一条标注文本）。
func queueToolMedia(pending *[]any, callID string, media []any) {
	if len(media) == 0 {
		return
	}
	*pending = append(*pending, map[string]any{
		"type": "text",
		"text": "[media output of tool call " + callID + "]",
	})
	*pending = append(*pending, media...)
}
