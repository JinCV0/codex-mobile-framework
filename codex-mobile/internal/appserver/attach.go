package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Attach 接管一条来自桌面版 Codex 的连接。
//
// 桌面版启动时会拉起它认为的 "codex CLI"（我们把 CODEX_CLI_PATH 指向了本程序），
// 于是它连到这里。从这一刻起，桌面端和手机用的是同一个 app-server 进程，
// Codex 那条"一条对话只能有一个写入者"的进程级锁就不再互相打架了。
//
// 调用方必须先做握手（见 bridge 包），进来之后就是纯粹的 JSON-RPC 行。
func (c *Client) Attach(conn io.ReadWriteCloser) error {
	c.mu.Lock()
	if c.desktop != nil {
		// 桌面端会同时开好几条 CLI 连接（它自己内部重试/多窗口），
		// 以前我们只认第一条、把后来的拒掉 —— 被拒的那个代理会**退回去
		// 自己起一个真的 app-server**，于是又变成两个 app-server 抢同一条
		// 对话，而且反复重连把正在跑的轮次搅断（2026-10-05 实测）。
		// 正确做法：新的顶替旧的，旧的连接一断它自己就退出了。
		old := c.desktop
		c.desktop = conn
		c.mu.Unlock()
		_ = old.Close()
	} else {
		c.desktop = conn
		c.mu.Unlock()
	}

	defer func() {
		c.mu.Lock()
		if c.desktop == conn {
			c.desktop = nil
		}
		c.mu.Unlock()
		_ = conn.Close()
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 256*1024), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		c.handleDesktopMessage(msg)
	}
	return nil
}

// HasDesktop 表示桌面版现在挂着没有。
func (c *Client) HasDesktop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.desktop != nil
}

// SetObserver 注册一个旁观者：app-server 的每条通知都会先问它要不要。
//
// 用途只有一个 —— 桌面端在某条对话里发起的那一轮，手机平时完全看不到，
// 靠这里把它翻译成界面事件推过去。
// 注意：回调跑在收报文那个 goroutine 上，必须立刻返回，不能阻塞、不能做重活。
func (c *Client) SetObserver(fn func(threadID, method string, params json.RawMessage)) {
	c.obsMu.Lock()
	c.observer = fn
	c.obsMu.Unlock()
}

// FireDesktopTurn 在指定 thread 上起一轮，但**不登记成手机自己发起的轮次**。
//
// 用途是自测：它的通知会走"桌面端发起"那条旁观链路（runFor 拿到 nil），
// 于是"电脑端发消息，手机能不能实时看到"这件事可以在没有桌面端的情况下验证。
//
// 回包要看：上一次自测就是死在这里 —— 请求被拒了，而我把回包丢掉，
// 结果"自测已发出"却什么都没发生。现在等它的结果，失败就把原因说出来。
func (c *Client) FireDesktopTurn(ctx context.Context, threadID, text string) error {
	if threadID == "" {
		return errors.New("缺少 thread")
	}
	if err := c.Ensure(ctx, ""); err != nil {
		return err
	}
	if c.ThreadBusy(threadID) {
		return errors.New("这条对话上一轮还没结束，稍后再试")
	}
	c.mu.Lock()
	c.nextID++
	id := "dz-" + strconv.Itoa(c.nextID)
	key := "s:" + id
	ch := make(chan rpcMessage, 1)
	c.pending[key] = pendingRoute{ch: ch}
	c.mu.Unlock()
	if err := c.write(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "turn/start",
		"params": map[string]any{
			"threadId": threadID,
			"input":    []map[string]any{{"type": "text", "text": text}},
		},
	}); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	select {
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("turn/start 被拒绝: %s", msg.Error.Message)
		}
		return nil
	case <-waitCtx.Done():
		return errors.New("turn/start 没有回包（超时）")
	}
}

// cachedInitResult 返回我们与 app-server 握手时拿到的 initialize 结果。
// 桌面端接入时用它代答，绕开"一条连接只能 initialize 一次"的限制。
func (c *Client) cachedInitResult() (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.initResult) == 0 {
		return nil, false
	}
	return c.initResult, true
}

