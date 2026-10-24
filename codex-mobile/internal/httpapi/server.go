// Package httpapi 提供手机界面需要的全部接口。
//
// 接口清单与旧的 Node 版本一一对应（开发文档 §10.2），
// 所以现有的手机界面可以原样使用，不需要改动。
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codexmobile/internal/appserver"
	"codexmobile/internal/codex"
	"codexmobile/internal/notify"
	"codexmobile/internal/store"
	"codexmobile/internal/sysmon"
	"codexmobile/internal/taskwatch"
)

// proxyCookie 是给服务反代用的通行证。反代出来的页面要加载的静态资源
// 都是不带口令的地址，靠这个 cookie 才能过门；限定 /proxy/ 路径，
// 并且是 HttpOnly，页面里的脚本读不到口令本身。
const proxyCookie = "codex_proxy"

type Server struct {
	DB         *store.DB
	Runner     *codex.Runner
	App        *appserver.Client
	Token      string
	Web        fs.FS
	DataDir    string
	DefaultCwd string
	StartedAt  time.Time
	// 扫码页需要的信息
	QRPNG     []byte
	PairPNG   []byte
	PublicURL string
	LANURL    string
	LocalURL  string
	// LANPort 是局域网直连用的端口（和本机调试入口同一个监听）。
	// 手机在同一个 WiFi 下会优先连 http://<本机局域网IP>:<LANPort>/。
	LANPort string
	PairURI string
	// UpdateDir 里放发布好的 APK 和 version.json，供手机端检查更新
	UpdateDir string
	// APKPNG 是"直接下载安装包"的二维码（用手机相机扫一下就能装）
	APKPNG []byte
	// Feed 是 ntfy 频道的本地缓存（电脑替手机守着，避免消息过期丢失）
	Feed *notify.Feed
	// Tasks 盯着计划任务的产出目录，有新日报 / 精读 / 周报就播报到手机
	Tasks *taskwatch.Watcher
	// Mon 是这台电脑的运行状态（CPU / 内存 / 磁盘 / 网络），「服务」里点开就能看
	Mon *sysmon.Collector
	// TSClient 是走内嵌 tailnet 网络栈的 HTTP 客户端。访问内网 / 组网里的
	// 服务（反向代理、服务健康检查）一律用它：这台电脑就算没装 Tailscale，
	// 也能连上同一 tailnet 里的机器。
	TSClient *http.Client
	// Build 是"这台电脑上跑的是哪一份程序"的标识。手机界面发现它变了
	// 就自动重新加载页面 —— 换了界面不用再让用户手动刷新。
	Build string
	// TSDial 是同一个网络栈的拨号器，SSH 这类非 HTTP 的连接走它。
	TSDial func(ctx context.Context, network, addr string) (net.Conn, error)

	approvalMu sync.Mutex
	approvals  map[string]chan appserver.Decision
	// approvalInfo 是每条待处理审批的摘要，专门给 /api/status 用。
	// 手机不在那条对话上（或者刚从后台回来、错过了一次推送）时，
	// 靠它下一次轮询就能把"有人在等你拍板"补出来。
	approvalInfo map[string]pendingApproval
	// 选择题：手机把选项挑好之后从这里回给正在等的那一轮
	questionMu sync.Mutex
	questions  map[string]chan map[string][]string

	// 手机离线发件箱补发时带去的幂等号：同一个 clientMsgId 只允许发起一轮。
	// 没它的话，第一次请求其实已在电脑上跑、只是响应没回到手机，补发就会
	// 把同一句话再跑一遍。只记半小时，补发窗口远小于此。
	clientMu   sync.Mutex
	clientSeen map[string]int64
	// clientLoaded 表示"已经试着从磁盘读过一次" —— 去重标记要落盘，
	// 否则服务一重启就全丢（每次换装都会重启），手机那边还留着发件箱，
	// 下一次补发就会被当成新消息收下。
	clientLoaded bool

	// 正在跑的那一轮。手机随时可能离开界面，这一轮照样在电脑上跑完，
	// 界面靠这个状态显示"正在生成回答"。
	runMu sync.Mutex
	run   *store.RunInfo
	// deskRun 是"电脑那边发起的轮次"。和 run 分开存：
	// 手机自己也可能在别的对话上发消息，两者不能互相覆盖。
	deskRun *store.RunInfo
	// deskSeen 是这条电脑端轮次最近一次有动静的时间，用来判断它是不是还活着。
	deskSeen int64

	// act 是"每条对话最近动没动"的记账本。
	//
	// 它不是拿来推送的，而是给手机一个很轻的问法："我这条对话变了没、还在跑吗"。
	// 手机每秒问一次，变了就把内容读回来 —— 这是跟 DusuWorks/codex-webui 学的
	// "轻量校准"：不依赖事件一定送达，断线、丢事件、来回切页面都能自己补上。
	actMu sync.Mutex
	act   map[string]*threadActivity

	// 界面订阅的实时事件（手机和电脑两端都挂在这上面）
	liveMu   sync.Mutex
	live     map[int]*subscriber
	liveNext int
}

// beginRun / endRun 标记某一轮正在生成。
func (s *Server) beginRun(sessionID string) {
	s.runMu.Lock()
	s.run = &store.RunInfo{SessionID: sessionID, StartedAt: time.Now().UnixMilli()}
	s.runMu.Unlock()
}

// setRunThread 补上 thread 号（协议模式要握手之后才知道）。
func (s *Server) setRunThread(threadID string) {
	s.runMu.Lock()
	if s.run != nil && threadID != "" {
		s.run.ThreadID = threadID
	}
	s.runMu.Unlock()
}

func (s *Server) endRun() {
	s.runMu.Lock()
	s.run = nil
	s.runMu.Unlock()
}

// setDeskRun / clearDeskRun 记录"电脑那边正在跑哪条对话"。
func (s *Server) setDeskRun(sessionID, threadID string) {
	s.runMu.Lock()
	s.deskRun = &store.RunInfo{
		SessionID: sessionID, ThreadID: threadID, StartedAt: time.Now().UnixMilli(),
	}
	s.deskSeen = time.Now().UnixMilli()
	s.runMu.Unlock()
}

func (s *Server) clearDeskRun(threadID string) bool {
	s.runMu.Lock()
	cleared := false
	if s.deskRun != nil && (threadID == "" || s.deskRun.ThreadID == threadID) {
		s.deskRun = nil
		s.deskSeen = 0
		cleared = true
	}
	s.runMu.Unlock()
	return cleared
}

// touchDeskRun 这条对话有活动，就给"电脑端正在跑"的记录续命。
func (s *Server) touchDeskRun(threadID string) {
	s.runMu.Lock()
	if s.deskRun != nil && (threadID == "" || s.deskRun.ThreadID == threadID) {
		s.deskSeen = time.Now().UnixMilli()
	}
	s.runMu.Unlock()
}

// deskRunState 只回"电脑那边在跑的那条"，不受手机自己那一轮影响。
//
// runState() 是给旧调用留的（手机那轮优先）。手机要判断"我正在看的这条在不在跑"，
// 必须两条都能看见：否则手机在别的对话上跑一轮时，这条的反馈会整段消失。
func (s *Server) deskRunState() *store.RunInfo {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.deskRun == nil {
		return nil
	}
	if !s.threadAliveIn(s.deskRun.ThreadID, deskRunIdleGrace) {
		s.deskRun, s.deskSeen = nil, 0
		return nil
	}
	c := *s.deskRun
	return &c
}

// deskRunIdleGrace 是"电脑端那一轮多久没动静就当它没在跑了"。
//
// 以前是 3 分钟，太短：一轮长任务卡在一条命令上十几分钟不发任何通知很正常，
// 一到点手机上就"明明在跑却看着像停了"，切一次对话更是再也找不回来。
//
// 放宽不等于放任：手机端会拿线程自己的状态（inProgress / completed）去核对，
// 见 web/index.html 的 applyThreadRun —— 那是权威答案，通知丢没丢都不影响。
// 这里的宽限只是"连手机都不在跟前看"时的最后一道兜底。
const deskRunIdleGrace = 60 * time.Minute

func (s *Server) runState() *store.RunInfo {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	// 手机自己发起的优先：那条有完整的流式输出。
	if s.run != nil {
		c := *s.run
		return &c
	}
	if s.deskRun != nil {
		// 兜底：正常情况下 turn/completed 会把状态收回去。
		// 万一那条通知丢了（子进程被换、连接断、轮次被掐断），别让手机上
		// 转圈转到天荒地老 —— 判据用"这条对话最近还有没有动静"：
		// 真在跑的时候通知不断，时间戳一直在走；停了就不再更新。
		stale := !s.threadAliveIn(s.deskRun.ThreadID, deskRunIdleGrace)
		if stale {
			s.deskRun, s.deskSeen = nil, 0
			return nil
		}
		c := *s.deskRun
		return &c
	}
	return nil
}

