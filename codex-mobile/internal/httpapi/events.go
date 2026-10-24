package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"codexmobile/internal/codex"
	"codexmobile/internal/notify"
	"codexmobile/internal/store"
)

// liveEvent 是推给所有界面的实时事件。
//
// 手机和电脑两边都订阅同一条流，所以"这条对话正在跑"是两端同时看得见的，
// 不需要各自去猜（以前是靠各自轮询 /api/status，最慢要等 4 秒）。
type liveEvent struct {
	SessionID string      `json:"sessionId,omitempty"`
	ThreadID  string      `json:"threadId,omitempty"`
	Kind      string      `json:"kind"` // run / working / delta / message / tool / done / error
	Event     codex.Event `json:"event,omitempty"`
	At        int64       `json:"at"`
}

type subscriber struct {
	ch chan liveEvent
}

func (s *Server) ensureLive() {
	s.liveMu.Lock()
	if s.live == nil {
		s.live = map[int]*subscriber{}
	}
	s.liveMu.Unlock()
}

func (s *Server) subscribeLive() (int, chan liveEvent, func()) {
	s.ensureLive()
	ch := make(chan liveEvent, 256)
	s.liveMu.Lock()
	s.liveNext++
	id := s.liveNext
	s.live[id] = &subscriber{ch: ch}
	s.liveMu.Unlock()
	return id, ch, func() {
		s.liveMu.Lock()
		delete(s.live, id)
		s.liveMu.Unlock()
	}
}

// publishLive 把事件推给所有正在看的界面。没人看就直接丢掉，
// 绝不阻塞正在跑的对话。
func (s *Server) publishLive(ev liveEvent) {
	if ev.At == 0 {
		ev.At = time.Now().UnixMilli()
	}
	s.liveMu.Lock()
	if len(s.live) == 0 {
		s.liveMu.Unlock()
		return
	}
	subs := make([]*subscriber, 0, len(s.live))
	for _, sub := range s.live {
		subs = append(subs, sub)
	}
	s.liveMu.Unlock()
	for _, sub := range subs {
		select {
		case sub.ch <- ev:
		default: // 这个客户端跟不上了就丢一条，别拖住别人
		}
	}
}

// liveEvents 是界面订阅的 SSE 流（手机和电脑打开的都是它）。
func (s *Server) liveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "这个环境不支持流式输出", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	_, ch, unsub := s.subscribeLive()
	defer unsub()

	// 连上就先告诉它"现在有没有在跑"，省得客户端还得先问一次接口
	if run := s.runState(); run != nil {
		writeSSE(w, liveEvent{
			SessionID: run.SessionID, ThreadID: run.ThreadID,
			Kind: "run", At: run.StartedAt,
		})
	}
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			writeSSE(w, ev)
			flusher.Flush()
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n") // 心跳，防中间设备把长连接掐了
			flusher.Flush()
		}
	}
}

func writeSSE(w io.Writer, ev liveEvent) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", raw)
}

// debugDesktopTurn 让服务自己扮演"电脑端"，在指定对话上起一轮。
//
// 和真桌面端唯一的区别是消息来源：它同样不登记成手机发起的轮次，
// 所以事件走的完全是 desktop 那条旁观链路。用它可以不开桌面端就验证
// "电脑端发消息 -> 手机端实时更新"。
func (s *Server) debugDesktopTurn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		Thread  string `json:"thread"`
		Text    string `json:"text"`
	}
	s.readJSON(r, &body)

	threadID := strings.TrimSpace(body.Thread)
	sessionID := strings.TrimSpace(body.Session)
	if threadID == "" && sessionID != "" {
		if sess, ok := s.DB.ByID(sessionID); ok {
			threadID = sess.ThreadID
		}
	}
	if threadID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "给个 thread 或带 thread 的 session"})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		body.Text = "（自测）这一轮由电脑端发起"
	}
	if err := s.App.FireDesktopTurn(r.Context(), threadID, body.Text); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sess, mapped := s.DB.ByThread(threadID)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "thread": threadID, "text": body.Text,
		"queued": true, "mappedSession": map[bool]string{true: sess.ID, false: ""}[mapped],
	})
}

// threadActivity 是"这条对话最近动没动"的一条记录。
//
// Gen 只增不减：手机记住自己上次看到的号，发现号变了就把内容读回来。
// 这样就算某条事件没送到手机，下一次问也能补上 —— 这是关键，
// 之前那套"服务端推给手机"的方案一旦漏一条就永远补不回来。
type threadActivity struct {
	Gen int64 `json:"n"`
	At  int64 `json:"at"`
	Run bool  `json:"run"`
	// Act 是"这条现在在干什么"，手机列表页照着副屏那套显示成一行
	//（▸ 正在执行的命令 / 💬 正在写回答 / … 正在思考）。
	Act string `json:"act,omitempty"`
}

