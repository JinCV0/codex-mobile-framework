// Package store 负责会话、常用项目和推送配置的持久化。
// 存储格式与旧的 Node 版本保持一致，方便直接沿用现有界面。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// SandboxModes 是 codex 支持的三种沙箱模式。
var SandboxModes = []string{"read-only", "workspace-write", "danger-full-access"}

// ApprovalModes 是 codex 的审批策略。
var ApprovalModes = []string{"untrusted", "on-request", "never"}

const (
	DefaultSandbox = "workspace-write"
	// DefaultApproval = on-request：默认只在需要越过沙箱时才问，
	// 问的时候手机上会弹出审批卡片。
	DefaultApproval = "on-request"
)

type Message struct {
	Role       string   `json:"role"`
	Text       string   `json:"text"`
	Activities []string `json:"activities,omitempty"`
	Ts         int64    `json:"ts"`
}

type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	Sandbox   string    `json:"sandbox"`
	Approval  string    `json:"approval"`
	ThreadID  string    `json:"threadId"`
	CreatedAt int64     `json:"createdAt"`
	UpdatedAt int64     `json:"updatedAt"`
	Messages  []Message `json:"messages"`
}

type Favorite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

// Service 是这个控制平台上的一个入口：一个网页、一台服务器、一个通知频道。
type Service struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Desc string `json:"desc"`
	Icon string `json:"icon"`
	// Group 用于把同一台服务器上的多个入口聚合展示。通用框架不预置
	// 任何分组，使用者可以按自己的部署方式添加。
	Group string `json:"group,omitempty"`
	// Proxy 是经由电脑中转的入口。手机上直接打开 URL 是进不去的
	// （tailnet 内部地址只有电脑能到），必须走这个。
	Proxy string `json:"proxy,omitempty"`
}

// defaultServices 首次运行时预置的入口。
func defaultServices() []Service {
	return nil
}

// ensureServices 把预置入口补齐。
func (db *DB) ensureServices() {
	def := defaultServices()
	have := map[string]int{}
	for i := range db.Services {
		have[db.Services[i].ID] = i
	}
	changed := false
	for _, d := range def {
		if i, ok := have[d.ID]; ok {
			cur := &db.Services[i]
			if cur.Name != d.Name || cur.Desc != d.Desc || cur.Icon != d.Icon ||
				cur.URL != d.URL || cur.Group != d.Group {
				*cur = d
				changed = true
			}
			continue
		}
		db.Services = append(db.Services, d)
		have[d.ID] = len(db.Services) - 1
		changed = true
	}
	if changed {
		db.save()
	}
}

// DefaultNtfyURL 为空表示通用框架不预置任何外部通知频道。
func DefaultNtfyURL() string { return "" }

// DefaultTaskWatch 默认关闭，避免新安装自动扫描与用户无关的目录。
func DefaultTaskWatch() TaskWatch {
	return TaskWatch{Inited: true, On: false}
}

