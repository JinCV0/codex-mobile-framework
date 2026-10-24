// Package taskwatch 把"计划任务的产出"播报到手机。
//
// 计划任务（Codex 里的定时任务）在这台电脑上跑完，会把成果写进知识库目录：
// 日报、精读笔记、概念卡、周报…… 这些文件平时只有坐在电脑前才看得到。
// 这里每隔一会儿扫一遍那几个目录，发现新产出就攒成一条消息：
//
//  1. 写进本机「通知」缓存 —— 手机 App 一拉就有，能弹系统通知，点开看全文
//  2. 推到 ntfy 频道 —— 手机上用任何 ntfy 客户端也收得到
//
// 只读文件，不碰知识库里的任何内容。
package taskwatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 扫描节奏。settleAfter 是"文件多久没动才算写完"，任务中途边写边扫会重复播报。
const (
	scanEvery   = 40 * time.Second
	settleAfter = 75 * time.Second
	// 本机「通知」里留的全文上限；推给第三方的那份更短（ntfy 单条 4KB）。
	maxLocalBytes = 6000
	maxPushBytes  = 3900
)

type kind int

const (
	kindNote  kind = iota // 普通 markdown：摘开头几行
	kindDaily             // 日报：压成"分类 + 条目 + 链接"的清单
	kindPlain             // html 这类不读内容，只报一句
)

// rule 描述一类产出：在知识库的哪个子目录、文件名长什么样、怎么摘要。
type rule struct {
	Label  string
	Dir    string
	Prefix string
	Ext    string
	Kind   kind
}

// rules 对应这套知识库里各计划任务的产出位置，顺序就是消息里的排列顺序。
var rules = []rule{
	{Label: "文献日报", Dir: "00-收件箱", Prefix: "日报-", Ext: ".md", Kind: kindDaily},
	{Label: "精读笔记", Dir: "01-文献库", Prefix: "论文-", Ext: ".md", Kind: kindNote},
	{Label: "精读笔记", Dir: "01-文献库", Prefix: "综述-", Ext: ".md", Kind: kindNote},
	{Label: "概念卡", Dir: "02-概念库", Prefix: "", Ext: ".md", Kind: kindNote},
	{Label: "技能笔记", Dir: "03-技能库", Prefix: "", Ext: ".md", Kind: kindNote},
	{Label: "周报", Dir: "04-输出", Prefix: "周报-", Ext: ".md", Kind: kindNote},
	// 看板跟周报是同一个任务的两份产出，合成一条消息，别叮两次。
	{Label: "周报", Dir: "04-输出", Prefix: "看板-", Ext: ".html", Kind: kindPlain},
	{Label: "周复盘", Dir: "05-复盘", Prefix: "周复盘-", Ext: ".md", Kind: kindNote},
	{Label: "月复盘", Dir: "05-复盘", Prefix: "月复盘-", Ext: ".md", Kind: kindNote},
}

// Item 是一条组装好的播报。
type Item struct {
	ID    string
	Title string
	Body  string // 本机「通知」里显示的版本
	Push  string // 推到 ntfy / 微信的版本（按字节截断过）
	At    int64
}

// Config 是任务播报的设置。
type Config struct {
	On    bool     `json:"on"`
	Roots []string `json:"roots"`
}

// stamp 记住一个文件上次播报时的样子，用来判断"变没变"。
type stamp struct {
	Mtime int64  `json:"mtime"`
	Size  int64  `json:"size"`
	Hash  string `json:"hash"`
}

// Watcher 定时扫描知识库目录，把新产出播报出去。
type Watcher struct {
	// Add 把一条消息放进本机「通知」缓存；Push 推第三方渠道。
	Add  func(id, title, body string, ts int64) bool
	Push func(title, body string)
	// Now 可替换，测试用。
	Now func() time.Time

	mu       sync.Mutex
	scanMu   sync.Mutex // 同一时间只跑一遍扫描（定时轮询和界面上的"扫一次"可能撞上）
	path     string
	cfg      Config
	stamps   map[string]stamp
	seeded   bool
	lastScan int64
	lastSent string
	lastAt   int64
	total    int
	lastErr  string
	// hello 表示"刚装上，还没跟用户打过招呼"，第一遍扫描之后播一条。
	hello bool
}

// New 建一个播报器。statePath 用来记住"哪些文件已经播报过"，重启不会重复推。
func New(statePath string, cfg Config) *Watcher {
	w := &Watcher{path: statePath, cfg: normalize(cfg), stamps: map[string]stamp{}, Now: time.Now}
	w.hello = !w.load()
	return w
}

