package taskwatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type capture struct {
	items []Item
	seen  map[string]bool
}

func newTestWatcher(t *testing.T, root string, at time.Time) (*Watcher, *capture) {
	t.Helper()
	cap := &capture{seen: map[string]bool{}}
	w := New(filepath.Join(t.TempDir(), "task-watch.json"), Config{On: true, Roots: []string{root}})
	w.Now = func() time.Time { return at }
	w.Add = func(id, title, body string, ts int64) bool {
		if cap.seen[id] {
			return false
		}
		cap.seen[id] = true
		cap.items = append(cap.items, Item{ID: id, Title: title, Body: body, At: ts})
		return true
	}
	w.Push = func(title, body string) {}
	return w, cap
}

func TestWatchSeedsThenReportsChanges(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "00-收件箱")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	// 装之前就存在的旧日报：不该被推出去
	old := filepath.Join(inbox, "日报-2026-09-01.md")
	if err := os.WriteFile(old, []byte("# 日报-2026-09-01\n\n**1. 旧内容**\n- 链接：https://example.com/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now().Add(-24 * time.Hour)
	w, cap := newTestWatcher(t, root, start)
	w.Scan() // 第一次：只登记 + 跟用户打个招呼
	if len(cap.items) != 1 || cap.items[0].Title != "任务播报已接入" {
		t.Fatalf("第一次扫描应该只播一条接入提示，实际 %+v", cap.items)
	}
	cap.items = nil

	// 新的日报落盘，等它安静下来
	fresh := filepath.Join(inbox, "日报-2026-10-05.md")
	if err := os.WriteFile(fresh, []byte(dailySample), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(3 * time.Minute)
	w.Now = func() time.Time { return when }
	w.Scan()
	if len(cap.items) != 1 {
		t.Fatalf("应该播报 1 条，实际 %d 条", len(cap.items))
	}
	if cap.items[0].Title != "文献日报 · 2026-10-05（3 条）" {
		t.Errorf("标题不对：%q", cap.items[0].Title)
	}

	// 再扫一遍：内容没变，不该重复播报
	cap.items = nil
	w.Now = func() time.Time { return when.Add(2 * time.Minute) }
	w.Scan()
	if len(cap.items) != 0 {
		t.Fatalf("内容没变却重复播报了 %d 条", len(cap.items))
	}

	// 同一份日报被追加内容 → 再播一次
	if err := os.WriteFile(fresh, []byte(dailySample+"\n**3. 追加的一条**\n- 链接：https://example.com/new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.Now = func() time.Time { return when.Add(5 * time.Minute) }
	w.Scan()
	if len(cap.items) != 1 {
		t.Fatalf("内容变了应该再播一条，实际 %d 条", len(cap.items))
	}
}

func TestWatchSkipsUnfinishedAndTemplates(t *testing.T) {
	root := t.TempDir()
	inbox := filepath.Join(root, "00-收件箱")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w, cap := newTestWatcher(t, root, now)
	w.hello = false
	w.Scan() // 登记空目录

	// 模板和私有文件不播
	for _, name := range []string{"_文献笔记模板.md", ".蒸馏登记.md"} {
		_ = os.WriteFile(filepath.Join(inbox, name), []byte("# 模板\n"), 0o600)
	}
	// 刚写完还没安静下来的日报，先不播
	_ = os.WriteFile(filepath.Join(inbox, "日报-2026-10-06.md"), []byte("# 日报\n\n**1. x**\n- 链接：https://example.com/x\n"), 0o600)

	w.Scan()
	if len(cap.items) != 0 {
		t.Fatalf("不该有播报，实际 %d 条", len(cap.items))
	}
}

// TestDryRunRealTree 是给自己看的彩排：拿本机真实的知识库跑一遍摘要，
// 看看手机上会收到什么样的标题和正文。设 TASKWATCH_REAL_ROOT 才会跑。
func TestDryRunRealTree(t *testing.T) {
	root := os.Getenv("TASKWATCH_REAL_ROOT")
	if root == "" {
		t.Skip("没有设置 TASKWATCH_REAL_ROOT，跳过")
	}
	now := time.Now()
	for _, r := range rules {
		dir := filepath.Join(root, r.Dir)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		var items []pending
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			if !strings.HasPrefix(name, r.Prefix) || !strings.EqualFold(filepath.Ext(name), r.Ext) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			items = append(items, pending{r: r, full: filepath.Join(dir, name), name: name, hash: hashOf(raw), text: string(raw)})
		}
		if len(items) == 0 {
			continue
		}
		// 只挑最新的一份，模拟"这次任务刚写完"的样子
		newest := items[0]
		for _, it := range items {
			info, err := os.Stat(it.full)
			if err == nil && info.ModTime().Unix() >= mustMod(t, newest.full) {
				newest = it
			}
		}
		item := build(r.Label, []pending{newest}, now)
		t.Logf("[%s] %s\n    本机 %d 字节 / 推送 %d 字节\n%s",
			r.Label, item.Title, len([]byte(item.Body)), len([]byte(item.Push)), firstLines(item.Push, 6))
	}
}

func mustMod(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().Unix()
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	out := strings.Join(lines, "\n")
	return "    " + strings.ReplaceAll(out, "\n", "\n    ")
}