// discoverTaskRoots 找出本机上计划任务的产出目录（知识库）。
//
// 不写死路径：按"Codex\<日期>\wo\outputs\ 下面、带 00-收件箱 这种子目录"
// 这个形状找一遍，找到几个就盯几个。找不到就留空，界面上可以手填。
func discoverTaskRoots() []string {
	if env := strings.TrimSpace(os.Getenv("CODEX_MOBILE_TASK_ROOTS")); env != "" {
		var out []string
		for _, p := range strings.FieldsFunc(env, func(r rune) bool { return r == ';' || r == '\n' }) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, pattern := range []string{
		filepath.Join(home, "Documents", "Codex", "*", "wo", "outputs", "*-知识库"),
		filepath.Join(home, "Documents", "Codex", "*", "wo", "outputs", "*", "00-收件箱"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			dir := m
			if filepath.Base(m) == "00-收件箱" {
				dir = filepath.Dir(m)
			}
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() || seen[strings.ToLower(dir)] {
				continue
			}
			seen[strings.ToLower(dir)] = true
			out = append(out, dir)
		}
		if len(out) > 0 {
			break
		}
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

type Notify struct {
	Channel string `json:"channel"`
	Value   string `json:"value"`
	URL     string `json:"url"`
	Format  string `json:"format"`
	OnDone  bool   `json:"onDone"`
	// OnApproval 管"跑一半停下来等你拍板"时要不要推送，和完成通知互不相干。
	OnApproval bool `json:"onApproval"`
}

// TaskWatch 是"计划任务产出播报"的设置。
//
// Inited 用来区分"老存档里没有这一项"和"用户自己关掉了"：前者补默认值
// （开着 + 自动找到的知识库目录），后者原样保留。
type TaskWatch struct {
	Inited bool     `json:"inited"`
	On     bool     `json:"on"`
	Roots  []string `json:"roots"`
}

type DB struct {
	mu          sync.Mutex
	path        string
	Sessions    []Session  `json:"sessions"`
	ActiveID    string     `json:"activeId"`
	LastCwd     string     `json:"lastCwd"`
	LastSandbox string     `json:"lastSandbox"`
	Favorites   []Favorite `json:"favorites"`
	Services    []Service  `json:"services"`
	Notify      Notify     `json:"notify"`
	TaskWatch   TaskWatch  `json:"taskWatch"`
	Machine     string     `json:"machine"`
	DefaultCwd  string     `json:"-"`
}

// Open 读取磁盘上的数据；文件不存在时返回一份默认值。
func Open(path string) (*DB, error) {
	db := &DB{
		path:        path,
		LastSandbox: DefaultSandbox,
		Machine:     hostname(),
		Services:    defaultServices(),
		Notify:      Notify{Channel: "none", Format: "json", OnDone: true},
		TaskWatch:   DefaultTaskWatch(),
	}
	raw, err := os.ReadFile(path)
	if err == nil && len(raw) > 0 {
		var loaded DB
		if err := json.Unmarshal(raw, &loaded); err == nil {
			loaded.path = path
			if loaded.LastSandbox == "" {
				loaded.LastSandbox = DefaultSandbox
			}
			for i := range loaded.Sessions {
				if !validApproval(loaded.Sessions[i].Approval) {
					loaded.Sessions[i].Approval = DefaultApproval
				}
			}
			if loaded.Notify.Channel == "" {
				loaded.Notify = Notify{Channel: "none", Format: "json", OnDone: true}
			}
			if loaded.Machine == "" {
				loaded.Machine = hostname()
			}
			if loaded.Services == nil {
				loaded.Services = defaultServices()
			}
			if !loaded.TaskWatch.Inited {
				loaded.TaskWatch = DefaultTaskWatch()
			}
			loaded.ensureServices()
			return &loaded, nil
		}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return db, nil
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// save 必须在持有锁的情况下调用。
func (db *DB) save() {
	raw, err := json.MarshalIndent(db, "", " ")
	if err != nil {
		return
	}
	tmp := db.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, db.path)
}

// ---------- 会话 ----------

// Active 返回当前会话的副本，第二个返回值表示是否存在。
func (db *DB) Active() (Session, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if s := db.activeLocked(); s != nil {
		copy := *s
		copy.Messages = append([]Message{}, s.Messages...)
		return copy, true
	}
	return Session{}, false
}

func (db *DB) activeLocked() *Session {
	for i := range db.Sessions {
		if db.Sessions[i].ID == db.ActiveID {
			return &db.Sessions[i]
		}
	}
	return nil
}

func validSandbox(mode string) bool {
	for _, m := range SandboxModes {
		if m == mode {
			return true
		}
	}
	return false
}

func validApproval(mode string) bool {
	for _, m := range ApprovalModes {
		if m == mode {
			return true
		}
	}
	return false
}

// NewSession 新建会话并设为当前会话。
//
// cwd 为空表示"调用方没有指定"，沿用上次用过的目录。
// 手机上那个「不指定项目」不是这个意思，走 NewSessionNoProject：
// 以前两者共用这一个入口，于是"不指定项目"被塞回上一条对话的目录，
// 表现就是"新增对话还是进了当前项目"。
func (db *DB) NewSession(cwd, sandbox string) Session {
	db.mu.Lock()
	defer db.mu.Unlock()

	if cwd = strings.TrimSpace(cwd); cwd == "" {
		cwd = db.lastDirLocked()
	}
	return db.newSessionLocked(cwd, sandbox)
}

// NewSessionNoProject 新建一条不绑定工作目录的会话。
//
// 目录留空（界面上显示"未指定项目"），真正开跑时才落到默认目录。
func (db *DB) NewSessionNoProject(sandbox string) Session {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.newSessionLocked("", sandbox)
}

// lastDirLocked 返回"没指定目录时该用哪个"：上次用过的，没了就退回默认目录。
// 调用者必须已经持有 db.mu。
func (db *DB) lastDirLocked() string {
	if isDir(db.LastCwd) {
		return db.LastCwd
	}
	return db.DefaultCwd
}

// newSessionLocked 建一条会话并设为当前；调用者必须已经持有 db.mu。
func (db *DB) newSessionLocked(cwd, sandbox string) Session {
	if !validSandbox(sandbox) {
		sandbox = db.LastSandbox
	}
	if !validSandbox(sandbox) {
		sandbox = DefaultSandbox
	}
	now := time.Now().UnixMilli()
	s := Session{
		ID: newID(), Title: "新对话", Cwd: cwd, Sandbox: sandbox,
		Approval:  DefaultApproval,
		CreatedAt: now, UpdatedAt: now, Messages: []Message{},
	}
	db.Sessions = append([]Session{s}, db.Sessions...)
	db.ActiveID = s.ID
	// "上次用过的目录"只记真实目录：不绑定项目的那条不该把它清掉，
	// 否则下一次"没指定目录"就没有可沿用的了。
	if cwd != "" {
		db.LastCwd = cwd
	}
	db.LastSandbox = sandbox
	db.save()
	return s
}

// Select 切换当前会话。
func (db *DB) Select(id string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			db.ActiveID = id
			// 切到一条"不指定项目"的对话时别把它当成"上次用过的目录"，
			// 那会把 LastCwd 清空。
			if db.Sessions[i].Cwd != "" {
				db.LastCwd = db.Sessions[i].Cwd
			}
			db.LastSandbox = db.Sessions[i].Sandbox
			db.save()
			return true
		}
	}
	return false
}

// ByThread 按 codex 线程号找回手机这边的会话。
//
// 电脑那边在某条对话里跑起来时，服务只拿到一个 threadId；
// 靠这个反查才知道"这一轮该算在哪条会话上"，手机界面才能把它显示出来。
func (db *DB) ByThread(threadID string) (Session, bool) {
	if threadID == "" {
		return Session{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ThreadID == threadID {
			return db.Sessions[i], true
		}
	}
	return Session{}, false
}

// ByID 按本地会话号取一条会话。
func (db *DB) ByID(id string) (Session, bool) {
	if id == "" {
		return Session{}, false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			return db.Sessions[i], true
		}
	}
	return Session{}, false
}

// Attach 绑定一条电脑上已有的对话；已经绑过就直接切过去。
func (db *DB) Attach(threadID, cwd, name string) (Session, string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ThreadID == threadID {
			// 之前可能是拿"电脑上的对话"这种占位标题接管的，
			// 这次如果知道了真实标题和工作目录就顺手补上。
			changed := false
			if name = strings.TrimSpace(name); name != "" && db.Sessions[i].Title != name {
				if utf8.RuneCountInString(name) > 40 {
					name = string([]rune(name)[:40])
				}
				db.Sessions[i].Title = name
				changed = true
			}
			if isDir(cwd) && cwd != db.Sessions[i].Cwd {
				db.Sessions[i].Cwd = cwd
				changed = true
			}
			db.ActiveID = db.Sessions[i].ID
			if changed {
				db.Sessions[i].UpdatedAt = time.Now().UnixMilli()
			}
			db.save()
			return db.Sessions[i], ""
		}
	}
	if !isDir(cwd) {
		cwd = db.DefaultCwd
	}
	title := strings.TrimSpace(name)
	if title == "" {
		title = "电脑上的对话"
	}
	if utf8.RuneCountInString(title) > 40 {
		title = string([]rune(title)[:40])
	}
	now := time.Now().UnixMilli()
	s := Session{
		ID: newID(), Title: title, Cwd: cwd, Sandbox: db.LastSandbox,
		Approval: DefaultApproval, ThreadID: threadID,
		CreatedAt: now, UpdatedAt: now, Messages: []Message{},
	}
	if !validSandbox(s.Sandbox) {
		s.Sandbox = DefaultSandbox
	}
	db.Sessions = append([]Session{s}, db.Sessions...)
	db.ActiveID = s.ID
	db.save()
	return s, ""
}

// Delete 删除会话。
func (db *DB) Delete(id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	kept := db.Sessions[:0]
	for _, s := range db.Sessions {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	db.Sessions = kept
	if db.ActiveID == id {
		if len(db.Sessions) > 0 {
			db.ActiveID = db.Sessions[0].ID
		} else {
			db.ActiveID = ""
			db.LastCwd = db.DefaultCwd
			db.LastSandbox = DefaultSandbox
		}
	}
	db.save()
}

// UpdateActive 修改当前会话的工作目录或沙箱模式。
func (db *DB) UpdateActive(cwd, sandbox, approval string) (Session, string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	s := db.activeLocked()
	if s == nil {
		return Session{}, "没有活动会话"
	}
	if cwd != "" {
		cwd = strings.TrimSpace(cwd)
		if cwd != "" {
			if !isDir(cwd) {
				return Session{}, "目录不存在：" + cwd
			}
			if cwd != s.Cwd {
				s.Cwd = cwd
				// 换了目录等于换了上下文，旧线程不能再用。
				s.ThreadID = ""
			}
			db.LastCwd = cwd
		}
	}
	if validSandbox(sandbox) {
		s.Sandbox = sandbox
		db.LastSandbox = sandbox
	}
	if validApproval(approval) {
		s.Approval = approval
	}
	s.UpdatedAt = time.Now().UnixMilli()
	db.save()
	return *s, ""
}

// Rename 改标题。
func (db *DB) Rename(id, title string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			db.Sessions[i].Title = title
			db.Sessions[i].UpdatedAt = time.Now().UnixMilli()
			db.save()
			return
		}
	}
}

