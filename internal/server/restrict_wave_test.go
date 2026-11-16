package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestRestrictedWaveFailFastStillArmed 11140 平台级 fail-fast 的计数入口必须被调用。
//
// 背景（2026-11 社区实测）：4 个国内版账号在同一分钟全部返回 403 + 11140，
// 而凭据校验与积分查询都正常——这是平台侧批量动作，不是 4 个账号各自坏掉。
// 此时继续换号，等于把剩下还健康的号挨个送去撞风控。
//
// 回归点：账号级 Disable 只让**这个**号出池，轮转仍会去试下一个健康号。没有
// 平台级判定时，一次客户端请求会把 MaxRotate 个健康号全部打一遍。
func TestRestrictedWaveFailFastStillArmed(t *testing.T) {
	var hits int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		hits++
		// 中文文案的封禁形态：旧实现只认英文 "request illegal"，会漏判成
		// ErrClient（不禁用、只喂连败），这里同时锁住文案识别与平台级判定。
		return 403, `{"code":11140,"msg":"账号存在异常行为，已被限制使用"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "r1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "r3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 两个不同账号受限即判定平台级 → 立即终止轮转，不应把第三个号也打一遍。
	if hits >= 3 {
		t.Errorf("upstream hits=%d（11140 平台级 fail-fast 未生效，轮转打满了）", hits)
	}
	if !h.wafIP.active() {
		t.Error("两个不同账号 11140 受限后应激活平台级判定")
	}
}

// TestRestrictedSingleAccountDoesNotFailFast 单个账号受限不得触发平台级判定。
//
// 单号受限是账号自己的事：照常 Disable、照常换号，其余健康号应继续被使用。
// 若阈值判错（例如按命中次数而非不同账号数），一个坏号就能让整个网关停止
// 轮转，把可用性打到零。
func TestRestrictedSingleAccountDoesNotFailFast(t *testing.T) {
	// 只有第一个号受限，第二个号正常返回流。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if strings.Contains(authz, "at1") {
			return 403, `{"code":11140,"msg":"request illegal"}`, false
		}
		return 200, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n", true
	})
	p := testPoolWith(
		&auth.Auth{UID: "s1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "s2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if h.wafIP.active() {
		t.Error("单个账号受限不应激活平台级判定")
	}
	// 第二个号应被正常使用：不能因为第一个号受限就整单失败。
	if st, ok := p.Status("s2"); ok && st.Disabled {
		t.Error("健康账号不该被误禁用")
	}
}

// TestRestrictedContentDoesNotDisableAccount 11140 的内容审核语义不得禁用账号。
//
// 同一个 11140 承载两种语义：账号被限制 vs 这条内容没过审核。后者换号、禁用
// 都无意义（用户改内容即可），误判成账号受限会硬禁用健康账号。
func TestRestrictedContentDoesNotDisableAccount(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 403, `{"code":11140,"msg":"内容未通过安全审核，请调整后重试"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "c1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if st, ok := p.Status("c1"); ok && st.Disabled {
		t.Error("内容审核语义的 11140 不得禁用账号（内容问题不是账号问题）")
	}
}
