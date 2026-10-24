package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexmobile/internal/store"
	"codexmobile/internal/taskwatch"
)

// 任务播报的接口：换目录 -> 试推一条 -> 状态里能看到。
func TestNotifyTasksEndpoint(t *testing.T) {
	dir := t.TempDir()
	inbox := filepath.Join(dir, "00-收件箱")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inbox, "日报-2026-10-05.md"),
		[]byte("# 日报-2026-10-05\n\n**1. 一篇论文**\n- 刊源：Food Chemistry（Q1）\n- 链接：https://example.com/a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	db, err := store.Open(filepath.Join(dataDir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	posted := make([]string, 0, 1)
	tasks := taskwatch.New(filepath.Join(dataDir, "task-watch.json"), taskwatch.Config{On: true, Roots: []string{dir}})
	tasks.Add = func(id, title, body string, ts int64) bool {
		posted = append(posted, title)
		return true
	}
	tasks.Push = func(title, body string) {}

	s := &Server{DB: db, Tasks: tasks}

	call := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/notify/tasks", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.notifyTasks(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("状态码 %d：%s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("返回不是 JSON：%s", w.Body.String())
		}
		return out
	}

	// 换成一个空目录：状态里要跟着变
	other := t.TempDir()
	out := call(`{"on":true,"roots":["` + strings.ReplaceAll(other, `\`, `\\`) + `"]}`)
	st, _ := out["tasks"].(map[string]any)
	if roots, _ := st["roots"].([]any); len(roots) != 1 || roots[0] != other {
		t.Fatalf("目录没存住：%+v", st)
	}

	// 试推最近一份日报
	out = call(`{"on":true,"roots":["` + strings.ReplaceAll(dir, `\`, `\\`) + `"],"try":true}`)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("试推失败：%+v", out)
	}
	if len(posted) != 1 || !strings.HasPrefix(posted[0], "文献日报 · 2026-10-05") {
		t.Fatalf("推出来的标题不对：%+v", posted)
	}

	// 目录里没有日报时给一句人话
	out = call(`{"roots":["` + strings.ReplaceAll(other, `\`, `\\`) + `"],"try":true}`)
	if ok, _ := out["ok"].(bool); ok {
		t.Fatalf("空目录不该推成功：%+v", out)
	}
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatalf("应该带一句说明：%+v", out)
	}
}
