// Package appserver 用官方 app-server 协议驱动本机 codex。
//
// 相比"每轮起一个 codex exec 进程再解析文本输出"，走协议有三个好处：
//   - 助手回复可以流式增量显示，不用等整段生成完
//   - 命令执行、文件改动是结构化数据，不再是猜文本
//   - Codex 需要审批时，能把请求发到手机上让用户点同意
//
// 传输方式：把 `codex app-server` 作为子进程拉起，用标准输入输出收发
// 行分隔的 JSON-RPC（已实测确认分帧方式）。
package appserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"codexmobile/internal/codex"
	"codexmobile/internal/netenv"
)

const (
	clientName    = "codex-mobile"
	clientVersion = "0.2.0"
)

// 审批结果。命令类与文件类共用这四个取值。
type Decision string

const (
	Accept           Decision = "accept"
	AcceptForSession Decision = "acceptForSession"
	Decline          Decision = "decline"
	Cancel           Decision = "cancel"
)

// Approval 是一条需要用户拍板的请求。
type Approval struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Reason string `json:"reason,omitempty"`
}

// Item 是会话里的一条条目。
type Item struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Text             string `json:"text"`
	Command          string `json:"command"`
	Status           string `json:"status"`
	ExitCode         *int   `json:"exitCode"`
	AggregatedOutput string `json:"aggregatedOutput"`
	Query            string `json:"query"`
	Path             string `json:"path"`
	Server           string `json:"server"`
	Tool             string `json:"tool"`
	Changes          []struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	} `json:"changes"`
}

// Turn 是一轮的收尾信息。
type Turn struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error"`
}

// Sink 接收一轮对话过程中发生的事。
type Sink struct {
	OnTurnStarted func(turnID string)
	OnDelta       func(text string)
	OnItemStarted func(item Item)
	OnItemDone    func(item Item)
	OnTurnDone    func(turn Turn)
	OnNotice      func(method string, params json.RawMessage)
	OnLog         func(text string)
	// OnApproval 会一直阻塞，直到用户做出选择；返回空表示拒绝。
	OnApproval func(req Approval) Decision
	// OnQuestion 会一直阻塞，直到用户在选项里挑完；返回空表示交给别人/放弃。
	OnQuestion func(qs []codex.Question) map[string][]string
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

var (
	errClosed   = errors.New("app-server 未启动")
	errBusy     = errors.New("上一轮还在进行中")
	approvalTTL = 10 * time.Minute
)

type turnRun struct {
	turnID   string
	threadID string
	sink     Sink
	text     strings.Builder
	done     chan struct{}
	once     sync.Once
}

func (r *turnRun) finish() { r.once.Do(func() { close(r.done) }) }

// pendingRoute 记着"我们发出去的这个请求是谁问的"，好把回答送回原处。
// 手机问的（desktop=false）送回等待的 channel；桌面端问的（desktop=true）
// 换回它原来的 id 再写回去。
type pendingRoute struct {
	desktop  bool
	original json.RawMessage
	ch       chan rpcMessage
	drop     bool // 自测用：回包直接丢掉，不送回任何一端
}

// idKey 把 JSON-RPC 的 id 归一化成 map 的键。协议允许 id 是数字或字符串，
// 我们发给子进程的一律是字符串（cm-* / cd-*），跟子进程自己生成的数字 id
// 天然错开，不会撞车。
func idKey(raw json.RawMessage) string {
	var s string
	if len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return "s:" + s
	}
	return "r:" + string(raw)
}