// SetThreadID 记录 codex 线程号，下一轮用它续接会话。
//
// 顺手把"同一条电脑对话被接管过两次"的重复会话并掉：旧版本里如果先按
// 加号开了新会话、第一轮还没跑完就回列表点了那条电脑对话，就会生成两条
// 指向同一个 thread 的本地会话，手机上同一条对话会显示两遍。
func (db *DB) SetThreadID(id, threadID string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	target := -1
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			db.Sessions[i].ThreadID = threadID
			db.Sessions[i].UpdatedAt = time.Now().UnixMilli()
			target = i
			break
		}
	}
	if target < 0 {
		return
	}
	if threadID != "" {
		kept := make([]Session, 0, len(db.Sessions))
		for i, s := range db.Sessions {
			if i != target && s.ThreadID == threadID {
				// 被并掉的那条如果装着消息、而保留的这条是空的，把消息接过来
				if len(db.Sessions[target].Messages) == 0 && len(s.Messages) > 0 {
					db.Sessions[target].Messages = s.Messages
				}
				continue
			}
			kept = append(kept, s)
		}
		db.Sessions = kept
	}
	db.save()
}

// AddMessage 追加一条消息。
func (db *DB) AddMessage(id string, m Message) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			db.Sessions[i].Messages = append(db.Sessions[i].Messages, m)
			db.Sessions[i].UpdatedAt = time.Now().UnixMilli()
			db.save()
			return
		}
	}
}