// WatchUpdates 盯着更新通道：一旦发布了新版本，就把这次更新的内容
// 写进「通知」里，并顺手推给用户配好的第三方渠道。
// 这样"发布了什么"和"任务跑完了"一样，都能在手机上看到。
func (s *Server) WatchUpdates(ctx context.Context) {
	if s.Feed == nil {
		return
	}
	s.checkPublished()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.checkPublished()
		}
	}
}

func (s *Server) checkPublished() {
	raw, err := os.ReadFile(filepath.Join(s.UpdateDir, "version.json"))
	if err != nil {
		return
	}
	var info struct {
		VersionCode int    `json:"versionCode"`
		VersionName string `json:"versionName"`
		Notes       string `json:"notes"`
	}
	if json.Unmarshal(raw, &info) != nil || info.VersionCode <= 0 {
		return
	}
	id := "update-" + strconv.Itoa(info.VersionCode)
	if s.Feed.Has(id) {
		return
	}
	body := strings.TrimSpace(info.Notes)
	if body == "" {
		body = "在「我的」里点检查更新就能装上。"
	} else {
		body += "\n\n在「我的」里点检查更新就能装上。"
	}
	title := "新版本 v" + info.VersionName + " 已发布"
	if !s.Feed.AddLocal(id, title, body, 0) {
		return
	}
	if s.DB != nil {
		_, _ = notify.Send(s.DB.NotifyConfig(), title, body)
	}
}

func (s *Server) initApprovals() {
	s.approvalMu.Lock()
	if s.approvals == nil {
		s.approvals = map[string]chan appserver.Decision{}
	}
	s.approvalMu.Unlock()
}