// Client 是一个常驻的 app-server 连接。
type Client struct {
	bin string

	writeMu sync.Mutex
	mu      sync.Mutex
	stdin   io.WriteCloser
	cmd     *exec.Cmd
	nextID  int
	pending map[string]pendingRoute
	closed  bool
	// runs 是"正在跑的轮次"，**按 thread 分开存**。
	//
	// 以前是一个 current：只要有一条对话在跑，手机就没法在另一条对话里说话
	// （服务端回"上一轮还在进行中"）。可 Codex 本来就是一条对话一个轮次，
	// 不同对话之间互不相干，所以这里改成按 thread 各存一份。
	runs map[string]*turnRun
	// turnOf 记着每条对话当前那一轮的轮次号。
	// 手机要"引导"正在跑的一轮时必须带上它（协议要求 expectedTurnId），
	// 而桌面端发起的轮次我们只从通知里知道，所以在这里单独记一份。
	turnOf map[string]string

	// 桌面版 Codex 的接入点。它被桌面端当成 CLI 拉起后（走 CODEX_CLI_PATH），
	// 会连到本进程，于是桌面端和手机共用这一个 app-server——Codex 的写入锁
	// 是"每个进程一把"，同一个进程里两个客户端就不会再互相抢锁。
	desktop   io.ReadWriteCloser
	deskWrite sync.Mutex

	// 服务端反向请求（审批等）的登记表：谁来答都行，先答的说了算。
	reqMu     sync.Mutex
	serverReq map[string]chan struct{}

	// 诊断用：反向请求的各种方法各见过几次，只记第一次（带完整参数）。
	// Codex 发来的方法里有一部分我们还没实现，那种情况手机上不会有任何提示，
	// 只能靠这份日志定位。见 logServerRequest。
	loggedReqMu sync.Mutex
	loggedReq   map[string]int

	// 旁观者。桌面端发起的轮次不经过任何一次 /api/send，手机原本什么都收不到；
	// 这里把"不是手机自己在跑的那条对话"的通知递出去（见 httpapi.OnThreadActivity）。
	obsMu    sync.Mutex
	observer func(threadID, method string, params json.RawMessage)
	// 电脑端发起的轮次要审批、要选择题时，手机也得能答 —— 这一对是兜底回调。
	askMu      sync.Mutex
	askGlobal  func(threadID string, req Approval) Decision
	quesGlobal func(threadID string, qs []codex.Question) map[string][]string

	// 桌面端启动时会带一串 -c 参数（比如 code_mode_host、插件开关）。
	// 子进程还没起来时把它们带上，让桌面端看到的环境和以前一致。
	extraArgs []string
	// 桌面版还会给自己拉起的 CLI 注入一批环境变量（沙箱、git、MCP node 路径…）。
	// 子进程由我们起，所以这些要由桌面端通过接入握手带过来。
	extraEnv []string
	extraCwd string

	readyMu sync.RWMutex
	isReady bool

	// 桌面端和手机共用**同一条**到 app-server 的 stdio 连接，而官方协议规定
	// "一条连接只能 initialize 一次"（重复 initialize 会得到 Already initialized）。
	// 我们自己的握手结果在这里留一份：桌面端再来 initialize 时照着回，
	// 不转给子进程。否则桌面端会当场收到 Already initialized 并断开连接，
	// 界面上就表现为"无法加载组织设置"。
	initResult json.RawMessage

	// app-server 常驻：本进程活着，它就活着；跑完一轮也不关。
	//
	// 以前是"用完就把子进程杀掉，好把这条对话的写入锁还给电脑端"。实测
	// （_probe/ui-check/idle-lock-probe.js）发现：Codex 的写入锁绑在"哪个进程
	// 加载了这条对话"上，thread/unsubscribe 不释放，只有进程退出才放。
	// 杀来杀去只会让服务变得不确定：每轮断一次，还得靠空档期挤进去。
	//
	// 现在的取舍是：服务不终止，手机端和电脑端都用同一个服务、同一条对话，
	// 谁都不用去抢锁。代价是电脑上另开的 Codex 进程（桌面版 / TUI）打不开
	// 这里已经加载过的对话——这是 Codex 自己的进程级锁，不是我们加的。
	startMu sync.Mutex
	cwd     string

	// 对话列表缓存：切页面时不用每次都问一遍协议
	cacheMu   sync.Mutex
	cacheAt   time.Time
	cacheList []ThreadSummary
	cacheNext string
}

func New(bin string) *Client {
	return &Client{
		bin:       bin,
		pending:   map[string]pendingRoute{},
		runs:      map[string]*turnRun{},
		turnOf:    map[string]string{},
		loggedReq: map[string]int{},
	}
}

// Ready 表示连接已经建立并完成握手。
func (c *Client) Ready() bool {
	c.readyMu.RLock()
	defer c.readyMu.RUnlock()
	return c.isReady
}

func (c *Client) setReady(v bool) {
	c.readyMu.Lock()
	c.isReady = v
	c.readyMu.Unlock()
}

// Start 拉起 app-server 子进程并完成握手。
func (c *Client) Start(ctx context.Context, cwd string) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	return c.startLocked(ctx, cwd)
}

