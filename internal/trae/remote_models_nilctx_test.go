package trae

import "testing"

// fetchRemoteModels 传 nil ctx 不得 panic。
//
// 回归：context.WithTimeout(nil, ...) 会 panic("cannot create context from
// nil parent")。调用链上有多个入口（面板刷新、catalog 拉取、余额查询），
// 任何一个忘了传 ctx 都会让整个进程崩掉——比「这一次查询失败」严重得多。
func TestFetchRemoteModelsNilContextDoesNotPanic(t *testing.T) {
	acc := &Account{UID: "u1", AccessToken: "tok"}
	cfg := &Config{ChatBase: "http://127.0.0.1:1", APITraeBase: "http://127.0.0.1:1"}
	// 端口 1 必然连接失败：只要不 panic 即通过（返回 nil 或空 map 都可接受）。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil ctx 导致 panic: %v", r)
		}
	}()
	_ = NewClient().fetchRemoteModels(nil, cfg, acc)
}
