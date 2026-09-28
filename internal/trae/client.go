package trae

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// client.go Trae 上游 HTTP 调用。
//
// 出站头清单与蓝本（TraeWorkAssistant api_server/routes.rs）逐项对齐——Trae
// 上游对头族敏感，缺项会得到 4xx/风控而非明确报错。

const (
	// streamHeaderTimeout 流式请求首字节超时（长回答本身无总时长限制）。
	streamHeaderTimeout = 120 * time.Second
	dialTimeout         = 30 * time.Second
	idleConnTimeout     = 90 * time.Second
	// maxErrorBodyBytes 错误体读取上限（原文要透传，但异常大 body 不得拖垮网关）。
	maxErrorBodyBytes = 64 * 1024
)

// Client Trae 上游客户端（双 http.Client 共享连接池，与 thirdparty 同策略）。
type Client struct {
	transport  *http.Transport
	streamHTTP *http.Client
	jsonHTTP   *http.Client
}

// NewClient 构造客户端。
func NewClient() *Client {
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: streamHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		transport:  tr,
		streamHTTP: &http.Client{Transport: tr},
		jsonHTTP:   &http.Client{Transport: tr},
	}
}

// ChatStream 发起一次 llm_utils_chat 请求。
//
// 成功时返回未读取的 SSE 响应体（调用方负责 Close）；上游 >=400 时返回
// (nil, status, body, nil)——错误分类交给调用方；传输层错误才返回 err。
func (c *Client) ChatStream(ctx context.Context, cfg *Config, acc *Account, body []byte) (io.ReadCloser, int, []byte, error) {
	if cfg == nil || acc == nil {
		return nil, 0, nil, fmt.Errorf("trae: nil config or account")
	}
	token := acc.AccessTokenHeader()
	if token == "" {
		return nil, 0, nil, fmt.Errorf("trae: 账号 %s 无 access_token", acc.UID)
	}
	url := cfg.ChatBase + EndpointLLMChat
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build request: %w", err)
	}
	applyChatHeaders(req, cfg, acc, token)

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// applyChatHeaders 设置 Trae 上游要求的完整头族。
//
// 说明：`x-ide-token` 传**裸 token**，`Authorization` 传带 `Cloud-IDE-JWT ` 前缀的
// 完整值——两处口径都保留（蓝本用前者，协议文档记载后者），避免因上游校验侧变化
// 而单点失效。
func applyChatHeaders(req *http.Request, cfg *Config, acc *Account, tokenWithPrefix string) {
	raw := acc.AccessToken
	if len(tokenWithPrefix) > len("Cloud-IDE-JWT ") {
		raw = tokenWithPrefix[len("Cloud-IDE-JWT "):]
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "*/*")
	h.Set("User-Agent", "TraeClient/TTNet")
	h.Set("Authorization", tokenWithPrefix)
	h.Set("X-Ide-Token", raw)
	h.Set("X-App-Id", cfg.AppID)
	h.Set("X-App-Version", "default")
	h.Set("X-App-Version-Code", cfg.IDEVersionCode)
	h.Set("X-Ide-Version", cfg.IDEVersion)
	h.Set("X-Ide-Version-Code", cfg.IDEVersionCode)
	h.Set("X-Ide-Version-Type", "stable")
	h.Set("X-Device-Type", "windows")
	h.Set("X-Device-Brand", "CREFG-XX")
	h.Set("X-Device-Cpu", "Intel")
	h.Set("X-Device-Id", acc.DeviceID)
	h.Set("X-Machine-Id", acc.MachineID)
	h.Set("X-Os-Version", "Windows 11 Home China")
	h.Set("Request-Traffic-Type", "prod")
	h.Set("Package-Type", "stable_cn")
	h.Set("X-Lgw-Req-Sdk-Type", "3")
	h.Set("X-Lscbd-Aid", "787976")
	h.Set("X-Lscbd-Platform", "windows")
	h.Set("X-Ss-Dp", "787976")
	h.Set("App-Version", cfg.IDEVersion)
	h.Set("Referer", cfg.ChatBase+EndpointLLMChat)

	// 链路追踪头：上游按 request id 记账，缺省不致命但补齐更贴近真实客户端。
	traceID := "00-" + genUUIDLike() + "-" + genUUIDLike() + "-01"
	h.Set("X-Tt-Trace-Id", traceID)
	h.Set("X-Request-Id", "req_"+genUUIDLike())
	if len(traceID) >= 16 {
		h.Set("X-Custom-Trace-Id", traceID[:16])
	}
	if len(traceID) >= 35 {
		h.Set("X-Flow-Traceparent", "04-"+traceID[3:35]+"-"+genUUIDLike()+"-01")
	}
}

// RetryAfter 从响应头解析 Retry-After（秒）；缺失或非法返回 0。
// 供调用方做分级退避（429 场景）。
func RetryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