// startLocked 真正把子进程拉起来；调用者必须已经拿到 startMu。
func (c *Client) startLocked(ctx context.Context, cwd string) error {
	if cwd == "" {
		cwd = c.cwd
	}
	if cwd == "" {
		cwd = c.extraCwd
	}
	c.cwd = cwd
	cmd := exec.Command(c.bin, c.childArgs()...)
	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		}
	}
	// 环境变量：桌面端连上来过就用它带过来的那份（保真），否则用本进程的。
	// 两种情况都要先清掉代理 —— 这台机器上挂着 Clash，而服务本身已经接了
	// Tailscale，再经一层代理只会带来抖动和不确定的路由。
	env := netenv.Clean(c.extraEnv)
	// 子进程眼里"codex 在哪"。桌面版拉起我们时也设了这个变量，而它指向的是
	// 我们自己 —— 不纠正的话，codex 自己再往下拉子进程就可能绕回本程序。
	env = netenv.Set(env, "CODEX_CLI_PATH", c.bin)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 app-server 失败: %w", err)
	}

	c.writeMu.Lock()
	c.stdin = stdin
	c.writeMu.Unlock()
	c.mu.Lock()
	c.cmd = cmd
	c.closed = false
	// 换了新子进程，旧的等待表就作废了。
	c.pending = map[string]pendingRoute{}
	// 旧子进程里正在跑的那些轮次也不可能再收到结束了，一并收尾。
	stale := c.runs
	c.runs = map[string]*turnRun{}
	c.mu.Unlock()
	for _, r := range stale {
		r.finish()
	}

	go c.readLoop(stdout)
	go c.drainStderr(stderr)

	var rawInit json.RawMessage
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": clientName, "version": clientVersion},
		"capabilities": map[string]any{"experimentalApi": true},
	}, &rawInit); err != nil {
		_ = c.Close()
		return fmt.Errorf("app-server 握手失败: %w", err)
	}
	// 握手结果留一份，供桌面端接入时代答（见 initResult 的说明）。
	c.mu.Lock()
	c.initResult = rawInit
	c.mu.Unlock()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		_ = c.Close()
		return err
	}
	c.setReady(true)
	// 通知旁观者：换了一条新的 app-server。
	// 旧进程被换掉时，正在跑的那一轮不会有 turn/completed，
	// 界面上的"正在生成"必须靠这条消息收回去，否则会一直转圈。
	c.notifyObserver("", "appserver/started", nil)
	return nil
}

// childArgs 决定子进程怎么起。
//
// 默认是 `codex app-server`；如果桌面版 Codex 连上来了，就用它原来的那串参数
// 起（`-c features... app-server --analytics...`），这样桌面端看到的运行环境和
// 以前直接拉起 codex 时一模一样。
func (c *Client) childArgs() []string {
	if len(c.extraArgs) > 0 {
		return append([]string(nil), c.extraArgs...)
	}
	return []string{"app-server"}
}

// Ensure 保证 app-server 在跑。它常驻不关，但万一崩了（或者被外面杀掉），
// 这里会把它拉起来；每次真正要用之前都过一下，最稳。
func (c *Client) Ensure(ctx context.Context, cwd string) error {
	if c.Ready() {
		return nil
	}
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if c.Ready() {
		return nil
	}
	return c.startLocked(ctx, cwd)
}

// SetDefaultCwd 记住默认工作目录。
//
// 现在子进程是"用到了才起"，各入口调 Ensure 时不一定带着工作目录，
// 启动时先记一份，保证子进程落在一个合理的位置。
func (c *Client) SetDefaultCwd(dir string) {
	c.startMu.Lock()
	c.cwd = dir
	c.startMu.Unlock()
}

// Close 关闭子进程（只在整个程序退出时调用）。
func (c *Client) Close() error {
	c.setReady(false)
	c.mu.Lock()
	c.closed = true
	cmd := c.cmd
	c.mu.Unlock()

	c.writeMu.Lock()
	if c.stdin != nil {
		_ = c.stdin.Close()
		c.stdin = nil
	}
	c.writeMu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		return cmd.Wait()
	}
	return nil
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
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
		c.dispatch(msg)
	}
	c.setReady(false)
	c.mu.Lock()
	left := c.runs
	c.runs = map[string]*turnRun{}
	c.mu.Unlock()
	for _, r := range left {
		r.finish()
	}
	// 子进程没了，桌面端那边也救不回来：把连接关掉，让它看到明确的失败，
	// 而不是一直干等。
	c.closeDesktop()
}

