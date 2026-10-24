package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codexmobile/internal/store"
)

// 手机断线重连后会把"没拿到结果"的那条重发一遍，而这一轮其实正在电脑上跑。
// 这种情况必须认出"已经收过了"，否则手机会一直显示"没发出去，正在重发"。
func TestDuplicateSendDetectsResendByClientID(t *testing.T) {
	s := &Server{}
	if !s.markClientID("m-abc") {
		t.Fatal("第一次记幂等号应该成功")
	}
	if !s.duplicateSend(store.Session{}, "m-abc", "随便什么", false) {
		t.Fatal("同一个幂等号再送一遍应该判为重复")
	}
	if s.duplicateSend(store.Session{}, "m-other", "随便什么", false) {
		t.Fatal("没见过的幂等号不该判为重复")
	}
}

// 服务重启会把幂等表清空，这时靠"最后收下的就是一模一样的话 + 它还在跑"来认。
func TestDuplicateSendFallsBackToLastUserText(t *testing.T) {
	s := &Server{}
	session := store.Session{
		ThreadID: "01a1-thread",
		Messages: []store.Message{
			{Role: "user", Text: "看一下这个 bug", Ts: time.Now().UnixMilli()},
			{Role: "assistant", Text: "好", Ts: time.Now().UnixMilli()},
		},
	}
	if !s.duplicateSend(session, "", "看一下这个 bug", true) {
		t.Fatal("内容相同、这轮还在跑，应该判为重复")
	}
	// 这一轮已经跑完：同样的内容可能是用户真的要再发一遍，不能吞掉
	if s.duplicateSend(session, "", "看一下这个 bug", false) {
		t.Fatal("这一轮没在跑时不该按内容判重复")
	}
	// 内容不一样：就是一条新消息
	if s.duplicateSend(session, "", "换一个问题", true) {
		t.Fatal("内容不同不该判为重复")
	}
}

// 很久以前发过同样一句、现在又在跑另一轮：不能误判成重复。
func TestDuplicateSendIgnoresStaleText(t *testing.T) {
	s := &Server{}
	session := store.Session{
		ThreadID: "01a1-thread",
		Messages: []store.Message{
			{Role: "user", Text: "继续", Ts: time.Now().Add(-2 * time.Hour).UnixMilli()},
		},
	}
	if s.duplicateSend(session, "", "继续", true) {
		t.Fatal("两小时前那句不该当成这次的重发")
	}
}

// 走一遍真实入口：重发的请求必须回 duplicate 标记。
// 手机就是靠这个标记把"没发出去"的气泡摘掉的，缺了它就永远转圈。
func TestSendReturnsDuplicateFlag(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess := db.NewSession(t.TempDir(), "")
	db.SetThreadID(sess.ID, "01a1-thread")
	db.AddMessage(sess.ID, store.Message{Role: "user", Text: "跑一下测试", Ts: time.Now().UnixMilli()})
	s := &Server{DB: db}
	s.markClientID("m-resend") // 上一条其实已经收到了

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/send",
		strings.NewReader(`{"text":"跑一下测试","clientId":"m-resend"}`))
	s.send(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("补发应该回 409，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Duplicate bool   `json:"duplicate"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Duplicate {
		t.Fatalf("回包里缺少 duplicate 标记：%s", rec.Body.String())
	}
}

// 去重标记必须活过服务重启。
//
// 每次换装都会重启服务；手机的发件箱可不管这些，重启后照样补发。
// 标记只放在内存里的话，那一次补发就会被当成新消息再跑一遍
// （2026-10-10 线上就是这么把同一条消息发了两遍）。
func TestClientIDsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	first := &Server{DataDir: dir}
	if !first.markClientID("m-keep") {
		t.Fatal("第一次应该记下这个号")
	}
	if !first.seenClientID("m-keep") {
		t.Fatal("同一个实例里应该查得到")
	}

	// 模拟服务重启：新实例、同一个数据目录
	second := &Server{DataDir: dir}
	if !second.seenClientID("m-keep") {
		t.Fatal("重启后去重标记必须还在，否则手机的补发会被当成新消息")
	}
	if second.markClientID("m-keep") {
		t.Fatal("重启后同一个号不该再放行")
	}
	if second.seenClientID("m-never-seen") {
		t.Fatal("没见过的号不该命中")
	}
}