// noteActivity 记下"这条对话现在在干什么"。手机列表页要把它显示成一行
// （副屏那套格式的第三行），顺便把"最后一次活动时间"往前推 —— 那个时间
// 同时用来判"还在跑"还是"未收尾"。
func (s *Server) noteActivity(threadID, label string) {
	if threadID == "" || label == "" {
		return
	}
	now := time.Now().UnixMilli()
	s.actMu.Lock()
	if s.act == nil {
		s.act = map[string]*threadActivity{}
	}
	a := s.act[threadID]
	if a == nil {
		a = &threadActivity{}
		s.act[threadID] = a
	}
	a.Act = label
	a.At = now
	s.actMu.Unlock()
}

// activityLabel 把一条事件翻成给列表页看的一行字。太长就截断，
// 列表那一行放不下，全的命令去对话里看。
func activityLabel(evt codex.Event) string {
	text := strings.TrimSpace(evt.Text)
	switch evt.Type {
	case "tool":
		if text == "" {
			return "▸ 正在执行命令"
		}
		return "▸ " + clipRunes(text, 70)
	case "reasoning":
		return "… 正在思考"
	case "delta", "message":
		return "💬 正在写回答"
	}
	return ""
}

// clipRunes 按字符（不是字节）截断，免得把中文切一半。
func clipRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// noteThreadActivity 记一笔"这条对话动了"。
func (s *Server) noteThreadActivity(threadID, method string) {
	if threadID == "" {
		return
	}
	now := time.Now().UnixMilli()
	s.actMu.Lock()
	if s.act == nil {
		s.act = map[string]*threadActivity{}
	}
	a := s.act[threadID]
	if a == nil {
		a = &threadActivity{}
		s.act[threadID] = a
	}
	a.Gen++
	a.At = now
	switch method {
	case "turn/started":
		a.Run = true
	case "turn/completed", "error":
		a.Run = false
	}
	// 记账本别无限长大：十分钟没动静的就不再回报给手机。
	if len(s.act) > 80 {
		for k, v := range s.act {
			if now-v.At > 10*60*1000 {
				delete(s.act, k)
			}
		}
	}
	s.actMu.Unlock()
}

// threadActivitySnapshot 只回最近十分钟有动静的那几条，手机只关心自己那条。
//
// 例外：**正在跑的那条不设保鲜期**。一轮长任务中间可能有十几分钟一条通知都没有
// （正卡在一条命令上），按时间过滤会把它整条抹掉，手机那边就变成"明明在跑却看着像停了"，
// 而且切一次对话就再也找不回来。
func (s *Server) threadActivitySnapshot() map[string]threadActivity {
	cut := time.Now().Add(-10 * time.Minute).UnixMilli()
	out := map[string]threadActivity{}
	s.actMu.Lock()
	for k, v := range s.act {
		if v.At >= cut || v.Run {
			out[k] = *v
		}
	}
	s.actMu.Unlock()
	return out
}

// threadAliveIn 表示这条对话在最近 within 之内还动过。
//
// 用处是判"它到底还在跑吗"：一轮真在跑的时候，agent 每条通知都会记账，
// 时间戳一直在往前走；一旦对方不再发任何东西（进程被换、通知丢了、轮次被
// 拦腰掐断），时间戳就停住 —— 这就是最可靠的"已经死了"的信号。
func (s *Server) threadAliveIn(threadID string, within time.Duration) bool {
	if threadID == "" {
		return false
	}
	s.actMu.Lock()
	defer s.actMu.Unlock()
	a := s.act[threadID]
	if a == nil {
		return false
	}
	return time.Since(time.UnixMilli(a.At)) <= within
}

