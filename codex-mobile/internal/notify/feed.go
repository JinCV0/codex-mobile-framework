package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Item 是从 ntfy 频道收到的一条消息。
type Item struct {
	ID    string `json:"id"`
	Time  int64  `json:"time"`
	Title string `json:"title"`
	Body  string `json:"body"`
	Topic string `json:"topic"`
}

// Feed 定期去 ntfy 拉取消息并缓存下来。
//
// 为什么要缓存：ntfy 免费版只保留 12 小时的消息。手机不可能一直在线，
// 所以让一直开着的电脑去拉、去存，手机什么时候来都能看到完整的。
type Feed struct {
	mu       sync.Mutex
	path     string
	url      string
	interval time.Duration
	on       bool
	items    []Item
	since    int64
	lastErr  string
	lastOK   int64
	seen     map[string]bool
	// local 记着"我们自己塞进来的消息标题"。任务播报会同时写本机缓存和
	// 推到 ntfy 频道，频道那头再拉回来就是同一件事，按标题认出来跳过。
	local    map[string]bool
	localSeq []string
}

const feedLimit = 500

// NewFeed 建一个频道缓存。defaultURL 是第一次运行时的默认频道，
// 也就是说"通知"默认是开着的，用户什么都不用设置就能收到消息；
// 用户手动关掉之后，下次启动不会又自己打开。
func NewFeed(path, defaultURL string) *Feed {
	f := &Feed{
		path: path, interval: 30 * time.Second,
		seen: map[string]bool{}, local: map[string]bool{},
	}
	f.load(defaultURL)
	return f
}

func (f *Feed) load(defaultURL string) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		// 还没配置过：默认打开，指向预置频道
		f.url = defaultURL
		f.on = defaultURL != ""
		return
	}
	var saved struct {
		URL   string   `json:"url"`
		On    bool     `json:"on"`
		Items []Item   `json:"items"`
		Local []string `json:"local"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	f.url, f.on, f.items = saved.URL, saved.On, saved.Items
	// local 记的是"我们自己推上去的标题"，用来把频道里回来的那一份认出来。
	// 要落盘：重启之后如果不认了，同一条消息会在「通知」里出现两遍。
	for _, t := range saved.Local {
		if t != "" {
			f.local[t] = true
			f.localSeq = append(f.localSeq, t)
		}
	}
	for _, it := range f.items {
		f.seen[it.ID] = true
		if it.Time > f.since {
			f.since = it.Time
		}
	}
}

// save 必须在持锁时调用
func (f *Feed) save() {
	raw, err := json.Marshal(map[string]any{
		"url": f.url, "on": f.on, "items": f.items, "local": f.localSeq,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(f.path, raw, 0o600)
}

// Configure 设置频道地址、开关和拉取间隔。
func (f *Feed) Configure(url string, on bool, intervalSec int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if url != "" && url != f.url {
		f.url = strings.TrimSpace(url)
		f.items = nil
		f.seen = map[string]bool{}
		f.local = map[string]bool{}
		f.localSeq = nil
		f.since = 0
	}
	f.on = on
	if intervalSec >= 5 && intervalSec <= 3600 {
		f.interval = time.Duration(intervalSec) * time.Second
	}
	f.save()
}

func (f *Feed) State() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"url": f.url, "on": f.on, "count": len(f.items),
		"interval": int(f.interval.Seconds()), "lastError": f.lastErr, "lastOk": f.lastOK,
	}
}

// Items 返回缓存的全部消息，新的在前。
func (f *Feed) Items() []Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Item, len(f.items))
	for i, it := range f.items {
		out[len(f.items)-1-i] = it
	}
	return out
}

// Has 看某条消息在不在（用来判断"这个版本已经通知过了"）。
func (f *Feed) Has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[id]
}

// AddLocal 塞一条本地生成的消息（不是从频道拉来的），比如"新版本可以更新了"。
// 已经有过同 id 的就返回 false，不会重复打扰。
func (f *Feed) AddLocal(id, title, body string, ts int64) bool {
	if id == "" {
		return false
	}
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen[id] {
		return false
	}
	f.seen[id] = true
	if title != "" {
		if !f.local[title] {
			f.localSeq = append(f.localSeq, title)
		}
		f.local[title] = true
		if len(f.localSeq) > 100 {
			delete(f.local, f.localSeq[0])
			f.localSeq = append([]string{}, f.localSeq[1:]...)
		}
	}
	f.items = append(f.items, Item{ID: id, Time: ts, Title: title, Body: body, Topic: "本机"})
	if ts > f.since {
		f.since = ts
	}
	if len(f.items) > feedLimit {
		drop := f.items[:len(f.items)-feedLimit]
		for _, d := range drop {
			delete(f.seen, d.ID)
		}
		f.items = append([]Item{}, f.items[len(f.items)-feedLimit:]...)
	}
	f.save()
	return true
}

// Run 按间隔一直拉，直到 ctx 结束。
func (f *Feed) Run(ctx context.Context) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			f.pollOnce(ctx)
			f.mu.Lock()
			d := f.interval
			f.mu.Unlock()
			timer.Reset(d)
		}
	}
}

// PollNow 立刻拉一次（界面上的"立即刷新"）。
func (f *Feed) PollNow(ctx context.Context) { f.pollOnce(ctx) }

func (f *Feed) pollOnce(ctx context.Context) {
	f.mu.Lock()
	url, on, since := f.url, f.on, f.since
	f.mu.Unlock()
	if !on || url == "" {
		return
	}

	// ntfy 的 poll=1 模式：拉一次就返回，配合 since 只取新的
	endpoint := strings.TrimRight(url, "/") + "/json?poll=1"
	if since > 0 {
		endpoint += fmt.Sprintf("&since=%d", since)
	} else {
		endpoint += "&since=12h"
	}

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, endpoint, nil)
	if err != nil {
		f.fail(err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.fail(fmt.Errorf("ntfy 返回 %d", resp.StatusCode))
		return
	}

	var fresh []Item
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var m struct {
			ID      string `json:"id"`
			Time    int64  `json:"time"`
			Event   string `json:"event"`
			Topic   string `json:"topic"`
			Message string `json:"message"`
			Title   string `json:"title"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Event != "message" {
			continue
		}
		if m.ID == "" {
			m.ID = fmt.Sprintf("%d-%d", m.Time, len(m.Message))
		}
		fresh = append(fresh, Item{
			ID: m.ID, Time: m.Time, Title: m.Title, Body: m.Message, Topic: m.Topic,
		})
	}

	f.mu.Lock()
	for _, it := range fresh {
		if f.seen[it.ID] {
			continue
		}
		// 频道里这条就是我们自己推上去的（本机已经存了更全的版本），
		// 不再收一遍；但游标要跟着走，否则每轮都会重新拉它。
		if it.Title != "" && f.local[it.Title] {
			if it.Time > f.since {
				f.since = it.Time
			}
			continue
		}
		f.seen[it.ID] = true
		f.items = append(f.items, it)
		if it.Time > f.since {
			f.since = it.Time
		}
	}
	if len(f.items) > feedLimit {
		drop := f.items[:len(f.items)-feedLimit]
		for _, d := range drop {
			delete(f.seen, d.ID)
		}
		f.items = append([]Item{}, f.items[len(f.items)-feedLimit:]...)
	}
	f.lastErr = ""
	f.lastOK = time.Now().Unix()
	f.save()
	f.mu.Unlock()
}

func (f *Feed) fail(err error) {
	f.mu.Lock()
	f.lastErr = err.Error()
	f.mu.Unlock()
}