// Touch 刷新会话的更新时间。
func (db *DB) Touch(id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			db.Sessions[i].UpdatedAt = time.Now().UnixMilli()
			db.save()
			return
		}
	}
}

// CountUserMessages 统计该会话里用户说了几句，用于判断是不是第一句。
func (db *DB) CountUserMessages(id string) int {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			n := 0
			for _, m := range db.Sessions[i].Messages {
				if m.Role == "user" {
					n++
				}
			}
			return n
		}
	}
	return 0
}

// Title 返回会话标题。
func (db *DB) Title(id string) string {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := range db.Sessions {
		if db.Sessions[i].ID == id {
			return db.Sessions[i].Title
		}
	}
	return ""
}

// ---------- 常用项目 ----------

func (db *DB) ListFavorites() []Favorite {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]Favorite{}, db.Favorites...)
}

// AddFavorite 添加或更新一个常用项目目录。
func (db *DB) AddFavorite(name, cwd string) ([]Favorite, string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil, "缺少目录"
	}
	if !isDir(cwd) {
		return nil, "目录不存在：" + cwd
	}
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(cwd)
		if name == "" {
			name = cwd
		}
	}
	for i := range db.Favorites {
		if db.Favorites[i].Cwd == cwd {
			db.Favorites[i].Name = name
			db.save()
			return append([]Favorite{}, db.Favorites...), ""
		}
	}
	db.Favorites = append(db.Favorites, Favorite{ID: newID(), Name: name, Cwd: cwd})
	db.save()
	return append([]Favorite{}, db.Favorites...), ""
}