// OnThreadActivity 把"电脑那边发起的轮次"翻译成界面事件。
//
// 为什么会缺这一块：手机自己发消息时，是 /api/send 里的 emit 把事件推出去的；
// 电脑端在它自己的界面里发消息时，那条路径根本不存在，
// 于是手机要等到下次主动读历史才知道刚才发生了什么（被动补拉）。
//
// 这里只处理"能对上手机会话的那条 thread"：
//   - appserver 那边已经用 runFor 排除了手机自己在跑的轮次，不会重复推
//   - 线程号对不上任何会话的直接丢掉，不把电脑上所有活动都灌给手机
func (s *Server) OnThreadActivity(threadID, method string, params json.RawMessage) {
	// 先记账：这条是给手机"问"的，不依赖任何推送是否送达。
	if method != "appserver/started" {
		s.noteThreadActivity(threadID, method)
		s.touchDeskRun(threadID)
	}
	// app-server 被换了一条：上一轮不管跑到哪都结束了，
	// 把"电脑端正在跑"的状态收回去，免得手机上一直转圈。
	if method == "appserver/started" {
		s.clearDeskRun("")
		return
	}
	if s.DB == nil || threadID == "" {
		return
	}
	sess, ok := s.DB.ByThread(threadID)
	if !ok {
		return
	}

	switch method {
	case "turn/started":
		s.setDeskRun(sess.ID, threadID)
		s.publishLive(liveEvent{
			SessionID: sess.ID, ThreadID: threadID, Kind: "run",
		})

	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		if json.Unmarshal(params, &p) != nil || p.Delta == "" {
			return
		}
		s.noteActivity(threadID, "💬 正在写回答")
		s.publishLive(liveEvent{
			SessionID: sess.ID, ThreadID: threadID, Kind: "delta",
			Event: codex.Event{Type: "delta", Text: p.Delta},
		})

	case "item/started":
		var p struct {
			Item struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"item"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		switch p.Item.Type {
		case "commandExecution":
			s.noteActivity(threadID, "▸ "+clipRunes(p.Item.Command, 70))
			s.publishLive(liveEvent{
				SessionID: sess.ID, ThreadID: threadID, Kind: "tool",
				Event: codex.Event{Type: "tool", Text: p.Item.Command},
			})
		case "reasoning":
			s.noteActivity(threadID, "… 正在思考")
			s.publishLive(liveEvent{
				SessionID: sess.ID, ThreadID: threadID, Kind: "reasoning",
				Event: codex.Event{Type: "reasoning"},
			})
		case "fileChange":
			s.noteActivity(threadID, "✎ 正在改文件")
			s.publishLive(liveEvent{
				SessionID: sess.ID, ThreadID: threadID, Kind: "tool",
				Event: codex.Event{Type: "tool", Text: "修改文件"},
			})
		}

	case "turn/completed":
		// 只有"确实在跑 -> 停下"这一次才推，error/completed 连着来也只吵一遍。
		if s.clearDeskRun(threadID) {
			var p struct {
				Turn struct {
					Status string `json:"status"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(params, &p)
			go s.notifyDesktopTurnDone(sess, p.Turn.Status)
		}
		s.publishLive(liveEvent{
			SessionID: sess.ID, ThreadID: threadID, Kind: "done",
		})

	case "error":
		errMsg := ""
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(params, &p) == nil {
			errMsg = p.Message
		}
		if s.clearDeskRun(threadID) {
			go s.notifyDesktopTurnError(sess, errMsg)
		}
		s.publishLive(liveEvent{
			SessionID: sess.ID, ThreadID: threadID, Kind: "error",
		})
	}
}

// notifyDesktopTurnDone 把"电脑那边发起的一轮跑完了"推到手机。
//
// 手机自己发起的轮次在 /api/send 收尾时推；桌面端发起的走这里，两边对齐，
// 否则人拿着手机走开，电脑上自己跑的活干完了没人知道。
func (s *Server) notifyDesktopTurnDone(sess store.Session, status string) {
	cfg := s.DB.NotifyConfig()
	if notify.Resolve(cfg) == nil {
		return
	}
	ok := status != "failed" && status != "interrupted"
	body := ""
	if s.App != nil && sess.ThreadID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		msgs, _, err := s.App.ReadThread(ctx, sess.ThreadID)
		cancel()
		if err == nil {
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Text) != "" {
					body = notify.Summarize(msgs[i].Text)
					break
				}
			}
		}
	}
	if body == "" {
		if ok {
			body = "电脑上的任务跑完了，打开手机看结果"
		} else if status != "" {
			body = "电脑上的任务没跑完（" + status + "）"
		} else {
			body = "电脑上的任务结束了"
		}
	}
	if _, err := notify.Send(cfg, notify.Title(ok, s.DB.Title(sess.ID)), body); err != nil {
		log.Printf("电脑端轮次完成推送没发出去：%v", err)
	}
}

// notifyDesktopTurnError 是桌面端那一轮直接报错的推送。
func (s *Server) notifyDesktopTurnError(sess store.Session, msg string) {
	cfg := s.DB.NotifyConfig()
	if notify.Resolve(cfg) == nil {
		return
	}
	body := "电脑上的任务出错了"
	if msg = strings.TrimSpace(msg); msg != "" {
		if r := []rune(msg); len(r) > 160 {
			msg = string(r[:160])
		}
		body += "：" + msg
	}
	if _, err := notify.Send(cfg, notify.Title(false, s.DB.Title(sess.ID)), body); err != nil {
		log.Printf("电脑端轮次出错推送没发出去：%v", err)
	}
}
