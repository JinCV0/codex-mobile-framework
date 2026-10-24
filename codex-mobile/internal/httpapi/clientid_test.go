package httpapi

import "testing"

// 离线补发的核心约定：同一个消息号第一次放行，第二次必须挡住，
// 否则"服务端已收到、响应没回手机"的那条会被再跑一遍。
func TestMarkClientIDDedup(t *testing.T) {
	s := &Server{}
	if !s.markClientID("m-abc-1") {
		t.Fatal("第一次见到的消息号应该放行")
	}
	if s.markClientID("m-abc-1") {
		t.Fatal("重复的消息号必须被挡住，不能重复发起一轮")
	}
	if !s.markClientID("m-abc-2") {
		t.Fatal("另一条消息应该正常放行")
	}
}

// 空号不记账：老版本手机不带 clientId 时行为和以前完全一样。
func TestMarkClientIDEmptyCallerBypasses(t *testing.T) {
	s := &Server{}
	if !s.markClientID("") {
		t.Fatal("空号不应被当成幂等键（实际调用方不会走到这里，但行为要可预期）")
	}
}