func (db *DB) DeleteFavorite(id string) []Favorite {
	db.mu.Lock()
	defer db.mu.Unlock()
	kept := db.Favorites[:0]
	for _, f := range db.Favorites {
		if f.ID != id {
			kept = append(kept, f)
		}
	}
	db.Favorites = kept
	db.save()
	return append([]Favorite{}, db.Favorites...)
}

// ---------- 推送配置 ----------

func (db *DB) ListServices() []Service {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]Service{}, db.Services...)
}

// AddService 添加一个入口；同名同地址的会覆盖。
func (db *DB) AddService(name, url, desc, icon string) ([]Service, string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	name = strings.TrimSpace(name)
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, "缺少地址"
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = "https://" + url
	}
	if name == "" {
		name = url
	}
	if icon == "" {
		icon = "🔗"
	}
	for i := range db.Services {
		if db.Services[i].URL == url {
			db.Services[i].Name = name
			db.Services[i].Desc = strings.TrimSpace(desc)
			db.Services[i].Icon = icon
			db.save()
			return append([]Service{}, db.Services...), ""
		}
	}
	db.Services = append(db.Services, Service{ID: newID(), Name: name, URL: url,
		Desc: strings.TrimSpace(desc), Icon: icon})
	db.save()
	return append([]Service{}, db.Services...), ""
}

func (db *DB) DeleteService(id string) []Service {
	db.mu.Lock()
	defer db.mu.Unlock()
	kept := db.Services[:0]
	for _, s := range db.Services {
		if s.ID != id {
			kept = append(kept, s)
		}
	}
	db.Services = kept
	db.save()
	return append([]Service{}, db.Services...)
}

func (db *DB) NotifyConfig() Notify {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.Notify
}

// SetNotify 部分更新推送配置。
func (db *DB) SetNotify(channel, value, url, format *string, onDone, onApproval *bool) Notify {
	db.mu.Lock()
	defer db.mu.Unlock()
	if channel != nil {
		db.Notify.Channel = *channel
	}
	if value != nil {
		db.Notify.Value = strings.TrimSpace(*value)
	}
	if url != nil {
		u := strings.TrimSpace(*url)
		db.Notify.URL = u
		if u != "" && channel == nil {
			db.Notify.Channel = "custom"
		}
		if u == "" && channel == nil {
			db.Notify.Channel = "none"
			db.Notify.Value = ""
		}
	}
	if format != nil && (*format == "json" || *format == "text" || *format == "form") {
		db.Notify.Format = *format
	}
	if onDone != nil {
		db.Notify.OnDone = *onDone
	}
	if onApproval != nil {
		db.Notify.OnApproval = *onApproval
	}
	db.save()
	return db.Notify
}

// TaskWatchConfig 读任务播报设置。
func (db *DB) TaskWatchConfig() TaskWatch {
	db.mu.Lock()
	defer db.mu.Unlock()
	if !db.TaskWatch.Inited {
		db.TaskWatch = DefaultTaskWatch()
	}
	cfg := db.TaskWatch
	cfg.Roots = append([]string{}, db.TaskWatch.Roots...)
	return cfg
}

// SetTaskWatch 部分更新任务播报设置。
func (db *DB) SetTaskWatch(on *bool, roots *[]string) TaskWatch {
	db.mu.Lock()
	defer db.mu.Unlock()
	if !db.TaskWatch.Inited {
		db.TaskWatch = DefaultTaskWatch()
	}
	if on != nil {
		db.TaskWatch.On = *on
	}
	if roots != nil {
		var kept []string
		for _, r := range *roots {
			if r = strings.TrimSpace(r); r != "" && isDir(r) {
				kept = append(kept, r)
			}
		}
		db.TaskWatch.Roots = kept
	}
	db.save()
	cfg := db.TaskWatch
	cfg.Roots = append([]string{}, db.TaskWatch.Roots...)
	return cfg
}

// ---------- 对外的状态快照 ----------

