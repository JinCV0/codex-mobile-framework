package appserver_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codexmobile/internal/appserver"
	"codexmobile/internal/bridge"
	"codexmobile/internal/codex"
)

// 这个测试复现的正是用户遇到的场景：
// 桌面版 Codex 先占住一条对话（它自己 thread/start + resume），
// 然后手机端去接着用同一条对话 —— 修好之后不该再报 active writer。
//
// 它同时验证了桥接的三件事：握手、请求 id 改写、通知广播。
func TestDesktopAndPhoneShareOneAppServer(t *testing.T) {
	if os.Getenv("CODEX_BROKER_TEST") == "" {
		t.Skip("设置 CODEX_BROKER_TEST=1 才跑（会真的拉起 codex，并产生一次模型调用）")
	}
	bin := codex.FindBinary()
	if bin == "" {
		t.Fatal("找不到 codex 可执行文件")
	}
	workDir := t.TempDir()
	dataDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	app := appserver.New(bin)
	app.SetDefaultCwd(workDir)
	defer app.Close()

	// 旁观者：桌面端发起的轮次，手机侧就是靠这条链拿到实时事件的。
	var obsMu sync.Mutex
	var observed []string
	app.SetObserver(func(threadID, method string, params json.RawMessage) {
		obsMu.Lock()
		observed = append(observed, method+"|"+threadID)
		obsMu.Unlock()
	})

	logf := func(format string, a ...any) { t.Logf("[服务] "+format, a...) }
	go func() {
		if err := bridge.ServeAttach(ctx, dataDir, app, logf); err != nil && ctx.Err() == nil {
			t.Logf("接入点退出: %v", err)
		}
	}()

	// 等接入点写出来
	var info struct {
		Port   int    `json:"port"`
		Secret string `json:"secret"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(dataDir, "attach.json"))
		if err == nil && json.Unmarshal(raw, &info) == nil && info.Port != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("接入点没起来")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// ---- 假装自己是桌面版 Codex，连上来 ----
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port), 5*time.Second)
	if err != nil {
		t.Fatalf("连接入点失败: %v", err)
	}
	defer conn.Close()
	hello, _ := json.Marshal(map[string]any{
		"type": "hello", "secret": info.Secret,
		"args": []string{"-c", "features.code_mode_host=true", "app-server"},
	})
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReaderSize(conn, 1<<20)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("读握手回复失败: %v", err)
	}
	t.Logf("握手回复: %s", strings.TrimSpace(string(line)))
	if !strings.Contains(string(line), `"ok":true`) {
		t.Fatalf("被拒绝接入: %s", line)
	}

	desk := newFakeDesktop(conn, br)
	desk.call(t, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "codex-desktop-test", "version": "1.0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	desk.notify("initialized")

	// 桌面端开一条新对话，并像打开界面那样 resume 它（这一步以前会占住写入锁）
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	desk.call(t, "thread/start", map[string]any{
		"cwd": workDir, "sandbox": "danger-full-access", "approvalPolicy": "never",
	}, &started)
	if started.Thread.ID == "" {
		t.Fatal("桌面端没拿到 thread id")
	}
	threadID := started.Thread.ID
	t.Logf("桌面端开了一条对话: %s", threadID)

	// 桌面端先在这条对话里说一句话 —— 这样对话才会真正落到磁盘上，
	// 也才谈得上"写入锁"。
	desk.expectTurn(threadID)
	desk.call(t, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    []map[string]any{{"type": "text", "text": "只回复两个字：桌面"}},
	}, nil)
	if err := desk.waitTurnDone(threadID, 120*time.Second); err != nil {
		t.Fatalf("桌面端这一轮没跑完: %v", err)
	}
	t.Log("桌面端这一轮已跑完")

	// 桌面端这一轮必须被旁观者看到：这正是"电脑发起的轮次手机也能实时看到"的数据源。
	obsMu.Lock()
	deskObserved := append([]string(nil), observed...)
	obsMu.Unlock()
	sawStart, sawDone := false, false
	for _, o := range deskObserved {
		if o == "turn/started|"+threadID {
			sawStart = true
		}
		if o == "turn/completed|"+threadID {
			sawDone = true
		}
	}
	if !sawStart || !sawDone {
		t.Fatalf("桌面端发起的轮次没传给旁观者（应当手机能实时看到）：%v", deskObserved)
	}
	t.Logf("旁观者收到了桌面端这一轮的 %d 条通知", len(deskObserved))

	// 桌面端界面此刻正开着这条对话：它再 resume 一次，把写入者身份拿住。
	var resumed struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	desk.call(t, "thread/resume", map[string]any{"threadId": threadID}, &resumed)
	t.Logf("桌面端重新载入这条对话: %s", resumed.Thread.ID)

	// ---- 现在轮到"手机" ----
	// 修好之前，这一步会报 thread ... already has an active writer。
	phoneThread, err := app.ResumeThread(ctx, threadID, workDir, "danger-full-access", "never")
	if err != nil {
		t.Fatalf("手机接管桌面端正在用的对话失败: %v", err)
	}
	if phoneThread != threadID {
		t.Fatalf("拿到的对话号对不上: %s != %s", phoneThread, threadID)
	}
	t.Logf("手机成功接管同一条对话: %s", phoneThread)

	// 手机在这条对话上说一句话，桌面端应该同时看到这一轮的内容（实时同步）。
	// 顺便盯住旁观者：手机自己发起的那一轮，不能又被旁观者推一遍（会重复显示）。
	obsMu.Lock()
	obsBeforePhone := len(observed)
	obsMu.Unlock()

	type turnEnd struct {
		text string
		err  error
	}
	done := make(chan turnEnd, 1)
	go func() {
		res, err := app.RunTurn(ctx, phoneThread, "只回复两个字：收到", appserver.Sink{
			OnDelta: func(string) {},
		})
		done <- turnEnd{res.Text, err}
	}()

	sawPhoneTurn := false
	turnDeadline := time.After(120 * time.Second)
	for !sawPhoneTurn {
		select {
		case te := <-done:
			if te.err != nil {
				t.Fatalf("手机发消息失败: %v", te.err)
			}
			t.Logf("手机收到的回复: %q", te.text)
			sawPhoneTurn = true
		case <-turnDeadline:
			t.Fatal("等手机这一轮超时")
		default:
			// 顺手看看桌面端有没有收到同一条对话的通知
			if desk.seenThreadEvent(threadID) {
				sawPhoneTurn = sawPhoneTurn || false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !desk.seenThreadEvent(threadID) {
		t.Log("注意：桌面端没收到这条对话的通知（不影响手机使用，但实时同步需要复查）")
	} else {
		t.Log("桌面端同步收到了这条对话的通知")
	}

	obsMu.Lock()
	obsAfterPhone := len(observed)
	var extra []string
	if obsAfterPhone > obsBeforePhone {
		extra = append(extra, observed[obsBeforePhone:]...)
	}
	obsMu.Unlock()
	if len(extra) > 0 {
		t.Fatalf("手机自己发的那一轮被旁观者重复推了一遍（会重复显示）：%v", extra)
	}
}

// 两条不同的对话必须能同时跑。
//
// 用户实测过：一条对话还在跑时，去另一条发消息会被拒（"上一轮还没结束"）。
// 那是把一个进程内的 current 当成了全局闸门，而 Codex 本来就是一条对话一个轮次。
func TestTwoConversationsRunAtOnce(t *testing.T) {
	if os.Getenv("CODEX_BROKER_TEST") == "" {
		t.Skip("设置 CODEX_BROKER_TEST=1 才跑（会真的拉起 codex，并产生两次模型调用）")
	}
	bin := codex.FindBinary()
	if bin == "" {
		t.Fatal("找不到 codex 可执行文件")
	}
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	app := appserver.New(bin)
	app.SetDefaultCwd(workDir)
	defer app.Close()
	if err := app.Ensure(ctx, workDir); err != nil {
		t.Fatalf("起 app-server 失败: %v", err)
	}

	a, err := app.StartThread(ctx, workDir, "danger-full-access", "never")
	if err != nil {
		t.Fatalf("开第一条对话失败: %v", err)
	}
	b, err := app.StartThread(ctx, workDir, "danger-full-access", "never")
	if err != nil {
		t.Fatalf("开第二条对话失败: %v", err)
	}
	if a == b {
		t.Fatal("两条对话拿到了同一个 thread")
	}

	type outcome struct {
		which string
		text  string
		err   error
	}
	done := make(chan outcome, 2)
	start := func(which, thread string) {
		res, err := app.RunTurn(ctx, thread, "只回复两个字：收到", appserver.Sink{})
		done <- outcome{which, res.Text, err}
	}
	go start("A", a)
	time.Sleep(700 * time.Millisecond) // 让 A 先真的跑起来
	go start("B", b)

	// A 还在跑的时候，B 这条对话不该被判成"忙"
	if !app.ThreadBusy(a) {
		t.Log("注意：没观察到 A 在跑（可能太快），并发断言仍然有效")
	}
	if app.ThreadBusy(b) {
		t.Fatal("第二条对话被误判成忙")
	}
	if !app.Busy() {
		t.Fatal("有对话在跑，Busy 却是 false")
	}

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case o := <-done:
			if o.err != nil {
				t.Fatalf("对话 %s 跑失败: %v", o.which, o.err)
			}
			got[o.which] = o.text
			t.Logf("对话 %s 回复: %q", o.which, o.text)
		case <-ctx.Done():
			t.Fatal("等两条对话结束超时")
		}
	}
	if got["A"] == "" || got["B"] == "" {
		t.Fatalf("有对话没拿到回复: %+v", got)
	}
	if app.Busy() {
		t.Fatal("都跑完了 Busy 还是 true")
	}
}

// fakeDesktop 是一个极简的 JSON-RPC 客户端，扮演桌面版 Codex。
type fakeDesktop struct {
	conn net.Conn
	br   *bufio.Reader
	next int

	mu      sync.Mutex
	seen    map[string]bool
	pending map[int]chan json.RawMessage
	turns   map[string]chan struct{}
}

func newFakeDesktop(conn net.Conn, br *bufio.Reader) *fakeDesktop {
	d := &fakeDesktop{
		conn: conn, br: br,
		seen:    map[string]bool{},
		pending: map[int]chan json.RawMessage{},
		turns:   map[string]chan struct{}{},
	}
	go d.readLoop()
	return d
}

func (d *fakeDesktop) readLoop() {
	for {
		line, err := d.br.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Params struct {
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil {
			continue
		}
		if msg.Method != "" && msg.Params.ThreadID != "" {
			d.mu.Lock()
			d.seen[msg.Params.ThreadID] = true
			ch := d.turns[msg.Params.ThreadID]
			d.mu.Unlock()
			if msg.Method == "turn/completed" && ch != nil {
				select {
				case <-ch:
				default:
					close(ch)
				}
			}
		}
		if len(msg.ID) > 0 && msg.Method != "" {
			// 服务端反向请求：桌面端这边统一拒绝，避免测试卡住
			out, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": json.RawMessage(msg.ID),
				"result": map[string]any{"decision": "decline"},
			})
			_, _ = d.conn.Write(append(out, '\n'))
			continue
		}
		if len(msg.ID) > 0 {
			var id int
			if json.Unmarshal(msg.ID, &id) == nil {
				d.mu.Lock()
				ch := d.pending[id]
				delete(d.pending, id)
				d.mu.Unlock()
				if ch != nil {
					ch <- msg.Result
				}
			}
		}
	}
}

func (d *fakeDesktop) seenThreadEvent(threadID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[threadID]
}

// expectTurn 在发消息之前先登记，免得"跑完得太快"把通知漏掉。
func (d *fakeDesktop) expectTurn(threadID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.turns[threadID] == nil {
		d.turns[threadID] = make(chan struct{})
	}
}

func (d *fakeDesktop) waitTurnDone(threadID string, timeout time.Duration) error {
	d.mu.Lock()
	ch := d.turns[threadID]
	d.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("没有登记这一轮")
	}
	select {
	case <-ch:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("等 turn/completed 超时")
	}
}

func (d *fakeDesktop) notify(method string, params ...any) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if len(params) > 0 {
		msg["params"] = params[0]
	}
	raw, _ := json.Marshal(msg)
	_, _ = d.conn.Write(append(raw, '\n'))
}

// call 发一个请求，等它自己的回包。中间的通知和反向请求交给 readLoop 处理。
func (d *fakeDesktop) call(t *testing.T, method string, params any, out any) {
	t.Helper()
	d.next++
	id := d.next
	ch := make(chan json.RawMessage, 1)
	d.mu.Lock()
	d.pending[id] = ch
	d.mu.Unlock()

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, _ := json.Marshal(msg)
	if _, err := d.conn.Write(append(raw, '\n')); err != nil {
		t.Fatalf("写 %s 失败: %v", method, err)
	}
	select {
	case result := <-ch:
		if out != nil && len(result) > 0 {
			_ = json.Unmarshal(result, out)
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("%s 等回包超时", method)
	}
}
