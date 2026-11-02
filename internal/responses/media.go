package responses

import (
	"encoding/json"
	"fmt"
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
	// toolMediaAttachedMarker 孤儿结果媒体挂在本条 user 消息内的占位文本。
	toolMediaAttachedMarker = "[tool result media attached as native media]"
	// base64ishMinBytes 疑似残留 base64 负载的最小字节数（媒体搬走后防 token 爆炸）。
	base64ishMinBytes = 16 * 1024
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
	return planToolOutputMediaWithMarker(output, toolMediaMovedMarker)
}

// planToolOutputMediaWithMarker 同 planToolOutputMedia，可指定媒体被剥离后的占位文本
// （孤儿结果把媒体挂在本条 user 消息内，占位要用「attached」而不是「moved」）。
// 媒体搬走后对残余的长 data:/base64 串做裁剪，避免整段 base64 撑爆上下文。
func planToolOutputMediaWithMarker(output any, marker string) (string, []any) {
	var media []any
	newOutput, replaced := stripMediaFromValue(output, &media, marker, 0)
	if replaced == 0 {
		return toolOutputString(output), nil
	}
	newOutput = clampBase64ishStrings(newOutput)
	return toolOutputString(newOutput), media
}

// stripMediaFromValue 递归剥离 value 里的媒体块，返回处理后的值与替换计数。
// map/slice 原地修改，string 返回替换后的新值。depth 超限时停止（防御畸形深嵌套）。
func stripMediaFromValue(value any, media *[]any, marker string, depth int) (any, int) {
	if depth > maxMediaTraversalDepth {
		return value, 0
	}
	switch v := value.(type) {
	case string:
		// 整个字符串就是一张图片 data URL → 收集，替换为标记文本。
		if part, ok := wholeStringImageDataURL(v); ok {
			*media = append(*media, part)
			return marker, 1
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
		newParsed, n := stripMediaFromValue(parsed, media, marker, depth+1)
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
			nv, n := stripMediaFromValue(v[i], media, marker, depth+1)
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
			return map[string]any{"type": "text", "text": marker}, 1
		}
		// 否则递归 content 字段。
		if content, ok := v["content"]; ok {
			nv, n := stripMediaFromValue(content, media, marker, depth+1)
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
			return imageURLContentPart(part, url), true
		}
	case "image":
		// Anthropic / Gemini 风格：{"type":"image","source":{...}} 或顶格 data+mimeType。
		if url, ok := typedImageURL(part); ok {
			return imageURLContentPart(part, url), true
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

// imageURLContentPart 组装 image_url part，并把顶层 detail 合并进 image_url
// （OpenAI 兼容网关只接受 auto/low/high，Codex 风格的 original 降级为 auto）。
func imageURLContentPart(host map[string]any, imageURL map[string]any) map[string]any {
	if _, has := imageURL["detail"]; !has {
		if detail, ok := host["detail"]; ok {
			if s, ok := detail.(string); ok && s == "original" {
				detail = "auto"
			}
			imageURL["detail"] = detail
		}
	}
	return map[string]any{"type": "image_url", "image_url": imageURL}
}

// typedImageURL 识别 Anthropic/Gemini 风格图片块：
// {"type":"image","source":{"type":"base64|url",...}} 或顶格 {"data","mimeType"}。
func typedImageURL(part map[string]any) (map[string]any, bool) {
	if source, ok := part["source"].(map[string]any); ok {
		if !sourceMediaTypeIsImage(source) {
			return nil, false
		}
		if url := stringField(source, "url"); url != "" {
			return map[string]any{"url": url}, true
		}
		if data := rawString(source, "data"); data != "" {
			mediaType := firstNonEmpty(stringField(source, "media_type"),
				stringField(source, "mime_type"), stringField(source, "mimeType"))
			if strings.HasPrefix(strings.ToLower(data), "data:image/") {
				return map[string]any{"url": data}, true
			}
			if mediaType == "" {
				mediaType = "image/png"
			}
			return map[string]any{"url": "data:" + mediaType + ";base64," + data}, true
		}
		return nil, false
	}
	if data := rawString(part, "data"); data != "" {
		mediaType := firstNonEmpty(stringField(part, "mimeType"), stringField(part, "mime_type"))
		if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			return map[string]any{"url": "data:" + mediaType + ";base64," + data}, true
		}
	}
	return nil, false
}

// sourceMediaTypeIsImage 判定 source 声明的媒体类型是否为图片（缺失视为图片）。
func sourceMediaTypeIsImage(source map[string]any) bool {
	mediaType := firstNonEmpty(stringField(source, "media_type"),
		stringField(source, "mime_type"), stringField(source, "mimeType"))
	return mediaType == "" || strings.HasPrefix(strings.ToLower(mediaType), "image/")
}

// clampBase64ishStrings 媒体已被搬走时，把残余的长 data: 串或纯 base64 串替换成
// 字节数标记（对齐参考 clamp_base64ish_strings；只对已识别含媒体的输出生效，
// 普通长文本保持原样）。
func clampBase64ishStrings(value any) any {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		looksData := len(trimmed) >= wholeDataURLMinBytes &&
			strings.HasPrefix(strings.ToLower(trimmed), "data:")
		if looksData || looksLikeBase64Payload(trimmed) {
			return fmt.Sprintf("[omitted %d bytes]", len(v))
		}
		return v
	case []any:
		for i := range v {
			v[i] = clampBase64ishStrings(v[i])
		}
		return v
	case map[string]any:
		for k := range v {
			v[k] = clampBase64ishStrings(v[k])
		}
		return v
	default:
		return value
	}
}

// looksLikeBase64Payload 判定长串是否全部由 base64 字符组成。
func looksLikeBase64Payload(value string) bool {
	if len(value) < base64ishMinBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '=':
		default:
			return false
		}
	}
	return true
}