func (c *Client) closeDesktop() {
	c.mu.Lock()
	conn := c.desktop
	c.desktop = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// handleDesktopMessage 处理桌面端发来的一行。
func (c *Client) handleDesktopMessage(msg rpcMessage) {
	hasID := len(msg.ID) > 0
	switch {
	case hasID && msg.Method != "":
		// initialize 这条连接上只能发一次（我们启动子进程时已经发过了）。
		// 桌面端这次直接由我们代答：用当初握手拿到的同一份结果回给它。
		// 转发给子进程只会拿到 Already initialized，桌面端会立刻断开。
		if msg.Method == "initialize" {
			if res, ok := c.cachedInitResult(); ok {
				c.forwardToDesktop(map[string]any{
					"jsonrpc": "2.0",
					"id":      json.RawMessage(msg.ID),
					"result":  json.RawMessage(res),
				})
				return
			}
		}
		// 桌面端发起的请求：换成内部 id 送进子进程，回答回来时再换回它的 id。
		c.mu.Lock()
		c.nextID++
		id := "cd-" + strconv.Itoa(c.nextID)
		c.pending["s:"+id] = pendingRoute{desktop: true, original: msg.ID}
		c.mu.Unlock()
		out := map[string]any{"jsonrpc": "2.0", "id": id, "method": msg.Method}
		if len(msg.Params) > 0 {
			out["params"] = json.RawMessage(msg.Params)
		}
		_ = c.write(out)

	case hasID:
		// 桌面端对某个服务端请求（审批等）的回答。抢到了才转给子进程。
		if !c.claimServerRequest(msg.ID) {
			return
		}
		out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID)}
		if msg.Error != nil {
			out["error"] = msg.Error
		} else {
			out["result"] = json.RawMessage(msg.Result)
		}
		_ = c.write(out)

	default:
		// 通知，原样往子进程送（initialized 这类）。
		out := map[string]any{"jsonrpc": "2.0", "method": msg.Method}
		if len(msg.Params) > 0 {
			out["params"] = json.RawMessage(msg.Params)
		}
		_ = c.write(out)
	}
}

// forwardToDesktop 把一条消息写给桌面端。没有桌面端时什么也不做。
func (c *Client) forwardToDesktop(v any) {
	c.mu.Lock()
	conn := c.desktop
	c.mu.Unlock()
	if conn == nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	c.deskWrite.Lock()
	defer c.deskWrite.Unlock()
	_, _ = conn.Write(raw)
}

// SetLaunchContext 记下桌面端启动时带来的那套启动条件：参数、环境变量、工作目录。
//
// 子进程还没起来就直接用它们起；已经起来而且在空闲，就换一条新的重开，
// 让桌面端看到和以前直接拉起 codex 时一模一样的运行环境
// （插件开关、code_mode_host、沙箱、git 路径这些都在里面）。
// 有轮次正在跑时不动它，宁可参数次优也不打断正在干活的对话。
func (c *Client) SetLaunchContext(ctx context.Context, args, env []string, cwd string) {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	// 只**记下**这套启动条件，供下次起子进程时使用；已经跑起来的就绝不动它。
	//
	// 2026-10-05 血的教训：桌面端会在两种参数之间来回切换（有时带插件开关、
	// 有时不带），而这里原本"参数变了就 Close+重开 app-server"。结果是每重连
	// 一次就杀一次子进程，正在跑的那一轮被拦腰掐断、turn/completed 永远不来，
	// 手机上就停在"正在生成"——用户那晚有一条对话卡了一个多小时。
	//
	// 之前还踩过一次同类的：拿 len(env)==0 判"没带环境"，桌面端每次都带完整
	// 环境，于是每连一次就重开一次，形成"连上就被踢"的死循环。
	// 两次的教训是同一条：**已经在跑的 app-server 不要为了参数好看去重启它**。
	argsChanged := len(args) > 0 && !sameArgs(c.extraArgs, args)
	if len(args) > 0 {
		c.extraArgs = args
	}
	if len(env) > 0 {
		c.extraEnv = env
	}
	if cwd != "" {
		c.extraCwd = cwd
	}
	if !argsChanged {
		return
	}
	if c.Ready() {
		// 正在跑：记下参数就够了，子进程保持不动（下次冷启动才会用上新参数）。
		return
	}
	// 还没起过，那就按新参数起。
	_ = c.startLocked(ctx, "")
}

func sameArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ExtraArgs 返回当前记着的桌面端参数（排查问题用）。
func (c *Client) ExtraArgs() []string {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return append([]string(nil), c.extraArgs...)
}
