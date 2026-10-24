package httpapi

import (
	"path/filepath"
	"testing"

	"codexmobile/internal/store"
)

// 补发必须发回它原来那条对话。
//
// 2026-10-10 的线上事故：/api/send 没有目标对话，服务端只认"当前活跃的那条"，
// 而手机的离线补发是在切对话的过程中发起的 —— 消息就落到了另一条对话里，
// 用户看到的是"上一条消息被重复发到了别的对话"。
func TestPickSendSessionTargetsRightConversation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := db.NewSession(t.TempDir(), "")
	second := db.NewSession(t.TempDir(), "")

	// seconds 是当前活跃的那条
	cur, ok := db.Active()
	if !ok || cur.ID != second.ID {
		t.Fatalf("准备数据不对，活跃会话是 %q", cur.ID)
	}

	s := &Server{DB: db}
	if got := s.pickSendSession(first.ID); got.ID != first.ID {
		t.Fatalf("指定了对话却发到了别处：想要 %q，拿到 %q", first.ID, got.ID)
	}
	// 不带（老客户端）仍然发到活跃那条
	if got := s.pickSendSession(""); got.ID != second.ID {
		t.Fatalf("没带目标时应该发到活跃会话 %q，实际 %q", second.ID, got.ID)
	}
	// 认不出来的 id：退回活跃那条，而不是凭空造一条
	if got := s.pickSendSession("不存在的会话"); got.ID != second.ID {
		t.Fatalf("无效 id 应该退回活跃会话 %q，实际 %q", second.ID, got.ID)
	}
}