func (c *Client) dispatch(msg rpcMessage) {
	hasID := len(msg.ID) > 0
	switch {
	case hasID && msg.Method == "":
		c.routeResponse(msg)
	case hasID:
		// 服务端反过来请求我们（审批就走这条路）。
		c.routeServerRequest(msg)
	case msg.Method != "":
		c.handleNotification(msg)
	}
}

// routeResponse 把子进程的回答送回当初提问的那一方。
func (c *Client) routeResponse(msg rpcMessage) {
	key := idKey(msg.ID)
	c.mu.Lock()
	route, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	if route.drop {
		return
	}
	if route.desktop {
		reply := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(route.original)}
		if msg.Error != nil {
			reply["error"] = msg.Error
		} else {
			reply["result"] = json.RawMessage(msg.Result)
		}
		c.forwardToDesktop(reply)
		return
	}
	select {
	case route.ch <- msg:
	default:
	}
}

// routeServerRequest 处理子进程反向发起的请求（审批、要用户补充输入等）。
// 桌面端和手机都收到同一份，谁先答谁算数，另一侧会收到"已被处理"的通知。
func (c *Client) routeServerRequest(msg rpcMessage) {
	key := idKey(msg.ID)
	done := make(chan struct{})
	c.reqMu.Lock()
	if c.serverReq == nil {
		c.serverReq = map[string]chan struct{}{}
	}
	c.serverReq[key] = done
	c.reqMu.Unlock()

	// 桌面端优先：它自己那套审批界面本来就会答，不需要手机插手。
	c.forwardToDesktop(msg)
	go c.answerServerRequest(msg, done)
}

// claimServerRequest 声明"这个服务端请求我接了"。返回 false 说明别人已经接过。
func (c *Client) claimServerRequest(raw json.RawMessage) bool {
	key := idKey(raw)
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	done, ok := c.serverReq[key]
	if !ok {
		return false
	}
	delete(c.serverReq, key)
	close(done)
	return true
}

// anyRun 随便挑一条正在跑的（日志这类全局输出用它）。
func (c *Client) anyRun() *turnRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.runs {
		return r
	}
	return nil
}

// runFor 找出"这条通知属于手机正在跑的那一轮吗"。
// 桌面端和手机共用同一个 app-server，桌面端那条对话的流式内容也会从这里经过，
// 不带这个过滤，手机上就会看到电脑在别处打字的内容混进自己的回复里。
func (c *Client) runFor(threadID string) *turnRun {
	if threadID == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[threadID]
}

func threadIDOf(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &p)
	return p.ThreadID
}

