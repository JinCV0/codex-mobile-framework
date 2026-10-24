package httpapi

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"codexmobile/internal/store"
)

// 电脑端发起的轮次，要能被翻译成手机界面认识的事件。
func TestOnThreadActivityMapsDesktopTurn(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess := db.NewSession(t.TempDir(), "danger-full-access")
	const thread = "01a1071a-d724-7cb2-aa3f-7c64f64af04c"
	db.SetThreadID(sess.ID, thread)

	s := &Server{DB: db}
	id, ch, unsub := s.subscribeLive()
	defer unsub()
	_ = id

	emit := func(method string, params string) {
		s.OnThreadActivity(thread, method, json.RawMessage(params))
	}
	emit("turn/started", `{"threadId":"`+thread+`"}`)
	emit("item/agentMessage/delta", `{"threadId":"`+thread+`","delta":"你"}`)
	emit("item/agentMessage/delta", `{"threadId":"`+thread+`","delta":"好"}`)
	emit("item/started", `{"threadId":"`+thread+`","item":{"type":"commandExecution","command":"go build ./..."}}`)
	emit("turn/completed", `{"threadId":"`+thread+`","turn":{"id":"t1","status":"completed"}}`)

	want := []liveEvent{}
	deadline := time.After(2 * time.Second)
	for len(want) < 5 {
		select {
		case ev := <-ch:
			want = append(want, ev)
		case <-deadline:
			t.Fatalf("只收到 %d 条事件", len(want))
		}
	}

	kinds := []string{}
	for _, ev := range want {
		kinds = append(kinds, ev.Kind)
		if ev.SessionID != sess.ID {
			t.Fatalf("事件没挂到正确的会话上: %+v", ev)
		}
		if ev.ThreadID != thread {
			t.Fatalf("事件没带线程号: %+v", ev)
		}
	}
	got := kinds[0] + "," + kinds[1] + "," + kinds[2] + "," + kinds[3] + "," + kinds[4]
	if got != "run,delta,delta,tool,done" {
		t.Fatalf("事件顺序不对: %s", got)
	}
	if want[1].Event.Text+want[2].Event.Text != "你好" {
		t.Fatalf("增量文本没拼对: %q", want[1].Event.Text+want[2].Event.Text)
	}
	if want[3].Event.Text != "go build ./..." {
		t.Fatalf("命令文本丢了: %q", want[3].Event.Text)
	}

	// 这一轮结束后，"电脑正在跑"的状态要清掉，否则手机上会一直转圈
	if run := s.runState(); run != nil {
		t.Fatalf("跑完了还在报运行中: %+v", run)
	}
}

// 正在跑的轮次不能被"十分钟没动静就过滤掉"的规则抹掉：
// 一轮长任务卡在命令上二十几分钟不发声是正常的，手机端不能因此失去反馈。
func TestRunningThreadSurvivesQuietStretch(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	sess := db.NewSession(t.TempDir(), "danger-full-access")
	const thread = "01a10cb5-9407-7bf1-aaec-ad02334fa7e8"
	db.SetThreadID(sess.ID, thread)

	s := &Server{DB: db}
	s.OnThreadActivity(thread, "turn/started", json.RawMessage(`{"threadId":"`+thread+`"}`))

	// 一轮长任务卡在一条命令上，二十几分钟一条通知都没有。
	// 光看时间戳会以为它死了 —— 但轮次还没结束，手机上就不能失去反馈。
	s.actMu.Lock()
	s.act[thread].At = time.Now().Add(-20 * time.Minute).UnixMilli()
	s.actMu.Unlock()

	inSnap := false
	for id := range s.threadActivitySnapshot() {
		if id == thread {
			inSnap = true
		}
	}
	if !inSnap {
		t.Fatal("正在跑的对话被十分钟保鲜期过滤掉了，手机端会看不到反馈")
	}
	if run := s.deskRunState(); run == nil || run.ThreadID != thread {
		t.Fatalf("电脑端正在跑的轮次丢了: %+v", run)
	}

	// 真的结束了：状态要收回去，不能一直转圈
	s.OnThreadActivity(thread, "turn/completed", json.RawMessage(`{"threadId":"`+thread+`"}`))
	if run := s.deskRunState(); run != nil {
		t.Fatalf("跑完了还在报运行中: %+v", run)
	}
}

// 线程号对不上任何手机会话的，直接丢掉：不能把电脑上的所有活动都灌给手机。
func TestOnThreadActivityIgnoresUnknownThread(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: db}
	_, ch, unsub := s.subscribeLive()
	defer unsub()

	s.OnThreadActivity("不存在的线程", "turn/started", json.RawMessage(`{}`))
	select {
	case ev := <-ch:
		t.Fatalf("不该推事件: %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
	if run := s.runState(); run != nil {
		t.Fatalf("不该记录运行状态: %+v", run)
	}
}
