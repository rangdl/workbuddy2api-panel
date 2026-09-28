package thirdparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// 流式首字节与空闲超时。与 internal/upstream 的三段式语义对齐：
// 非流式用总超时（TimeoutSeconds），流式只约束首字节（ResponseHeaderTimeout），
// 流中空闲交给调用方的 ctx（客户端断连即取消），不设总时长——长回答合法可达数分钟。
const (
	streamHeaderTimeout = 120 * time.Second
	dialTimeout         = 30 * time.Second
	idleConnTimeout     = 90 * time.Second
)

// Client 第三方上游转发客户端。
//
// 双 http.Client 共享同一 Transport：streamHTTP 无总超时（长流式），
// jsonHTTP 有总超时（非流式一次性响应）。连接池不重复建立。
type Client struct {
	transport  *http.Transport
	streamHTTP *http.Client
	jsonHTTP   *http.Client
}

// NewClient 构造转发客户端。
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
		// 流式响应不缓冲：SSE 帧到达即交给上层，避免网关侧引入额外延迟。
		ResponseHeaderTimeout: streamHeaderTimeout,
		ForceAttemptHTTP2:     true,
	}
	return &Client{
		transport:  tr,
		streamHTTP: &http.Client{Transport: tr},
		jsonHTTP:   &http.Client{Transport: tr},
	}
}

// Chat 向第三方上游发起一次 chat 请求。
//
// stream=true 时返回未读取的响应体（调用方按 SSE 逐帧转发，须自行 Close）；
// stream=false 时把响应体读全后随 status 一起返回（rc 为 nil）。
// 上游返回 >=400 时同样返回 (nil, status, body, nil)——错误分类交给调用方，
// 传输层错误才返回 err。
func (c *Client) Chat(ctx context.Context, p *Provider, body []byte, stream bool) (io.ReadCloser, int, []byte, error) {
	if p == nil {
		return nil, 0, nil, fmt.Errorf("thirdparty: nil provider")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.ChatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	applyAuthHeaders(req, p)

	hc := c.jsonHTTP
	if stream {
		hc = c.streamHTTP
	} else {
		// 非流式总超时按 provider 配置；用请求级 ctx 而非 Client.Timeout，
		// 以便与调用方 ctx（客户端断连）取更早者。
		// TimeoutSeconds<=0 必须回落缺省：0 会让 context.WithTimeout 立即到期，
		// 表现为"所有非流式请求瞬间 502"，而不经 Load() 的构造路径很容易漏填。
		timeout := time.Duration(p.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = defaultTimeoutSeconds * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, resp.StatusCode, raw, nil
	}
	if stream {
		return resp.Body, resp.StatusCode, nil, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return nil, resp.StatusCode, raw, nil
}

// maxErrorBodyBytes 上游错误体读取上限：错误原文要透传给客户端，
// 但异常超大 body 不得拖垮网关内存（与 upstream 侧 64KB 同量级）。
const maxErrorBodyBytes = 64 * 1024

// Probe 连通性探活：向该 provider 发一条最小 chat 请求（非流式），
// 返回可读摘要或失败原因。供面板「测试连接」按钮使用。
func (c *Client) Probe(ctx context.Context, p *Provider) (string, error) {
	if p == nil {
		return "", fmt.Errorf("thirdparty: nil provider")
	}
	model := ""
	if len(p.Models) > 0 {
		model = p.Models[0]
	}
	if model == "" {
		return "", fmt.Errorf("provider %s 未声明 models，无法探活（兜底 provider 需先指定一个模型名）", p.Name)
	}
	body := fmt.Sprintf(
		`{"model":%q,"messages":[{"role":"user","content":"ping"}],"max_tokens":16,"stream":false}`,
		model)
	_, status, raw, err := c.Chat(ctx, p, []byte(body), false)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("上游返回 status %d：%s", status, preview(raw, 200))
	}
	return fmt.Sprintf("连通成功（model=%s，响应 %d 字节）", model, len(raw)), nil
}

// applyAuthHeaders 设置鉴权与自定义头。自定义头在鉴权之后应用——
// 少数中转要求非 Bearer 的自定义鉴权头，允许用户显式覆盖。
func applyAuthHeaders(req *http.Request, p *Provider) {
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
}

// preview 截断文本用于错误摘要（避免把超长上游原文塞进面板/日志）。
func preview(raw []byte, n int) string {
	s := string(raw)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