func (w *Watcher) load() bool {
	raw, err := os.ReadFile(w.path)
	if err != nil {
		// 第一次运行：seeded 保持 false，头一遍扫描只登记不播报，
		// 免得刚装上就把一整个月的旧日报全推一遍。
		return false
	}
	var saved struct {
		Stamps   map[string]stamp `json:"stamps"`
		Seeded   bool             `json:"seeded"`
		LastSent string           `json:"lastSent"`
		LastAt   int64            `json:"lastAt"`
		Total    int              `json:"total"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		return false
	}
	if saved.Stamps != nil {
		w.stamps = saved.Stamps
	}
	w.seeded, w.lastSent, w.lastAt, w.total = saved.Seeded, saved.LastSent, saved.LastAt, saved.Total
	return true
}

// save 必须在持锁时调用。
func (w *Watcher) save() {
	raw, err := json.Marshal(map[string]any{
		"stamps": w.stamps, "seeded": w.seeded,
		"lastSent": w.lastSent, "lastAt": w.lastAt, "total": w.total,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(w.path, raw, 0o600)
}

// SetConfig 换一套设置（开关、要盯的目录）。
func (w *Watcher) SetConfig(on bool, roots []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cfg = normalize(Config{On: on, Roots: roots})
	w.save()
}

// Config 返回当前设置。
func (w *Watcher) Config() Config {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Config{On: w.cfg.On, Roots: append([]string{}, w.cfg.Roots...)}
}

// State 给界面看的状态。
func (w *Watcher) State() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return map[string]any{
		"on":        w.cfg.On,
		"roots":     append([]string{}, w.cfg.Roots...),
		"count":     w.total,
		"seeded":    w.seeded,
		"lastScan":  w.lastScan,
		"lastSent":  w.lastSent,
		"lastAt":    w.lastAt,
		"lastError": w.lastErr,
	}
}

// Run 一直扫到 ctx 结束。
func (w *Watcher) Run(ctx context.Context) {
	w.Scan()
	t := time.NewTicker(scanEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Scan()
		}
	}
}

// pending 是一条扫到的、内容确实变了的产出。
type pending struct {
	r    rule
	full string
	name string
	mod  int64
	size int64
	hash string
	text string
}

// Scan 扫一遍所有目录：登记变化，把攒好的产出播报出去。
func (w *Watcher) Scan() {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()

	now := w.Now()
	w.mu.Lock()
	if w.stamps == nil {
		w.stamps = map[string]stamp{}
	}
	seeding := !w.seeded
	on, roots := w.cfg.On, append([]string{}, w.cfg.Roots...)
	w.lastScan = now.Unix()
	w.mu.Unlock()

	if !on || len(roots) == 0 {
		return
	}

	var found []pending
	errText := ""
	for _, root := range roots {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			errText = "找不到目录：" + root
			continue
		}
		for _, r := range rules {
			dir := filepath.Join(root, r.Dir)
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				name := e.Name()
				if e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
					continue
				}
				if !strings.HasPrefix(name, r.Prefix) || !strings.EqualFold(filepath.Ext(name), r.Ext) {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				full := filepath.Join(dir, name)
				key := strings.ToLower(full)
				mod, size := info.ModTime().Unix(), info.Size()

				w.mu.Lock()
				old, seen := w.stamps[key]
				w.mu.Unlock()
				if seen && old.Mtime == mod && old.Size == size {
					continue
				}
				// 任务可能还在往这个文件里写，等它安静下来再说。
				if !seeding && now.Sub(info.ModTime()) < settleAfter {
					continue
				}
				raw, err := os.ReadFile(full)
				if err != nil {
					continue
				}
				hash := hashOf(raw)
				if seen && old.Hash == hash {
					// 只是碰了时间戳，内容没变，登记一下就行。
					w.remember(key, mod, size, hash)
					continue
				}
				found = append(found, pending{
					r: r, full: full, name: name, mod: mod, size: size,
					hash: hash, text: string(raw),
				})
			}
		}
	}

	// 按标签分组，一次扫描里同一类的产出合成一条消息，
	// 免得"概念提炼"一次新建三张卡就叮三次。
	type group struct {
		label string
		items []pending
	}
	var groups []group
	index := map[string]int{}
	for _, p := range found {
		if i, ok := index[p.r.Label]; ok {
			groups[i].items = append(groups[i].items, p)
			continue
		}
		index[p.r.Label] = len(groups)
		groups = append(groups, group{label: p.r.Label, items: []pending{p}})
	}

	for _, g := range groups {
		item := build(g.label, g.items, now)
		for _, p := range g.items {
			w.remember(strings.ToLower(p.full), p.mod, p.size, p.hash)
		}
		if seeding {
			continue
		}
		w.emit(item)
	}

	w.mu.Lock()
	w.seeded = true
	w.lastErr = errText
	helloTodo := seeding && w.hello && len(roots) > 0
	if helloTodo {
		w.hello = false
	}
	w.save()
	w.mu.Unlock()

	if helloTodo {
		w.Hello(roots)
	}
}

// BroadcastLatest 立刻把最近一份日报推一条出来，给界面上的"试一条"用。
// 返回推出去的标题；没有日报时返回空串。
func (w *Watcher) BroadcastLatest() string {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()

	now := w.Now()
	var best *pending
	for _, root := range w.Config().Roots {
		dir := filepath.Join(root, "00-收件箱")
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "日报-") || !strings.EqualFold(filepath.Ext(name), ".md") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if best != nil && info.ModTime().Unix() <= best.mod {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			best = &pending{
				r:    dailyRule(),
				full: filepath.Join(dir, name), name: name,
				mod: info.ModTime().Unix(), size: info.Size(),
				hash: hashOf(raw), text: string(raw),
			}
		}
	}
	if best == nil {
		return ""
	}
	item := build("文献日报", []pending{*best}, now)
	w.emit(item)
	return item.Title
}

// dailyRule 找日报那条规则（试推最近一份日报时要用它来定名字和摘要方式）。
func dailyRule() rule {
	for _, r := range rules {
		if r.Kind == kindDaily {
			return r
		}
	}
	return rule{Label: "文献日报", Prefix: "日报-", Ext: ".md", Kind: kindDaily}
}

// Hello 播一条"已接入"，让用户知道这条通道通了。
func (w *Watcher) Hello(roots []string) {
	body := "以后这几个计划任务有了新产出，会自动推到这里：\n" +
		"文献日报 / 精读笔记 / 概念卡 / 技能笔记 / 周报看板 / 复盘。\n\n" +
		"盯的目录：" + strings.Join(roots, "、") + "\n" +
		"点开消息能看全文，链接可以直接在手机上打开。"
	w.emit(Item{
		ID: "task-hello", Title: "任务播报已接入", Body: body,
		Push: "计划任务有了新产出会自动推到这里（日报 / 精读 / 周报 / 复盘）。",
		At:   w.Now().Unix(),
	})
}

func (w *Watcher) remember(key string, mod, size int64, hash string) {
	w.mu.Lock()
	if w.stamps == nil {
		w.stamps = map[string]stamp{}
	}
	w.stamps[key] = stamp{Mtime: mod, Size: size, Hash: hash}
	w.save()
	w.mu.Unlock()
}

func (w *Watcher) emit(it Item) {
	if it.ID == "" {
		return
	}
	if w.Add != nil {
		w.Add(it.ID, it.Title, it.Body, it.At)
	}
	if w.Push != nil && strings.TrimSpace(it.Push) != "" {
		w.Push(it.Title, it.Push)
	}
	w.mu.Lock()
	w.total++
	w.lastSent, w.lastAt = it.Title, it.At
	w.save()
	w.mu.Unlock()
}

// build 把一批同类的产出攒成一条消息。
func build(label string, items []pending, now time.Time) Item {
	sort.Slice(items, func(i, j int) bool { return items[i].mod < items[j].mod })

	var blocks []block
	for _, p := range items {
		switch p.r.Kind {
		case kindDaily:
			blocks = append(blocks, dailyBlocks(p.text)...)
		case kindPlain:
			blocks = append(blocks, block{Text: cleanName(p.name, p.r.Prefix) + " 的看板已更新，在电脑上打开看"})
		default:
			head := "《" + cleanName(p.name, p.r.Prefix) + "》"
			if d := noteDigest(p.text, 600); d != "" {
				head += "\n" + d
			}
			blocks = append(blocks, block{Text: head})
		}
	}

	title := label
	switch {
	case len(items) == 1:
		one := cleanName(items[0].name, items[0].r.Prefix)
		title = label + " · " + one
	case len(items) > 1:
		title = fmt.Sprintf("%s · %d 个新产出", label, len(items))
	}
	if n := countEntries(blocks); n > 1 {
		title = fmt.Sprintf("%s（%d 条）", title, n)
	}

	body, droppedBody := fit(blocks, maxLocalBytes, false)
	if droppedBody > 0 {
		body += fmt.Sprintf("\n…后面还有 %d 段，完整内容在电脑上的文件里", droppedBody)
	}
	push, dropped := fit(blocks, maxPushBytes, true)
	if dropped > 0 {
		push += fmt.Sprintf("\n…还有 %d 段，打开「随行 · 通知」看全部", dropped)
	}
	if strings.TrimSpace(push) == "" {
		push = body
	}

	// ID 跟着内容走：同一天日报被追加内容时会重新生成一条，
	// 本机「通知」里也就跟着更新；内容没变则还是同一条，不会重复。
	last := items[len(items)-1]
	h := sha1.Sum([]byte(label + "|" + last.full + "|" + last.hash))
	return Item{
		ID:    "task-" + hex.EncodeToString(h[:])[:12],
		Title: title,
		Body:  body,
		Push:  push,
		At:    now.Unix(),
	}
}

func hashOf(raw []byte) string {
	h := sha1.Sum(raw)
	return hex.EncodeToString(h[:])
}

// normalize 去掉空目录、去重，让配置里不至于留一堆没用的空行。
func normalize(cfg Config) Config {
	out := make([]string, 0, len(cfg.Roots))
	seen := map[string]bool{}
	for _, r := range cfg.Roots {
		r = strings.TrimSpace(strings.Trim(r, `"`))
		if r == "" {
			continue
		}
		key := strings.ToLower(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return Config{On: cfg.On, Roots: out}
}