// pendingApproval 是一条"还在等用户拍板"的请求的摘要。
// 字段名跟手机端 renderApproval 认的那几个对齐，省得中间再转换。
type pendingApproval struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	ThreadID  string `json:"threadId"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Detail    string `json:"detail"`
	Text      string `json:"text"`
	At        int64  `json:"at"`
}

// trackApproval 记下一条待处理审批，返回的清理函数要在这一轮结束时调用。
func (s *Server) trackApproval(sessionID, threadID string, req appserver.Approval) func() {
	s.approvalMu.Lock()
	if s.approvalInfo == nil {
		s.approvalInfo = map[string]pendingApproval{}
	}
	s.approvalInfo[req.ID] = pendingApproval{
		ID: req.ID, SessionID: sessionID, ThreadID: threadID,
		Kind: req.Kind, Title: req.Title, Detail: req.Detail, Text: req.Reason,
		At: time.Now().UnixMilli(),
	}
	s.approvalMu.Unlock()
	return func() {
		s.approvalMu.Lock()
		delete(s.approvalInfo, req.ID)
		s.approvalMu.Unlock()
	}
}

// pendingApprovals 列出所有还在等的请求，最早的在前面。
func (s *Server) pendingApprovals() []pendingApproval {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()
	out := make([]pendingApproval, 0, len(s.approvalInfo))
	for _, a := range s.approvalInfo {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// markClientID 记下一个手机端消息幂等号。首次见到返回 true；
// 半小时内重复见到返回 false（补发的重复请求）。
func (s *Server) markClientID(id string) bool {
	if id == "" {
		return true // 老版本手机不带幂等号，行为和以前一样，也不往表里塞空键
	}
	now := time.Now().UnixMilli()
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.ensureClientIDsLocked()
	if _, dup := s.clientSeen[id]; dup {
		return false
	}
	s.clientSeen[id] = now
	// 顺手清旧账，别让这张表无限长
	if len(s.clientSeen) > 200 {
		for k, at := range s.clientSeen {
			if now-at > 30*60*1000 {
				delete(s.clientSeen, k)
			}
		}
	}
	s.saveClientIDsLocked()
	return true
}

// seenClientID 只查不记。send 里要回答"这条是不是已经收过了"，
// 但不能像 markClientID 那样顺手把号占住 —— 被拦掉的消息不该占幂等号。
func (s *Server) seenClientID(id string) bool {
	if id == "" {
		return false
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.ensureClientIDsLocked()
	_, dup := s.clientSeen[id]
	return dup
}

// clientIDsPath 去重标记落盘的位置。
func (s *Server) clientIDsPath() string {
	if s.DataDir == "" {
		return ""
	}
	return filepath.Join(s.DataDir, "clientids.json")
}

// ensureClientIDsLocked 第一次用到时把磁盘上的标记读回来（调用方已持锁）。
// 超过半小时的旧账直接丢，跟运行时用的是同一个窗口。
func (s *Server) ensureClientIDsLocked() {
	if s.clientLoaded {
		return
	}
	s.clientLoaded = true
	if s.clientSeen == nil {
		s.clientSeen = map[string]int64{}
	}
	p := s.clientIDsPath()
	if p == "" {
		return
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var m map[string]int64
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	now := time.Now().UnixMilli()
	for k, at := range m {
		if k != "" && now-at <= 30*60*1000 {
			s.clientSeen[k] = at
		}
	}
}

// saveClientIDsLocked 落盘（调用方已持锁）。写失败不影响发消息，只是这次没记住。
func (s *Server) saveClientIDsLocked() {
	p := s.clientIDsPath()
	if p == "" {
		return
	}
	raw, err := json.Marshal(s.clientSeen)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// duplicateSend 判断这一条是不是"已经收过的同一条"。
//
// 两道关：
//   - 幂等号：同一个进程内绝对准，正常情况走这一条；
//   - 内容兜底：服务重启会把上面那张表清空，这时只能靠"这条对话最后收下的
//     就是一模一样的话，而且它现在还在跑"来认。
//
// 内容兜底只在"这一轮还在跑"时生效：否则用户连着发两句一样的话，第二句会被
// 当成重复直接吞掉，那比重复跑一轮还糟。
func (s *Server) duplicateSend(session store.Session, clientID, text string, threadBusy bool) bool {
	if clientID != "" && s.seenClientID(clientID) {
		return true
	}
	if text == "" || session.ThreadID == "" || !threadBusy {
		return false
	}
	last, at := lastUserMessage(session)
	if last == "" || last != text {
		return false
	}
	return time.Since(time.UnixMilli(at)) < 15*time.Minute
}

// lastUserMessage 取这条会话最后一条用户消息的内容和时间。
func lastUserMessage(session store.Session) (string, int64) {
	for i := len(session.Messages) - 1; i >= 0; i-- {
		if session.Messages[i].Role == "user" {
			return strings.TrimSpace(session.Messages[i].Text), session.Messages[i].Ts
		}
	}
	return "", 0
}

var mimeTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "application/javascript; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".png":         "image/png",
	".svg":         "image/svg+xml",
	".ico":         "image/x-icon",
	".css":         "text/css; charset=utf-8",
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 文本响应统一走 gzip：手机在公网链路上，"少发一点"比"发得快一点"划算。
	// 二进制响应和 SSE 会在里面被自动放过，见 compress.go。
	if !gzipWanted(r) {
		s.serve(w, r)
		return
	}
	gw := newGzipWriter(w)
	defer gw.close()
	s.serve(gw, r)
}

// serve 是真正的路由：口令校验 + 分发。
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// 访问口令：Funnel 是公网地址，这一层是唯一的门锁。
	//
	// 除了 ?k=，也认 cookie：服务反代出来的网页自己会再拉一堆子资源
	// （css / js / 图片），那些地址没法一个个挂口令。带对口令时顺手种一个
	// HttpOnly cookie，子请求靠它过门。
	//
	// 作用域是整个站点而不是 /proxy/：有些被代理的页面会把接口地址写成
	// 根路径 fetch('/api/...')，那些请求跑到根上来了，作用域太窄的 cookie
	// 带不上，就会变成一屏 403。
	authed := false
	if k := r.URL.Query().Get("k"); k != "" {
		if k == s.Token {
			authed = true
			http.SetCookie(w, &http.Cookie{
				Name: proxyCookie, Value: s.Token, Path: "/",
				MaxAge: 30 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
		}
	} else if c, err := r.Cookie(proxyCookie); err == nil && c.Value == s.Token {
		authed = true
	}
	if !authed {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "403 链接无效或已过期")
		return
	}

	name := r.URL.Path
	// 服务反代：/proxy/<id>/... 由电脑替手机去取
	if strings.HasPrefix(name, "/proxy/") {
		rest := strings.TrimPrefix(name, "/proxy/")
		id, tail := rest, ""
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			id, tail = rest[:i], rest[i+1:]
		}
		if tail == "" && !strings.HasSuffix(r.URL.Path, "/") {
			dest := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				dest += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, dest, http.StatusFound)
			return
		}
		s.proxyService(w, r, id, tail)
		return
	}
	switch {
	case name == "/" || name == "/index.html":
		s.serveHTML(w, r, "index.html")
	case name == "/api/state":
		s.writeJSON(w, http.StatusOK, s.stateSnapshot())
	case name == "/api/status":
		// 只回"有没有在跑"，界面轮询这个就很轻。
		// 顺手把手机端要做的调试指令捎过去：一次请求办两件事，
		// 少一轮往返，界面也更省电。
		s.writeJSON(w, http.StatusOK, map[string]any{
			"busy": s.busy(), "running": s.runState(), "phoneCmd": dequeuePhoneCmd(),
			"build": s.Build,
			// 电脑端那条单独给一份：手机在别的对话上跑一轮时，这条也不能看不见
			"desk": s.deskRunState(),
			// 手机靠这个自己校准：我这条对话变了没、还在跑吗
			"threads": s.threadActivitySnapshot(),
			// 每条对话最近一轮跑成什么样（完成/失败/中断），读的是 Codex 自己的
			// 状态库，跟副屏同一套数据。列表页用它显示状态词和"耗时"。
			"turnStatus": s.turnStatuses(),
			// 正在等人拍板的请求。手机不一定开着那条对话，也可能刚从后台回来 ——
			// 光靠 /api/events 推一次是不够的（错过就永远看不到，
			// 而审批卡满 10 分钟会直接按拒绝处理）。
			"approvals": s.pendingApprovals(),
		})
	case name == "/api/sysinfo":
		s.sysinfo(w, r)
	case name == "/api/events":
		// 手机和电脑两端的界面都订阅这条流：谁在跑、跑到哪了，两端同时看到
		s.liveEvents(w, r)
	case name == "/api/debug/desktop-turn":
		// 自测用：以"电脑端"的身份在某条对话上起一轮，验证手机能不能实时看到。
		// 走的是和桌面端完全一样的那条旁观链路，所以它通了就代表真机也通。
		s.debugDesktopTurn(w, r)
	case name == "/api/phone/debug":
		// 手机把日志/截图送回来（原生直接发，不依赖网页脚本还活着）
		s.phoneDebug(w, r)
	case name == "/api/phone/debug/list":
		s.phoneDebugList(w, r)
	case name == "/api/phone/command":
		// 电脑让手机做事：拍一张、传日志
		s.phoneCommand(w, r)
	case name == "/api/config":
		s.config(w, r)
	case name == "/api/approval":
		s.approval(w, r)
	case name == "/api/answer":
		s.answerQuestion(w, r)
	case name == "/api/upload":
		s.upload(w, r)
	case name == "/api/threads":
		s.threads(w, r)
	case name == "/api/threads/read":
		s.threadRead(w, r)
	case name == "/api/dirs":
		s.dirs(w, r)
	case name == "/api/fs/list":
		s.fsList(w, r)
	case name == "/api/fs/image":
		s.fsImage(w, r)
	case name == "/api/session/attach":
		s.sessionAttach(w, r)
	case name == "/api/services/add":
		s.serviceAdd(w, r)
	case name == "/api/services/delete":
		s.serviceDelete(w, r)
	case name == "/api/app/version":
		s.appVersion(w, r)
	case name == "/api/notifications":
		s.notifications(w, r)
	case name == "/api/notify/watch":
		s.notifyWatch(w, r)
	case name == "/api/notify/tasks":
		s.notifyTasks(w, r)
	case name == "/apk":
		s.serveAPK(w, r)
	case name == "/qr.png":
		if len(s.QRPNG) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.QRPNG)
	case name == "/pair.png":
		if len(s.PairPNG) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.PairPNG)
	case name == "/apk.png":
		if len(s.APKPNG) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(s.APKPNG)
	case name == "/qr":
		s.serveQR(w, r)
	case name == "/api/session/new":
		s.sessionNew(w, r)
	case name == "/api/session/select":
		s.sessionSelect(w, r)
	case name == "/api/session/delete":
		s.sessionDelete(w, r)
	case name == "/api/session/update":
		s.sessionUpdate(w, r)
	case name == "/api/send":
		s.send(w, r)
	case name == "/api/steer":
		s.steer(w, r)
	case name == "/api/stop":
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopped": s.stopTurn(r.Context())})
	case name == "/api/notify/settings":
		s.notifySettings(w, r)
	case name == "/api/notify/test":
		s.notifyTest(w, r)
	case name == "/api/fav/add":
		s.favAdd(w, r)
	case name == "/api/fav/delete":
		s.favDelete(w, r)
	default:
		// 被代理的网站常把地址写成根路径：fetch('/api/...')，或者前端框架
		// 自己 import('/_app/...')。那些请求绕不过代理前缀，会落到这里。
		// 能认出它属于哪个服务就替他转过去，认不出再当静态资源找。
		if id, ok := proxyReferer(r); ok && !s.webHas(name) {
			// 用跳转而不是就地代拿：脚本被加载进来以后，它自己再发起的子请求
			// 是拿"我自己的地址"当来源的。就地代拿的话那个地址还在根路径上，
			// 下一层就认不出来了（SvelteKit 这类前端就是一层层往下拉的）。
			// 跳过去以后地址落在 /proxy/<id>/ 底下，整条链都顺了。
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				dest := "/proxy/" + id + r.URL.EscapedPath()
				if r.URL.RawQuery != "" {
					dest += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, dest, http.StatusFound)
				return
			}
			s.proxyService(w, r, id, strings.TrimPrefix(name, "/"))
			return
		}
		s.serveStatic(w, r, name)
	}
}

// webHas 判断这个名字是不是电脑端自己内嵌的界面文件。
// 有的话优先按自己的文件发，别被代理兜底抢走。
func (s *Server) webHas(name string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" || strings.Contains(clean, "..") {
		return false
	}
	if _, ok := mimeTypes[strings.ToLower(path.Ext(clean))]; !ok {
		return false
	}
	_, err := fs.Stat(s.Web, clean)
	return err == nil
}

// tailnetTransport 返回访问内网服务要用的传输层。
func (s *Server) tailnetTransport() http.RoundTripper {
	if s.TSClient != nil && s.TSClient.Transport != nil {
		return s.TSClient.Transport
	}
	return http.DefaultTransport
}

// notifications 返回电脑替我们缓存下来的频道消息。
func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	if s.Feed == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		s.Feed.PollNow(r.Context())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"items": s.Feed.Items(),
		"state": s.Feed.State(),
	})
}

// notifyWatch 配置"实时拉取"：频道地址、开关、间隔。
func (s *Server) notifyWatch(w http.ResponseWriter, r *http.Request) {
	if s.Feed == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "功能未启用"})
		return
	}
	var body struct {
		URL      *string `json:"url"`
		On       *bool   `json:"on"`
		Interval *int    `json:"interval"`
	}
	s.readJSON(r, &body)
	cur := s.Feed.State()
	url := cur["url"].(string)
	on := cur["on"].(bool)
	interval := cur["interval"].(int)
	if body.URL != nil {
		url = *body.URL
	}
	if body.On != nil {
		on = *body.On
	}
	if body.Interval != nil {
		interval = *body.Interval
	}
	s.Feed.Configure(url, on, interval)
	if r.URL.Query().Get("refresh") == "1" {
		s.Feed.PollNow(r.Context())
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.Feed.State()})
}

// notifyTasks 配置「任务播报」：开关、要盯的知识库目录，还能立刻试推一条。
//
// 计划任务跑完会把成果写进知识库目录，这里配的就是"盯哪几个目录"。
func (s *Server) notifyTasks(w http.ResponseWriter, r *http.Request) {
	if s.Tasks == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "功能未启用"})
		return
	}
	var body struct {
		On    *bool     `json:"on"`
		Roots *[]string `json:"roots"`
		Scan  bool      `json:"scan"`
		Try   bool      `json:"try"`
	}
	_ = s.readJSON(r, &body)

	if body.On != nil || body.Roots != nil {
		cfg := s.DB.SetTaskWatch(body.On, body.Roots)
		s.Tasks.SetConfig(cfg.On, cfg.Roots)
		// 路径写错了别装作存上了：不存在的目录会被丢掉，这里如实说一声。
		if body.Roots != nil && len(cfg.Roots) == 0 && len(*body.Roots) > 0 {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "error": "这些目录都不存在，检查一下路径", "tasks": s.Tasks.State()})
			return
		}
	}
	if body.Scan {
		s.Tasks.Scan()
	}
	if body.Try {
		title := s.Tasks.BroadcastLatest()
		if title == "" {
			s.writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "error": "这几个目录里还没有日报", "tasks": s.Tasks.State()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "title": title, "tasks": s.Tasks.State()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": s.Tasks.State()})
}

func (s *Server) busy() bool {
	if s.App != nil && s.App.Busy() {
		return true
	}
	return s.Runner.Busy()
}

// stateSnapshot 是界面要的那一份完整状态。/api/state 和 /api/session/select
// 共用它：手机切一条对话本来要"select → state"两趟串行，现在一趟就够。
func (s *Server) stateSnapshot() store.State {
	st := s.DB.Snapshot(s.busy())
	st.PublicURL = s.PublicURL
	st.LANURLs = s.lanURLs()
	st.Running = s.runState()
	// 每个服务都给出经过电脑中转的入口
	for i := range st.Services {
		st.Services[i].Proxy = "/proxy/" + st.Services[i].ID + "/"
	}
	if s.Feed != nil {
		st.Feed = s.Feed.State()
	}
	if s.Tasks != nil {
		st.Tasks = s.Tasks.State()
	}
	return st
}

// ensureApp 保证 app-server 在跑。它常驻不关（杀来杀去只会让服务变得不确定），
// 但万一崩了这里会重新拉起来；起不来就返回 false，调用方退回兼容模式。
func (s *Server) ensureApp(cwd string) bool {
	if s.App == nil {
		return false
	}
	if s.App.Ready() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.App.Ensure(ctx, cwd) == nil
}

// ---------- 静态资源 ----------

func (s *Server) serveHTML(w http.ResponseWriter, r *http.Request, name string) {
	raw, err := fs.ReadFile(s.Web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body := strings.ReplaceAll(string(raw), "__TOKEN__", s.Token)
	// 把"这一份外壳属于哪个版本"烤进页面里。
	// 手机端的 Service Worker 现在是"缓存优先"：页面上手快，但要是它一直
	// 拿着缓存不肯换，就会永远看着老界面（2026-10-10 真踩到了：修好的东西
	// 十一个小时都没上到手机上）。页面拿这个值和 /api/status 的 build 一比，
	// 不一样就主动让 SW 去换新的，见 web/index.html 的 forceShellRefresh。
	body = strings.ReplaceAll(body, "__BUILD__", s.Build)
	etag := etagOf([]byte(body))
	w.Header().Set("Content-Type", mimeTypes[".html"])
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	// 内容没变就只回 304：手机冷启动不用再搬 195 KB，界面照样永远是最新的
	if etagMatches(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = io.WriteString(w, body)
}

// serveQR 是"用手机扫这个码"的页面。
// 走 HTTP 而不是本地 html 文件，因为这台机器上 .html 未必有默认打开方式，
// 双击会弹出"选择一个应用"的对话框。
//
// 页面上有两个码：
//   - 上面那个是给 App 扫的，扫完自动填好地址和口令，直接连上
//   - 下面那个是给浏览器扫的，没装 App 时用
func (s *Server) serveQR(w http.ResponseWriter, r *http.Request) {
	page := `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Codex 随行 · 扫码打开</title>