type SessionSummary struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Cwd          string `json:"cwd"`
	Sandbox      string `json:"sandbox"`
	Approval     string `json:"approval"`
	ThreadID     string `json:"threadId,omitempty"`
	UpdatedAt    int64  `json:"updatedAt"`
	MessageCount int    `json:"messageCount"`
}

type ActivePayload struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Cwd      string    `json:"cwd"`
	Sandbox  string    `json:"sandbox"`
	Approval string    `json:"approval"`
	ThreadID string    `json:"threadId,omitempty"`
	Messages []Message `json:"messages"`
}

type State struct {
	Sessions   []SessionSummary `json:"sessions"`
	ActiveID   string           `json:"activeId"`
	Favorites  []Favorite       `json:"favorites"`
	Services   []Service        `json:"services"`
	Notify     Notify           `json:"notify"`
	Machine    string           `json:"machine"`
	Active     *ActivePayload   `json:"active"`
	DefaultCwd string           `json:"defaultCwd"`
	PublicURL  string           `json:"publicUrl"`
	// LANURLs 是这台电脑在局域网里的入口（http://<ip>:<port>/?k=...）。
	// 手机和电脑在同一个 WiFi 下时优先拿它直连，绕开公网隧道和运营商链路；
	// 出门用蜂窝时这些地址连不上，手机自己会退回 PublicURL。
	//
	// 每次 /api/state 现算：地址是路由器 DHCP 发的，换网络、续租就会变。
	LANURLs []string       `json:"lanUrls,omitempty"`
	Feed    map[string]any `json:"feed,omitempty"`
	Tasks   map[string]any `json:"tasks,omitempty"`
	Busy    bool           `json:"busy"`
	// Running 是"正在跑的那一轮"。手机离开界面也照样在电脑上跑完，
	// 列表和对话详情靠它显示"正在生成回答"。
	Running *RunInfo `json:"running,omitempty"`
}

// RunInfo 描述当前正在生成的那一轮对话。
type RunInfo struct {
	SessionID string `json:"sessionId"`
	ThreadID  string `json:"threadId,omitempty"`
	StartedAt int64  `json:"startedAt"`
}

// SetDefaultCwd 改新会话的默认工作目录。
func (db *DB) SetDefaultCwd(path string) string {
	db.mu.Lock()
	defer db.mu.Unlock()
	path = strings.TrimSpace(path)
	if path == "" {
		return "目录不能为空"
	}
	if !isDir(path) {
		return "目录不存在：" + path
	}
	db.DefaultCwd = path
	db.LastCwd = path
	db.save()
	return ""
}

// Snapshot 生成界面需要的完整状态。
func (db *DB) Snapshot(busy bool) State {
	db.mu.Lock()
	defer db.mu.Unlock()
	st := State{
		Sessions:   []SessionSummary{},
		ActiveID:   db.ActiveID,
		Favorites:  append([]Favorite{}, db.Favorites...),
		Services:   append([]Service{}, db.Services...),
		Notify:     db.Notify,
		Machine:    db.Machine,
		DefaultCwd: db.DefaultCwd,
		Busy:       busy,
	}
	if st.Favorites == nil {
		st.Favorites = []Favorite{}
	}
	for _, s := range db.Sessions {
		st.Sessions = append(st.Sessions, SessionSummary{
			ID: s.ID, Title: s.Title, Cwd: s.Cwd, Sandbox: s.Sandbox,
			Approval:  s.Approval,
			ThreadID:  s.ThreadID,
			UpdatedAt: s.UpdatedAt, MessageCount: len(s.Messages),
		})
	}
	if s := db.activeLocked(); s != nil {
		msgs := s.Messages
		if msgs == nil {
			msgs = []Message{}
		}
		st.Active = &ActivePayload{ID: s.ID, Title: s.Title, Cwd: s.Cwd, Sandbox: s.Sandbox,
			Approval: s.Approval, ThreadID: s.ThreadID, Messages: msgs}
	}
	return st
}

// Summary 生成单个会话的摘要，供新建/更新接口返回。
func (s Session) Summary() SessionSummary {
	return SessionSummary{
		ID: s.ID, Title: s.Title, Cwd: s.Cwd, Sandbox: s.Sandbox,
		Approval:  s.Approval,
		ThreadID:  s.ThreadID,
		UpdatedAt: s.UpdatedAt, MessageCount: len(s.Messages),
	}
}

func isDir(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