func (c *Client) handleNotification(msg rpcMessage) {
	// 桌面端挂着的话，通知原样转给它 —— 它的界面要实时看到同一条对话。
	c.forwardToDesktop(msg)

	threadID := threadIDOf(msg.Params)

	// 旁观者：桌面端发起的那一轮不属于任何一次 /api/send，
	// 手机那边原本一个字都收不到（要等下次发消息才被动补拉历史）。
	// 判断条件用 runFor：手机自己在跑的那条，下面的 sink 已经在推了，
	// 不能重复推第二遍。
	if threadID != "" && c.runFor(threadID) == nil {
		c.notifyObserver(threadID, msg.Method, msg.Params)
	}

	run := c.runFor(threadID)
	var sink Sink
	if run != nil {
		sink = run.sink
	}
	logf := func(text string) {
		if sink.OnLog != nil {
			sink.OnLog(text)
		}
	}

	switch msg.Method {
	case "item/agentMessage/delta":
		var p struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if run != nil && p.Delta != "" {
			run.text.WriteString(p.Delta)
		}
		if sink.OnDelta != nil && p.Delta != "" {
			sink.OnDelta(p.Delta)
		}
	case "item/started", "item/completed":
		var p struct {
			Item Item `json:"item"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if msg.Method == "item/started" {
			if sink.OnItemStarted != nil {
				sink.OnItemStarted(p.Item)
			}
			return
		}
		if p.Item.Type == "agentMessage" && run != nil && p.Item.Text != "" {
			run.text.Reset()
			run.text.WriteString(p.Item.Text)
		}
		if sink.OnItemDone != nil {
			sink.OnItemDone(p.Item)
		}
	case "turn/started":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     Turn   `json:"turn"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.ThreadID != "" && p.Turn.ID != "" {
			c.mu.Lock()
			c.turnOf[p.ThreadID] = p.Turn.ID
			c.mu.Unlock()
		}
		if run != nil && p.Turn.ID != "" {
			run.turnID = p.Turn.ID
		}
		if sink.OnTurnStarted != nil {
			sink.OnTurnStarted(p.Turn.ID)
		}
	case "turn/completed":
		var p struct {
			Turn Turn `json:"turn"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if threadID != "" {
			c.mu.Lock()
			delete(c.turnOf, threadID)
			c.mu.Unlock()
		}
		if sink.OnTurnDone != nil {
			sink.OnTurnDone(p.Turn)
		}
		if run != nil {
			run.finish()
		}
	case "error":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Message == "" {
			p.Message = string(msg.Params)
		}
		logf(p.Message)
	case "warning":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if p.Message != "" && !IsNoise(p.Message) {
			logf(p.Message)
		}
	default:
		if sink.OnNotice != nil {
			sink.OnNotice(msg.Method, msg.Params)
		}
	}
}

// notifyObserver 把一条消息递给旁观者（没有旁观者就什么也不做）。
// 回调跑在收报文的 goroutine 上，所以它必须立刻返回。
func (c *Client) notifyObserver(threadID, method string, params json.RawMessage) {
	c.obsMu.Lock()
	obs := c.observer
	c.obsMu.Unlock()
	if obs != nil {
		obs(threadID, method, params)
	}
}

// answerServerRequest 处理服务端的反向请求，主要是审批。
//
// 桌面端在线时这里是"抢答"：我们只接审批这类手机上会弹卡片的请求，
// 其余的（登录刷新、动态工具、补充输入）原样留给桌面端，别替它答。
// logServerRequest 把子进程反向发起的请求记一笔日志。
//
// 为什么需要它：这类请求有十来种，我们只实现了其中一部分（命令审批、
// 文件审批、选择题、MCP 询问），其余的原样留给桌面端。问题在于
// "留给桌面端"在没有桌面端的时候等于**什么都不回**，Codex 只能自己退回去，
// 表现就是它告诉你"这个我做不到，请你手动执行" —— 而手机界面上从头到尾
// 不会出现任何提示，因为压根没有渲染它的代码。记下方法名和原始参数，
// 这种"看不见的请求"才定位得到。
//
// 同一种方法只记第一次，避免 currentTime/read 这类可能每轮都来的请求把日志刷满。
func (c *Client) logServerRequest(method string, params json.RawMessage, ok bool) {
	if method == "" {
		return
	}
	c.loggedReqMu.Lock()
	if c.loggedReq == nil {
		c.loggedReq = map[string]int{}
	}
	c.loggedReq[method]++
	first := c.loggedReq[method] == 1
	c.loggedReqMu.Unlock()
	if !first {
		return
	}
	state := "已接住"
	if !ok {
		state = "未实现，手机上不会有提示"
	}
	body := string(params)
	const maxBody = 4000
	if len(body) > maxBody {
		body = body[:maxBody] + "…（已截断）"
	}
	log.Printf("[协议诊断] 反向请求 method=%s（%s） params=%s", method, state, body)
}

// done 会在别的客户端抢先回答时被关闭，这时什么都不做。
func (c *Client) answerServerRequest(msg rpcMessage, done chan struct{}) {
	result, err := c.buildServerRequestResult(msg, done)
	// 诊断：不论有没有接住都记一笔（每种方法只记第一次）。
	// 没接住的那种在手机界面上完全没有反应，只有这里能看见。
	c.logServerRequest(msg.Method, msg.Params, err == nil)
	if err != nil {
		if c.HasDesktop() {
			return
		}
		_ = c.write(map[string]any{
			"jsonrpc": "2.0",
			"id":      msg.ID,
			"error":   map[string]any{"code": -32601, "message": err.Error()},
		})
		return
	}
	if result == nil {
		// 交给桌面端，或者用户根本没答。
		return
	}
	if !c.claimServerRequest(msg.ID) {
		return
	}
	_ = c.write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
}

func (c *Client) buildServerRequestResult(msg rpcMessage, done chan struct{}) (any, error) {
	switch msg.Method {
	case "item/commandExecution/requestApproval", "execCommandApproval":
		var p struct {
			Command  string `json:"command"`
			Reason   string `json:"reason"`
			ItemID   string `json:"itemId"`
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if !c.ownsThread(p.ThreadID) {
			return nil, nil
		}
		if p.Command == "" {
			p.Command = "（未提供命令内容）"
		}
		decision := c.ask(Approval{
			ID: string(msg.ID), Kind: "command",
			Title: "请求执行命令", Detail: p.Command, Reason: p.Reason,
		}, done, p.ThreadID)
		if decision == "" {
			return nil, nil
		}
		return map[string]any{"decision": string(decision)}, nil

	case "item/fileChange/requestApproval", "applyPatchApproval":
		var p struct {
			Reason    string  `json:"reason"`
			GrantRoot *string `json:"grantRoot"`
			ThreadID  string  `json:"threadId"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		if !c.ownsThread(p.ThreadID) {
			return nil, nil
		}
		detail := "Codex 想修改工作目录里的文件"
		if p.GrantRoot != nil && *p.GrantRoot != "" {
			detail = "Codex 想修改：" + *p.GrantRoot
		}
		decision := c.ask(Approval{
			ID: string(msg.ID), Kind: "fileChange",
			Title: "请求修改文件", Detail: detail, Reason: p.Reason,
		}, done, p.ThreadID)
		if decision == "" {
			return nil, nil
		}
		return map[string]any{"decision": string(decision)}, nil

	case "mcpServer/elicitation/request":
		if c.HasDesktop() {
			return nil, nil
		}
		return map[string]any{"action": "decline"}, nil

	case "item/tool/requestUserInput":
		var p struct {
			ThreadID  string `json:"threadId"`
			Questions []struct {
				ID       string `json:"id"`
				Header   string `json:"header"`
				Question string `json:"question"`
				Options  []struct {
					Label       string `json:"label"`
					Description string `json:"description"`
				} `json:"options"`
				IsOther bool `json:"isOther"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		qs := make([]codex.Question, 0, len(p.Questions))
		for _, q := range p.Questions {
			item := codex.Question{ID: q.ID, Header: q.Header, Text: q.Question, Other: q.IsOther}
			for _, o := range q.Options {
				item.Options = append(item.Options, codex.QuestionOption{Label: o.Label, Description: o.Description})
			}
			qs = append(qs, item)
		}
		if len(qs) == 0 {
			return nil, nil
		}
		answers := c.askQuestions(qs, done, p.ThreadID)
		if answers == nil {
			if c.HasDesktop() {
				return nil, nil // 桌面端会答（它那边也有这张卡）
			}
			return map[string]any{"answers": map[string]any{}}, nil
		}
		out := map[string]any{}
		for id, list := range answers {
			out[id] = map[string]any{"answers": list}
		}
		return map[string]any{"answers": out}, nil

	default:
		return nil, fmt.Errorf("暂不支持的请求: %s", msg.Method)
	}
}

// ownsThread 判断某个请求是不是落在手机正在跑的那条对话上。
// 桌面端自己那条对话的审批，不该弹到手机上。
func (c *Client) ownsThread(threadID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.runs) == 0 {
		return false
	}
	if threadID == "" {
		return true
	}
	return c.runs[threadID] != nil
}

// ask 把审批推到手机上等答复。返回空字符串表示"别管了"：
// 要么桌面端抢先处理了，要么这条对话跟手机正在跑的无关。
func (c *Client) ask(req Approval, done chan struct{}, threadID string) Decision {
	run := c.runFor(threadID)
	sink := Sink{}
	if run != nil {
		sink = run.sink
	}
	fn := sink.OnApproval
	if fn == nil {
		// 不是手机自己发起的那一轮（电脑端发起的）：走兜底回调，
		// 这样人不在电脑前也能在手机上点"允许/拒绝"。
		c.askMu.Lock()
		g := c.askGlobal
		c.askMu.Unlock()
		if g == nil {
			return ""
		}
		fn = func(r Approval) Decision { return g(threadID, r) }
	}
	if fn == nil {
		return ""
	}
	answer := make(chan Decision, 1)
	go func() { answer <- fn(req) }()
	select {
	case d := <-answer:
		if d == "" {
			return Decline
		}
		return d
	case <-done:
		return ""
	case <-time.After(approvalTTL):
		return ""
	}
}

// askQuestions 把选择题推到手机等回答。没人答/别人先答了返回 nil。
func (c *Client) askQuestions(qs []codex.Question, done chan struct{}, threadID string) map[string][]string {
	run := c.runFor(threadID)
	sink := Sink{}
	if run != nil {
		sink = run.sink
	}
	fn := sink.OnQuestion
	if fn == nil {
		c.askMu.Lock()
		g := c.quesGlobal
		c.askMu.Unlock()
		if g == nil {
			return nil
		}
		fn = func(list []codex.Question) map[string][]string { return g(threadID, list) }
	}
	if fn == nil {
		return nil
	}
	answer := make(chan map[string][]string, 1)
	go func() { answer <- fn(qs) }()
	select {
	case a := <-answer:
		return a
	case <-done:
		return nil
	case <-time.After(approvalTTL):
		return nil
	}
}

// SetFallbackAsker 注册"电脑端那一轮"的审批 / 选择题回调。
func (c *Client) SetFallbackAsker(
	approval func(threadID string, req Approval) Decision,
	question func(threadID string, qs []codex.Question) map[string][]string,
) {
	c.askMu.Lock()
	c.askGlobal = approval
	c.quesGlobal = question
	c.askMu.Unlock()
}

func (c *Client) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return errClosed
	}
	_, err = c.stdin.Write(raw)
	return err
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errClosed
	}
	c.nextID++
	id := fmt.Sprintf("cm-%d", c.nextID)
	key := "s:" + id
	ch := make(chan rpcMessage, 1)
	c.pending[key] = pendingRoute{ch: ch}
	c.mu.Unlock()

	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		payload["params"] = params
	}
	if err := c.write(payload); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return ctx.Err()
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("%s 被拒绝: %s", method, msg.Error.Message)
		}
		if out != nil && len(msg.Result) > 0 {
			return json.Unmarshal(msg.Result, out)
		}
		return nil
	}
}

// EnsureThread 复用已有线程；线程不在（或复用失败）就新建一个。
//
// 唯一的例外是"这条对话正被别的客户端占着写"（一般是在电脑上的 Codex 里开着）。
// Codex 同一时间只允许一个客户端写一条对话，这种时候不能悄悄另起一条：
// 手机上会凭空多出一条同名对话，之后两边看起来就是"两个重复的对话"。
func (c *Client) EnsureThread(ctx context.Context, existing, cwd, sandbox, approval string) (string, error) {
	if err := c.Ensure(ctx, cwd); err != nil {
		return "", err
	}
	if existing != "" {
		if id, err := c.ResumeThread(ctx, existing, cwd, sandbox, approval); err == nil && id != "" {
			return id, nil
		} else if err != nil && strings.Contains(err.Error(), "active writer") {
			return "", fmt.Errorf("这条对话现在正在电脑上用（Codex 同时只允许一边写它），先在电脑上结束或关掉它，再从这里继续")
		}
	}
	return c.StartThread(ctx, cwd, sandbox, approval)
}

func (c *Client) StartThread(ctx context.Context, cwd, sandbox, approval string) (string, error) {
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.call(ctx, "thread/start", map[string]any{
		"cwd":            cwd,
		"sandbox":        sandbox,
		"approvalPolicy": approval,
	}, &res); err != nil {
		return "", err
	}
	return res.Thread.ID, nil
}

func (c *Client) ResumeThread(ctx context.Context, threadID, cwd, sandbox, approval string) (string, error) {
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := c.call(ctx, "thread/resume", map[string]any{
		"threadId":       threadID,
		"cwd":            cwd,
		"sandbox":        sandbox,
		"approvalPolicy": approval,
	}, &res); err != nil {
		return "", err
	}
	if res.Thread.ID == "" {
		return threadID, nil
	}
	return res.Thread.ID, nil
}

// Result 是一轮跑完后的结果。
type Result struct {
	Text   string
	Status string
	TurnID string
}

// Attachment 是跟着一条消息一起发出去的文件。
//
// 图片走 localImage（模型直接看图）；其它文件走 mention（相当于桌面上
// 把文件 @ 进对话，模型需要时自己去读）。
type Attachment struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Image bool   `json:"image"`
}

// inputs 把"文字 + 附件"拼成协议要的 input 数组。
func buildInputs(text string, atts []Attachment) []map[string]any {
	out := make([]map[string]any, 0, len(atts)+1)
	if text != "" {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	for _, a := range atts {
		if a.Path == "" {
			continue
		}
		if a.Image {
			out = append(out, map[string]any{"type": "localImage", "path": a.Path})
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Path
		}
		out = append(out, map[string]any{"type": "mention", "name": name, "path": a.Path})
	}
	return out
}

// RunTurn 发起一轮对话并等待它结束。
func (c *Client) RunTurn(ctx context.Context, threadID, text string, sink Sink) (Result, error) {
	return c.RunTurnWith(ctx, threadID, text, nil, sink)
}

// RunTurnWith 和 RunTurn 一样，只是可以带上图片/文件。
func (c *Client) RunTurnWith(ctx context.Context, threadID, text string, atts []Attachment, sink Sink) (Result, error) {
	run := &turnRun{sink: sink, threadID: threadID, done: make(chan struct{})}
	c.mu.Lock()
	// 只有**同一条对话**才不允许并发：不同对话各跑各的，本来就互不相干。
	if c.runs[threadID] != nil {
		c.mu.Unlock()
		return Result{}, errBusy
	}
	c.runs[threadID] = run
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.runs[threadID] == run {
			delete(c.runs, threadID)
		}
		c.mu.Unlock()
	}()

	var res struct {
		Turn Turn `json:"turn"`
	}
	if err := c.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    buildInputs(text, atts),
	}, &res); err != nil {
		return Result{}, err
	}
	if res.Turn.ID != "" {
		run.turnID = res.Turn.ID
		c.mu.Lock()
		c.turnOf[threadID] = res.Turn.ID
		c.mu.Unlock()
	}

	select {
	case <-ctx.Done():
		return Result{Text: run.text.String(), TurnID: run.turnID}, ctx.Err()
	case <-run.done:
		return Result{Text: run.text.String(), TurnID: run.turnID}, nil
	}
}

// ActiveTurnID 返回这条对话当前那一轮的轮次号（没有就是空）。
func (c *Client) ActiveTurnID(threadID string) string {
	if threadID == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.runs[threadID]; r != nil && r.turnID != "" {
		return r.turnID
	}
	return c.turnOf[threadID]
}

// SteerTurn 往正在跑的那一轮里**插一句话**（手机上的"引导"）。
//
// 协议要求带上 expectedTurnId，对不上会被拒 —— 那说明这一轮刚好结束了，
// 这时候让调用方改成正常发一条就好。
func (c *Client) SteerTurn(ctx context.Context, threadID, text string) error {
	turnID := c.ActiveTurnID(threadID)
	if turnID == "" {
		return errors.New("这条对话现在没有正在进行的轮次")
	}
	return c.call(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input":          []map[string]any{{"type": "text", "text": text}},
	}, nil)
}

// Interrupt 中断当前轮次。
func (c *Client) Interrupt(ctx context.Context, threadID string) error {
	run := c.runFor(threadID)
	if run == nil || run.turnID == "" {
		return errors.New("这条对话没有正在进行的轮次")
	}
	return c.call(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID, "turnId": run.turnID,
	}, nil)
}

// Busy 表示是否有任何轮次在跑。
func (c *Client) Busy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.runs) > 0
}

// ThreadBusy 表示**这条对话**有没有轮次在跑。
// 手机在另一条对话里发消息时，靠它放行。
func (c *Client) ThreadBusy(threadID string) bool {
	if threadID == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[threadID] != nil
}

func (c *Client) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || IsNoise(line) {
			continue
		}
		if run := c.anyRun(); run != nil && run.sink.OnLog != nil {
			run.sink.OnLog(line)
		}
	}
}

// IsNoise 过滤 codex 内部组件的常规日志，避免手机上被刷屏。
func IsNoise(text string) bool {
	markers := []string{
		"codex_core_plugins::", "codex_core::shell_snapshot", "codex_models_manager::model_info",
		"codex_mcp::rmcp_client", "codex_skills::interface", "rmcp::transport::worker",
		"failed to initialize MCP client", "MCP server startup failed",
		"unrecognized configuration setting", "Under-development features",
	}
	low := strings.ToLower(text)
	for _, m := range markers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	upper := strings.ToUpper(text)
	for _, level := range []string{" WARN ", " INFO ", " DEBUG ", " TRACE "} {
		idx := strings.Index(upper, level)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(text[idx+len(level):])
		module := rest
		if cut := strings.IndexAny(rest, " :"); cut >= 0 {
			module = rest[:cut]
		}
		m := strings.ToLower(module)
		if strings.HasPrefix(m, "codex_") || strings.HasPrefix(m, "rmcp") || strings.HasPrefix(m, "mcp") {
			return true
		}
	}
	return false
}