<style>
  :root { color-scheme: dark; }
  body { margin:0; min-height:100dvh; display:flex; align-items:center; justify-content:center;
         font:15px/1.6 "Microsoft YaHei",system-ui,sans-serif; background:#101114; color:#e6e7e9;
         padding:24px 0; }
  .wrap { display:flex; gap:22px; flex-wrap:wrap; justify-content:center; align-items:flex-start; }
  .card { width:min(430px,92vw); padding:26px; border-radius:20px; background:#17181c;
          border:1px solid #2a2c33; text-align:center; }
  .card.main { border-color:#69c497; }
  h1 { margin:0 0 4px; font-size:19px; }
  .sub { color:#9a9ca3; font-size:13px; margin-bottom:18px; }
  img { width:min(268px,62vw); height:auto; background:#fff; padding:11px; border-radius:14px; }
  .steps { text-align:left; color:#9a9ca3; font-size:13px; margin-top:16px; line-height:1.9; }
  .steps b { color:#e6e7e9; font-weight:600; }
  .url { margin-top:16px; padding:10px 12px; border-radius:12px; background:#101114;
         border:1px solid #2a2c33; word-break:break-all; font-size:12px; color:#9a9ca3; text-align:left; }
  .tag { display:inline-block; padding:2px 10px; border-radius:999px; font-size:12px;
         background:#69c497; color:#0c1512; font-weight:600; margin-bottom:12px; }
  .tag.alt { background:#2a2c33; color:#9a9ca3; font-weight:400; }
</style>
</head>
<body>
  <div class="wrap">
    <div class="card main">
      <div class="tag">推荐</div>
      <h1>用 App 扫码配对</h1>
      <div class="sub">在手机上打开「随行」，点「扫电脑上的码」</div>
      <img src="pair.png?k=__K__" alt="配对二维码">
      <div class="steps">
        <b>1.</b> 手机装好「随行」<br>
        <b>2.</b> 打开 App，点「扫电脑上的码」<br>
        <b>3.</b> 对着这个码扫一下，自动连上
      </div>
    </div>
    <div class="card">
      <div class="tag alt">没装 App</div>
      <h1>用浏览器打开</h1>
      <div class="sub">手机相机直接扫这个码也能用</div>
      <img src="qr.png?k=__K__" alt="网页二维码">
      <div class="url">__PUBLIC__</div>
    </div>
    <div class="card">
      <div class="tag alt">装新版本</div>
      <h1>直接下载安装包</h1>
      <div class="sub">手机相机扫一下，浏览器会下 APK</div>
      <img src="apk.png?k=__K__" alt="安装包二维码">
      <div class="steps">
        <b>1.</b> 用手机相机扫这个码<br>
        <b>2.</b> 在浏览器里下载这个 apk<br>
        <b>3.</b> 点开它，允许安装未知应用即可
      </div>
      <div class="url">__APKURL__</div>
    </div>
  </div>
</body>
</html>`
	replacements := map[string]string{
		"__K__":      url.QueryEscape(s.Token),
		"__PUBLIC__": escapeText(s.PublicURL),
		"__APKURL__": escapeText(strings.Replace(s.PublicURL, "/?k=", "/apk?k=", 1)),
	}
	for k, v := range replacements {
		page = strings.ReplaceAll(page, k, v)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, page)
}

func escapeText(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, name string) {
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" || strings.Contains(clean, "..") {
		http.NotFound(w, nil)
		return
	}
	ext := strings.ToLower(path.Ext(clean))
	contentType, ok := mimeTypes[ext]
	if !ok {
		http.NotFound(w, nil)
		return
	}
	raw, err := fs.ReadFile(s.Web, clean)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	// 清单里带着起始地址，必须把口令注入进去，
	// 否则从手机桌面图标启动会因为缺口令被拒。
	// sw.js 也是"一变就得让浏览器立刻知道"的文件：它要是被缓存住，
	// 手机就会一直用旧的外壳缓存，界面改了也看不到。
	if ext == ".webmanifest" || ext == ".html" || clean == "sw.js" {
		raw = []byte(strings.ReplaceAll(string(raw), "__TOKEN__", s.Token))
		// 这几个文件"一变就得让手机立刻知道"，所以每次都回源校验；
		// 但只要没变就只回 304，不再重传整个文件
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "max-age=600")
	}
	etag := etagOf(raw)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	if etagMatches(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(raw)
}

// ---------- 会话 ----------

type newSessionReq struct {
	// Cwd 用指针是为了分得清两件事，它们以前被当成同一件：
	//   字段缺席        —— 调用方没指定目录，沿用上次用过的
	//   给了空串 ""     —— "不指定项目"，必须真的不绑定目录
	Cwd     *string `json:"cwd"`
	Sandbox string  `json:"sandbox"`
}

func (s *Server) sessionNew(w http.ResponseWriter, r *http.Request) {
	var body newSessionReq
	s.readJSON(r, &body)
	var session store.Session
	switch {
	case body.Cwd == nil:
		session = s.DB.NewSession("", body.Sandbox)
	case strings.TrimSpace(*body.Cwd) == "":
		session = s.DB.NewSessionNoProject(body.Sandbox)
	default:
		session = s.DB.NewSession(strings.TrimSpace(*body.Cwd), body.Sandbox)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session.Summary()})
}

func (s *Server) sessionSelect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	s.readJSON(r, &body)
	if !s.DB.Select(body.ID) {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "会话不存在"})
		return
	}
	// 顺手把整份状态带回去。
	//
	// 手机打开一条对话原本是"select 一趟 → state 一趟 → 历史一趟"串行等三次，
	// 走公网实测 1.7 秒（单趟地板 363ms）。select 直接把 state 塞回响应里，
	// 客户端就少等一趟；历史那一趟在客户端跟这里并行发，见 web/index.html。
	s.writeJSON(w, http.StatusOK, s.stateSnapshot())
}

func (s *Server) sessionDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	s.readJSON(r, &body)
	s.DB.Delete(body.ID)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) sessionUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd      string `json:"cwd"`
		Sandbox  string `json:"sandbox"`
		Approval string `json:"approval"`
	}
	s.readJSON(r, &body)
	session, errMsg := s.DB.UpdateActive(body.Cwd, body.Sandbox, body.Approval)
	if errMsg != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session.Summary()})
}

// ---------- 对话 ----------

// threads 列出电脑上 Codex 自己的对话——桌面端、CLI 里聊过的都能看到，
// 这就是"把电脑上的聊天记录同步过来"。
func (s *Server) threads(w http.ResponseWriter, r *http.Request) {
	if s.App == nil || !s.ensureApp("") {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "协议模式还没就绪"})
		return
	}
	q := r.URL.Query()
	if v := q.Get("q"); v != "" || q.Get("cursor") != "" || q.Get("archived") == "1" {
		limit := 50
		if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
			limit = n
		}
		list, next, err := s.App.ListThreads(r.Context(), limit, q.Get("cursor"), q.Get("q"), q.Get("archived") == "1")
		if err != nil {
			s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"threads": list, "nextCursor": next})
		return
	}

	// 不带参数就是"要全部"：自动翻页 + 30 秒缓存
	list, err := s.App.ListAllThreads(r.Context(), q.Get("refresh") == "1")
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"threads": list})
}

// dirs 返回可以作为工作目录的候选：常用项目、最近对话用到过的目录、默认目录。
// 手机上就不用手打 C:\... 这种路径了。
// fsList 让手机能"浏览电脑上的文件夹"。
//
// 路径为空就先列驱动器（Windows 上就是 C:\ D:\ 这些），
// 给了路径就列它的子目录，顺带给出上层目录，手机那边做一个能点进去的列表。
// 跳过隐藏目录和系统目录：正常选项目用不到它们，列出来只会碍事。
func (s *Server) fsList(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	out := struct {
		Path   string  `json:"path"`
		Parent string  `json:"parent"`
		Dirs   []entry `json:"dirs"`
		Drives []entry `json:"drives,omitempty"`
	}{Path: p}

	if p == "" {
		letters := "CDEFGHIJKLMNOPQRSTUVWXYZ"
		for _, c := range letters {
			root := string(c) + ":\\"
			if info, err := os.Stat(root); err == nil && info.IsDir() {
				out.Drives = append(out.Drives, entry{Name: string(c) + " 盘", Path: root})
			}
		}
		s.writeJSON(w, http.StatusOK, out)
		return
	}
	abs, err := filepath.Abs(p)
	if err != nil || !isDir(abs) {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "这个目录打不开"})
		return
	}
	out.Path = abs
	if parent := filepath.Dir(abs); parent != abs {
		out.Parent = parent
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读不到这个目录：" + err.Error()})
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
			continue
		}
		out.Dirs = append(out.Dirs, entry{Name: name, Path: filepath.Join(abs, name)})
		if len(out.Dirs) >= 300 {
			break
		}
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) dirs(w http.ResponseWriter, r *http.Request) {
	type item struct {
		Path     string `json:"path"`
		Name     string `json:"name"`
		Count    int    `json:"count"`
		LastUsed int64  `json:"lastUsed"`
		Pinned   bool   `json:"pinned"`
	}
	byPath := map[string]*item{}
	order := []string{}

	add := func(p string, ts int64) {
		p = strings.TrimSpace(p)
		if p == "" || !isDir(p) {
			return
		}
		it, ok := byPath[p]
		if !ok {
			it = &item{Path: p, Name: baseName(p)}
			byPath[p] = it
			order = append(order, p)
		}
		it.Count++
		if ts > it.LastUsed {
			it.LastUsed = ts
		}
	}

	// 常用项目排最前
	for _, f := range s.DB.ListFavorites() {
		add(f.Cwd, 1<<62)
		if it, ok := byPath[f.Cwd]; ok {
			it.Name = f.Name
			it.Pinned = true
		}
	}
	if s.App != nil && s.App.Ready() {
		if list, err := s.App.ListAllThreads(r.Context(), false); err == nil {
			for _, t := range list {
				add(t.Cwd, t.UpdatedAt)
			}
		}
	}
	if s.DefaultCwd != "" {
		add(s.DefaultCwd, 0)
	}

	out := make([]item, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		return out[i].LastUsed > out[j].LastUsed
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"dirs": out})
}

func baseName(p string) string {
	p = strings.TrimRight(p, `\/`)
	if i := strings.LastIndexAny(p, `\/`); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}

// threadRead 读回某条电脑上对话的完整内容。
func (s *Server) threadRead(w http.ResponseWriter, r *http.Request) {
	if s.App == nil || !s.ensureApp("") {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "协议模式还没就绪"})
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
		return
	}
	msgs, lastStatus, err := s.App.ReadThread(r.Context(), id)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	// lastStatus 让手机知道最后一轮是 completed / interrupted / failed：
	// 被打断或失败时，界面上给一个"继续"。
	s.writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "lastStatus": lastStatus})
}

// ---------- 全局设置 ----------

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DefaultCwd string `json:"defaultCwd"`
	}
	s.readJSON(r, &body)
	if body.DefaultCwd != "" {
		if errMsg := s.DB.SetDefaultCwd(body.DefaultCwd); errMsg != "" {
			s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
			return
		}
	}
	st := s.DB.Snapshot(s.busy())
	st.PublicURL = s.PublicURL
	st.Running = s.runState()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": st})
}

// ---------- 服务入口 ----------

// sessionAttach 把电脑上已有的一条对话"接管"过来：
// 在本地建一个绑定了同一个 threadId 的会话，之后发消息就是接着那条聊。
func (s *Server) sessionAttach(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ThreadID string `json:"threadId"`
		Cwd      string `json:"cwd"`
		Name     string `json:"name"`
	}
	s.readJSON(r, &body)
	if body.ThreadID == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 threadId"})
		return
	}
	session, errMsg := s.DB.Attach(body.ThreadID, body.Cwd, body.Name)
	if errMsg != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": session.Summary()})
}

func (s *Server) serviceAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		URL  string `json:"url"`
		Desc string `json:"desc"`
		Icon string `json:"icon"`
	}
	s.readJSON(r, &body)
	list, errMsg := s.DB.AddService(body.Name, body.URL, body.Desc, body.Icon)
	if errMsg != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "services": list})
}

func (s *Server) serviceDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	s.readJSON(r, &body)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "services": s.DB.DeleteService(body.ID)})
}

// sysinfo 把电脑的运行状态交给手机。数字都是电脑端常驻采样的，
// 手机每两秒来取一次也不费什么。
func (s *Server) sysinfo(w http.ResponseWriter, r *http.Request) {
	if s.Mon == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"error": "监控没有启动"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.Mon.Snapshot())
}

// ---------- 更新通道 ----------
//
// 电脑端把发布好的 APK 和版本信息放在数据目录的 更新\ 下面，
// 手机上的 App 启动时来问一句，有新版本就自己下载安装。

func (s *Server) appVersion(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.UpdateDir, "version.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	var info map[string]any
	if err := json.Unmarshal(raw, &info); err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	info["available"] = true
	// 下载地址带上版本号。每次发版地址都不一样，手机上和中间的缓存
	// 就没机会把旧安装包当成新的发回来（曾经出现过提示 2.0、装上是 1.9）。
	q := url.Values{}
	if v, ok := info["versionCode"].(float64); ok && v > 0 {
		q.Set("v", strconv.Itoa(int(v)))
	}
	q.Set("k", s.Token)
	info["url"] = "/apk?" + q.Encode()
	s.writeJSON(w, http.StatusOK, info)
}

func (s *Server) serveAPK(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.UpdateDir, "app.apk")
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := s.apkFileName()
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	// 安装包一律不缓存：这个接口的地址是固定的，被任何一层缓存住
	// 就会把上一个版本的包发出去。
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(w, f)
	log.Printf("下发安装包 %s（%d 字节，实际写出 %d）", name, info.Size(), n)
}

// apkFileName 从 version.json 里拼一个带版本号的文件名，手机下载列表里
// 一眼就能看出下的是哪个版本。
func (s *Server) apkFileName() string {
	raw, err := os.ReadFile(filepath.Join(s.UpdateDir, "version.json"))
	if err != nil {
		return "CodexMobile-update.apk"
	}
	var info struct {
		VersionCode int    `json:"versionCode"`
		VersionName string `json:"versionName"`
	}
	if json.Unmarshal(raw, &info) != nil || info.VersionName == "" {
		return "CodexMobile-update.apk"
	}
	return fmt.Sprintf("CodexMobile-v%s-code%d.apk", info.VersionName, info.VersionCode)
}

// steer 往正在跑的那一轮里插一句话（手机上的"引导"）。
// 不打断它，只是把新要求塞进去 —— 这是 Codex 原生就有的能力。
func (s *Server) steer(w http.ResponseWriter, r *http.Request) {
	session, ok := s.DB.Active()
	if !ok || session.ThreadID == "" {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "这条对话还没开始跑"})
		return
	}
	var body struct {
		Text        string                 `json:"text"`
		Attachments []appserver.Attachment `json:"attachments"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" && len(body.Attachments) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容为空"})
		return
	}
	// 用"这条对话当前有没有轮次号"来判，而不是"手机自己有没有在跑"：
	// 电脑端发起的那一轮手机也要能引导。
	if s.App == nil || s.App.ActiveTurnID(session.ThreadID) == "" {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "这条对话现在没有在跑"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.App.SteerTurn(ctx, session.ThreadID, text); err != nil {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	// 让所有界面立刻重读这条对话（引导本身也会进历史）
	s.publishLive(liveEvent{
		SessionID: session.ID, ThreadID: session.ThreadID, Kind: "steer",
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// upload 接收手机传上来的文件 / 图片，落到电脑上下载目录里，返回绝对路径。
//
// 手机那边把文件读成 base64 发过来（WebView 里拿不到真实路径），
// 这里存到 <数据目录>\uploads\，之后跟着消息一起交给 Codex：
// 图片按图片给模型看，其它文件按"提到这个文件"给模型读。
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读不到上传内容"})
		return
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "上传格式不对"})
		return
	}
	data := strings.TrimSpace(body.Data)
	if i := strings.Index(data, ","); i >= 0 && strings.HasPrefix(data, "data:") {
		data = data[i+1:] // 容忍 data:image/png;base64,xxx
	}
	blob, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(blob) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文件内容不对"})
		return
	}
	name := safeName(filepath.Base(strings.TrimSpace(body.Name)))
	if name == "" {
		name = "file"
	}
	dir := filepath.Join(s.DataDir, "uploads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+name)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "path": path, "name": name, "size": len(blob),
		// 后缀认不出来就看文件头 —— 只要真是图片，就按图片发给模型
		"image": isImageName(name) || sniffImage(blob),
	})
}

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".heic", ".heif":
		return true
	}
	return false
}

// sniffImage 按文件头认图片。只靠后缀不够稳（手机传上来的名字可能压根没后缀）。
func sniffImage(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	switch {
	case b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF: // JPEG
		return true
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G': // PNG
		return true
	case b[0] == 'G' && b[1] == 'I' && b[2] == 'F': // GIF
		return true
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP": // WebP
		return true
	case b[0] == 'B' && b[1] == 'M': // BMP
		return true
	}
	return false
}

// changeStats 数一次文件改动：几个文件、加了多少行、删了多少行。
// 补丁里每行以 + / - 开头，忽略 +++ / --- 这两行头。
func changeStats(it appserver.Item) (files, added, removed int) {
	files = len(it.Changes)
	for _, c := range it.Changes {
		for _, line := range strings.Split(c.Diff, "\n") {
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
				continue
			case strings.HasPrefix(line, "+"):
				added++
			case strings.HasPrefix(line, "-"):
				removed++
			}
		}
	}
	return files, added, removed
}

// pickSendSession 决定这条消息发到哪条对话。
//
// 带了 sessionId 就发给它（手机的离线补发必须带：补发是在切对话的过程中
// 发起的，那会儿"活跃对话"早就变成另一条了，不带就会串门）；没带或认不出来
// 才退回"当前活跃的那条"，老客户端的行为保持不变。
func (s *Server) pickSendSession(id string) store.Session {
	if id != "" {
		if target, found := s.DB.ByID(id); found {
			return target
		}
	}
	if cur, ok := s.DB.Active(); ok {
		return cur
	}
	return s.DB.NewSession("", "")
}

func (s *Server) send(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text        string                 `json:"text"`
		Attachments []appserver.Attachment `json:"attachments"`
		ClientID    string                 `json:"clientId"`
		// SessionID 是"这条发到哪条对话"。
		//
		// 2026-10-10 的教训：以前没有这个字段，服务端只能发到"当前活跃的那条"。
		// 而手机的离线补发是在切对话的过程中发起的 —— 切走之后"活跃"已经变成
		// 另一条了，于是补发的消息落到别的对话里，用户看到的是"上一条消息被
		// 重复发到了另一条对话"。带上目标就再也不会串门。
		SessionID string `json:"sessionId"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}

	session := s.pickSendSession(body.SessionID)

	text := strings.TrimSpace(body.Text)
	if text == "" && len(body.Attachments) == 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内容为空"})
		return
	}

	// 幂等必须排在"这条对话还在跑"前面。
	//
	// 手机断线重连后会把没拿到结果的那条重发一遍（离线发件箱）。可这一轮其实
	// 正在电脑上跑得好好的，要是先回一句笼统的"上一轮还没结束"，手机就以为
	// 没送到，气泡会一直转着"没发出去，联网后自动重发"，这一轮跑多久它转多久
	// —— 电脑那边一切正常，手机这边看着像卡死了。如实回 duplicate，手机当场
	// 就把它从发件箱里摘掉。
	threadBusy := session.ThreadID != "" && s.App != nil && s.App.ThreadBusy(session.ThreadID)

	if s.duplicateSend(session, body.ClientID, text, threadBusy) {
		s.writeJSON(w, http.StatusConflict, map[string]any{
			"error": "这条消息已经在电脑上跑了", "duplicate": true,
		})
		return
	}

	// 只有**这条对话**上一轮还没跑完才拦；别的对话在跑不影响这里。
	// 以前这里看的是"全局有没有在跑"，所以在另一条对话里发消息会被误拦。
	if threadBusy {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "这条对话上一轮还没结束"})
		return
	}
	if s.Runner != nil && s.Runner.Busy() {
		s.writeJSON(w, http.StatusConflict, map[string]any{"error": "上一轮还在进行中"})
		return
	}
	// 真收下这一条之后才记账。被上面几道拦掉的消息不能占住幂等号，
	// 否则用户等会儿再发同一条会被误判成"已经跑过了"而永远发不出去。
	if body.ClientID != "" {
		s.markClientID(body.ClientID)
	}

	// 这一轮在哪个目录跑。
	runDir, dropThread := runDirFor(session.Cwd, s.fallbackCwd())
	if dropThread {
		session.ThreadID = ""
	}
	session.Cwd = runDir

	s.DB.AddMessage(session.ID, store.Message{Role: "user", Text: text, Ts: time.Now().UnixMilli()})
	if s.DB.CountUserMessages(session.ID) == 1 {
		s.DB.Rename(session.ID, humanTitle(text))
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	enc := json.NewEncoder(w)
	emit := func(evt codex.Event) {
		_ = enc.Encode(evt)
		if flusher != nil {
			flusher.Flush()
		}
		// 记下"这条对话现在在干什么"：手机列表页拿它当状态行的第三行显示
		s.noteActivity(evt.ThreadID, activityLabel(evt))
		// 同时广播给所有界面：这样手机在跑的时候，电脑那边打开同一个界面
		// 也能立刻看到"正在生成"，而不是干等
		s.publishLive(liveEvent{
			SessionID: session.ID, ThreadID: evt.ThreadID,
			Kind: evt.Type, Event: codex.Event{Type: evt.Type, Text: evt.Text, Title: evt.Title},
		})
	}

	// 这一轮不跟着手机的连接走：手机返回列表、切后台、锁屏，生成都继续在
	// 电脑上跑完，跑完还会写进会话记录。用户回来时靠 /api/status 看到它
	// 还在跑（列表和对话里转圈圈），跑完再把内容读回来。
	runCtx := context.Background()
	s.beginRun(session.ID)
	// 立刻广播"这条对话开始跑了"：别的界面（电脑那边）不用等第一个事件
	// 就能看到转圈和计时
	s.publishLive(liveEvent{SessionID: session.ID, Kind: "run", At: time.Now().UnixMilli()})
	defer s.endRun()

	var turn codex.Turn
	threadID := session.ThreadID
	if s.ensureApp(session.Cwd) {
		var err error
		threadID, turn, err = s.runViaAppServer(runCtx, session, text, body.Attachments, emit)
		if err != nil {
			emit(codex.Event{Type: "error", Text: err.Error()})
		}
	} else {
		turn = s.Runner.Run(session.Cwd, session.Sandbox, session.ThreadID, text, emit)
		threadID = turn.ThreadID
	}

	if threadID != "" {
		s.DB.SetThreadID(session.ID, threadID)
	}
	final := turn.Text
	if final == "" {
		if turn.OK {
			final = "(没有返回内容)"
		} else {
			final = "(已停止)"
		}
	}
	s.DB.AddMessage(session.ID, store.Message{
		Role: "assistant", Text: final,
		Activities: turn.Activities, Ts: time.Now().UnixMilli(),
	})

	cfg := s.DB.NotifyConfig()
	go func() {
		_, _ = notify.Send(cfg, notify.Title(turn.OK, s.DB.Title(session.ID)), notify.Summarize(final))
	}()

	emit(codex.Event{Type: "done"})
}

// runViaAppServer 用官方 app-server 协议跑一轮，能流式输出、能弹审批。
func (s *Server) runViaAppServer(ctx context.Context, session store.Session, text string, atts []appserver.Attachment, emit func(codex.Event)) (string, codex.Turn, error) {
	s.initApprovals()

	approval := session.Approval
	if approval == "" {
		approval = store.DefaultApproval
	}
	threadID, err := s.App.EnsureThread(ctx, session.ThreadID, session.Cwd, session.Sandbox, approval)
	if err != nil {
		return "", codex.Turn{}, fmt.Errorf("准备会话失败：%w", err)
	}
	emit(codex.Event{Type: "thread", ThreadID: threadID})
	// 线程号一拿到就先落盘。以前是等整轮跑完才记，中间这段时间手机上
	// 会同时出现"本地这条"和"电脑上那条"，点一下就又被接管一次。
	s.setRunThread(threadID)
	s.DB.SetThreadID(session.ID, threadID)

	turn := codex.Turn{}
	status := ""
	sink := appserver.Sink{
		OnTurnStarted: func(string) { emit(codex.Event{Type: "working"}) },
		OnDelta:       func(t string) { emit(codex.Event{Type: "delta", Text: t}) },
		OnItemStarted: func(it appserver.Item) {
			if it.Type == "reasoning" {
				emit(codex.Event{Type: "reasoning"})
			}
		},
		OnItemDone: func(it appserver.Item) {
			switch it.Type {
			case "agentMessage":
				if it.Text != "" {
					turn.Text = it.Text
					emit(codex.Event{Type: "message", Text: it.Text})
				}
			case "reasoning":
				emit(codex.Event{Type: "reasoning"})
			case "commandExecution":
				label := it.Command
				if label == "" {
					label = "执行命令"
				}
				turn.Activities = append(turn.Activities, label)
				emit(codex.Event{Type: "tool", Text: label})
			case "fileChange":
				label := describeChanges(it)
				turn.Activities = append(turn.Activities, label)
				// 桌面版在这里显示一张"N 个文件已更改 +X -Y"的卡片，
				// 手机也照做：数一下文件数，再从补丁里数增删行。
				// 明细（每个文件的路径和 diff）不往手机送：手机上不展示它们，
				// 送过去只是白占这条公网链路。
				files, added, removed := changeStats(it)
				emit(codex.Event{
					Type: "fileChange", Text: label,
					Files: files, Added: added, Removed: removed,
				})
			case "mcpToolCall":
				label := "调用 " + it.Server + " 的 " + it.Tool
				turn.Activities = append(turn.Activities, label)
				emit(codex.Event{Type: "tool", Text: label})
			case "webSearch":
				label := "联网搜索：" + it.Query
				turn.Activities = append(turn.Activities, label)
				emit(codex.Event{Type: "tool", Text: label})
			}
		},
		OnTurnDone: func(t appserver.Turn) {
			status = t.Status
			emit(codex.Event{Type: "turn_done"})
		},
		OnLog: func(t string) {
			if !appserver.IsNoise(t) {
				emit(codex.Event{Type: "log", Text: t})
			}
		},
		OnApproval: func(req appserver.Approval) appserver.Decision {
			ch := make(chan appserver.Decision, 1)
			s.approvalMu.Lock()
			s.approvals[req.ID] = ch
			s.approvalMu.Unlock()
			untrack := s.trackApproval(session.ID, threadID, req)
			defer func() {
				s.approvalMu.Lock()
				delete(s.approvals, req.ID)
				s.approvalMu.Unlock()
				untrack()
			}()

			emit(codex.Event{
				Type: "approval", ApprovalID: req.ID, Kind: req.Kind,
				Title: req.Title, Detail: req.Detail, Text: req.Reason,
			})
			s.pushApprovalNotify(req)
			select {
			case d := <-ch:
				if d == "" {
					return appserver.Decline
				}
				return d
			case <-time.After(9 * time.Minute):
				emit(codex.Event{Type: "log", Text: "审批超时，已按拒绝处理。"})
				return appserver.Decline
			case <-ctx.Done():
				return appserver.Decline
			}
		},
		OnQuestion: func(qs []codex.Question) map[string][]string {
			// 手机自己发起的这一轮：同样把选择题推出去等它答。
			threadID := ""
			if run := s.runState(); run != nil {
				threadID = run.ThreadID
			}
			return s.askPhoneQuestions(session.ID, threadID, qs)
		},
	}

	res, err := s.App.RunTurnWith(ctx, threadID, text, atts, sink)
	if res.Text != "" {
		turn.Text = res.Text
	}
	turn.OK = err == nil && status != "interrupted" && status != "failed"
	// 这一轮的结局记一行日志：以后"对话怎么没跑完"能直接翻日志对时间，
	// 不用再靠猜。
	if !turn.OK {
		log.Printf("这一轮没正常结束：thread=%s status=%q err=%v", threadID, status, err)
	} else {
		log.Printf("这一轮完成：thread=%s 输出=%d 字", threadID, len(turn.Text))
	}
	return threadID, turn, err
}

func describeChanges(it appserver.Item) string {
	paths := make([]string, 0, len(it.Changes))
	for _, ch := range it.Changes {
		if ch.Path != "" {
			paths = append(paths, ch.Path)
		}
	}
	if len(paths) == 0 {
		return "修改文件"
	}
	if len(paths) > 3 {
		paths = append(paths[:3], fmt.Sprintf("等 %d 个文件", len(it.Changes)))
	}
	return "修改文件：" + strings.Join(paths, "、")
}

// stopTurn 中断当前轮次，app-server 与兜底通道都覆盖。
func (s *Server) stopTurn(ctx context.Context) bool {
	if s.App != nil && s.App.Busy() {
		// 现在可以多条对话同时跑，所以"停止"要按你**正看着的那条**来：
		// 先认当前选中的对话，它没在跑再退回正在跑的那条。
		threadID := ""
		if session, ok := s.DB.Active(); ok && session.ThreadID != "" && s.App.ThreadBusy(session.ThreadID) {
			threadID = session.ThreadID
		}
		if threadID == "" {
			if run := s.runState(); run != nil {
				threadID = run.ThreadID
			}
		}
		if threadID == "" {
			return false
		}
		ictx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = s.App.Interrupt(ictx, threadID)
		return true
	}
	return s.Runner.Stop()
}

// approval 接收手机端对审批卡片的回答。
// SetThreadAsker 让"电脑端发起的那一轮"的审批和选择题也能推到手机上。
//
// 以前只有手机自己发起的轮次才有卡片（那些走 /api/send 里的事件流），
// 结果人走开了、电脑上跑的一轮问"选哪个"，手机上什么都看不到。
func (s *Server) InitFallbackAsker() {
	if s.App == nil {
		return
	}
	s.App.SetFallbackAsker(
		func(threadID string, req appserver.Approval) appserver.Decision {
			sess, ok := s.DB.ByThread(threadID)
			if !ok {
				return ""
			}
			ch := make(chan appserver.Decision, 1)
			s.approvalMu.Lock()
			if s.approvals == nil {
				s.approvals = map[string]chan appserver.Decision{}
			}
			s.approvals[req.ID] = ch
			s.approvalMu.Unlock()
			untrack := s.trackApproval(sess.ID, threadID, req)
			defer func() {
				s.approvalMu.Lock()
				delete(s.approvals, req.ID)
				s.approvalMu.Unlock()
				untrack()
			}()
			s.publishLive(liveEvent{
				SessionID: sess.ID, ThreadID: threadID, Kind: "approval",
				Event: codex.Event{
					Type: "approval", ApprovalID: req.ID, Kind: req.Kind,
					Title: req.Title, Detail: req.Detail, Text: req.Reason,
				},
			})
			s.pushApprovalNotify(req)
			select {
			case d := <-ch:
				return d
			case <-time.After(10 * time.Minute):
				return ""
			}
		},
		func(threadID string, qs []codex.Question) map[string][]string {
			sess, ok := s.DB.ByThread(threadID)
			if !ok {
				return nil
			}
			return s.askPhoneQuestions(sess.ID, threadID, qs)
		},
	)
}

// askPhoneQuestions 把选择题推到手机上等回答（电脑端那一轮也走这里）。
func (s *Server) askPhoneQuestions(sessionID, threadID string, qs []codex.Question) map[string][]string {
	id := "q-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ch := make(chan map[string][]string, 1)
	s.questionMu.Lock()
	if s.questions == nil {
		s.questions = map[string]chan map[string][]string{}
	}
	s.questions[id] = ch
	s.questionMu.Unlock()
	defer func() {
		s.questionMu.Lock()
		delete(s.questions, id)
		s.questionMu.Unlock()
	}()
	s.publishLive(liveEvent{
		SessionID: sessionID, ThreadID: threadID, Kind: "question",
		Event: codex.Event{Type: "question", ApprovalID: id, Questions: qs},
	})
	s.pushQuestionNotify(id, qs)
	select {
	case a := <-ch:
		return a
	case <-time.After(10 * time.Minute):
		return nil
	}
}

// pushQuestionNotify 把选择题推到手机（和审批对称：锁屏也得知道它在等你）。
func (s *Server) pushQuestionNotify(id string, qs []codex.Question) {
	cfg := s.DB.NotifyConfig()
	if notify.Resolve(cfg) == nil {
		return
	}
	var b strings.Builder
	for i, q := range qs {
		if i > 0 {
			b.WriteString("\n")
		}
		if q.Header != "" {
			b.WriteString(q.Header)
		}
		if q.Text != "" {
			b.WriteString("\n")
			b.WriteString(q.Text)
		}
	}
	body := strings.TrimSpace(b.String())
	if body == "" {
		body = "有选择题等你确认"
	}
	go func() {
		if _, err := notify.SendQuestion(cfg, "❓ Codex 问你一件事", body, s.PublicURL); err != nil {
			log.Printf("选择题推送没发出去：%v", err)
		}
		timer := time.NewTimer(4 * time.Minute)
		defer timer.Stop()
		<-timer.C
		s.questionMu.Lock()
		_, pending := s.questions[id]
		s.questionMu.Unlock()
		if !pending {
			return
		}
		if _, err := notify.SendQuestion(cfg, "⏰ Codex 还在等你选", body, s.PublicURL); err != nil {
			log.Printf("选择题催办没发出去：%v", err)
		}
	}()
}

// answerQuestion 接收手机端对选择题的回答。
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string              `json:"id"`
		Answers map[string][]string `json:"answers"`
	}
	s.readJSON(r, &body)
	s.questionMu.Lock()
	ch := s.questions[body.ID]
	s.questionMu.Unlock()
	if ch == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "这道题已经失效"})
		return
	}
	select {
	case ch <- body.Answers:
	default:
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// pushApprovalNotify 把"等你拍板"推到手机。审批 9~10 分钟没人理就按拒绝处理，
// 所以这口气必须抢在超时之前送到：发出去之后 4 分钟还没人理，再催一次。
func (s *Server) pushApprovalNotify(req appserver.Approval) {
	cfg := s.DB.NotifyConfig()
	if notify.Resolve(cfg) == nil {
		return
	}
	callback := ""
	if s.PublicURL != "" {
		// PublicURL 形如 https://xxx.ts.net/?k=口令，换成 /api/approval 的入口。
		callback = strings.Replace(s.PublicURL, "/?", "/api/approval?", 1)
	}
	body := req.Title
	if req.Reason != "" {
		body += "\n" + req.Reason
	} else if req.Detail != "" {
		body += "\n" + req.Detail
	}
	title := "⏸️ Codex 等你确认"
	go func() {
		if _, err := notify.SendApproval(cfg, req.ID, title, body, callback); err != nil {
			log.Printf("审批推送没发出去：%v", err)
		}
		timer := time.NewTimer(4 * time.Minute)
		defer timer.Stop()
		<-timer.C
		s.approvalMu.Lock()
		_, pending := s.approvals[req.ID]
		s.approvalMu.Unlock()
		if !pending {
			return
		}
		if _, err := notify.SendApproval(cfg, req.ID,
			"⏰ Codex 审批快超时了", "还没人确认，过几分钟会按拒绝处理："+req.Title, ""); err != nil {
			log.Printf("审批催办没发出去：%v", err)
		}
	}()
}

func (s *Server) approval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Decision string `json:"decision"`
	}
	s.readJSON(r, &body)

	s.approvalMu.Lock()
	ch := s.approvals[body.ID]
	s.approvalMu.Unlock()
	if ch == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "这条审批已经失效"})
		return
	}
	switch body.Decision {
	case string(appserver.Accept), string(appserver.AcceptForSession), string(appserver.Cancel):
	default:
		body.Decision = string(appserver.Decline)
	}
	select {
	case ch <- appserver.Decision(body.Decision):
	default:
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) notifySettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel    *string `json:"channel"`
		Value      *string `json:"value"`
		URL        *string `json:"url"`
		Format     *string `json:"format"`
		OnDone     *bool   `json:"onDone"`
		OnApproval *bool   `json:"onApproval"`
	}
	if err := s.readJSON(r, &body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误"})
		return
	}
	cfg := s.DB.SetNotify(body.Channel, body.Value, body.URL, body.Format, body.OnDone, body.OnApproval)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "notify": cfg})
}

func (s *Server) notifyTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.DB.NotifyConfig()
	if notify.Resolve(cfg) == nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "还没有设置推送方式或密钥"})
		return
	}
	status, err := notify.Send(cfg, notify.Title(true, "测试通知"),
		"这是一条来自你电脑上 Codex 的测试消息，收到就说明通道没问题。")
	if status == 0 || err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": "请求没有发出去，检查一下网络或密钥"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": status})
}

func (s *Server) favAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Cwd  string `json:"cwd"`
	}
	s.readJSON(r, &body)
	favorites, errMsg := s.DB.AddFavorite(body.Name, body.Cwd)
	if errMsg != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "favorites": favorites})
}

func (s *Server) favDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	s.readJSON(r, &body)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "favorites": s.DB.DeleteFavorite(body.ID)})
}

// ---------- 小工具 ----------

func (s *Server) readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	err := dec.Decode(dst)
	if err == io.EOF {
		return nil
	}
	return err
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

func humanTitle(text string) string {
	flat := strings.Join(strings.Fields(text), " ")
	if flat == "" {
		return "新对话"
	}
	if utf8.RuneCountInString(flat) > 24 {
		runes := []rune(flat)
		return string(runes[:24])
	}
	return flat
}

func isDir(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// fallbackCwd 是"没有工作目录时该去哪个目录跑"。
//
// 用户在「我的」里设过默认目录就用它（那份才是他改得动的），否则用启动时算出来的
// 那一份（一般是用户主目录）。两条都可能指到一个已经没了的目录，所以都验一下。
func (s *Server) fallbackCwd() string {
	if s.DB != nil {
		if d := strings.TrimSpace(s.DB.DefaultCwd); isDir(d) {
			return d
		}
	}
	if isDir(s.DefaultCwd) {
		return s.DefaultCwd
	}
	return s.DefaultCwd
}

// runDirFor 决定这一轮的会话在哪个目录跑，以及要不要丢掉旧线程。
//
// 三种情况分得很清楚：
//
//	绑了目录且还在      -> 就用它
//	绑了目录但目录没了  -> 退回默认目录，并且丢掉旧线程（上下文已经不在那儿了）
//	本来就没绑目录      -> 用默认目录，但线程照旧接着用
//
// 最后一种就是手机上的「不指定项目」。它和"目录没了"必须分开：DB 里这种会话的
// cwd 一直是空的，要是按"目录不存在"处理，每发一条消息都会丢掉线程号、另起一条
// 新线程，上下文全没了，旧线程还会一直占着写入锁。
func runDirFor(cwd, fallback string) (dir string, dropThread bool) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return fallback, false
	}
	if !isDir(cwd) {
		return fallback, true
	}
	return cwd, false
}
