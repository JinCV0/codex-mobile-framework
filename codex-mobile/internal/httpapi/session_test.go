package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codexmobile/internal/store"
)

// 手机点「不指定项目」发的是 {"cwd": ""}：这条会话必须真的不绑目录，
// 而不是悄悄用上一条对话的目录（那个 bug 就是从这儿来的）。
func TestSessionNewNoProject(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	db.DefaultCwd = t.TempDir()
	db.NewSession(project, "")
	s := &Server{DB: db}

	post := func(body string) store.SessionSummary {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/session/new", strings.NewReader(body))
		s.sessionNew(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("body=%s 状态码 %d: %s", body, rec.Code, rec.Body.String())
		}
		var out struct {
			Session store.SessionSummary `json:"session"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Session
	}

	if got := post(`{"cwd":""}`); got.Cwd != "" {
		t.Fatalf("「不指定项目」拿到的目录 = %q, 应该是空的", got.Cwd)
	}
	if got := post(`{"cwd":"   "}`); got.Cwd != "" {
		t.Fatalf("只有空白的 cwd 也该按「不指定项目」处理，实际 = %q", got.Cwd)
	}
	if got := post(`{}`); got.Cwd != project {
		t.Fatalf("整段不带 cwd 时应沿用上次用过的目录 %q, 实际 = %q", project, got.Cwd)
	}

	raw, err := json.Marshal(map[string]any{"cwd": project})
	if err != nil {
		t.Fatal(err)
	}
	if got := post(string(raw)); got.Cwd != project {
		t.Fatalf("指定了目录就用它：想要 %q, 实际 %q", project, got.Cwd)
	}
}

// 每一轮开始前都要决定"这条对话在哪个目录跑"。「不指定项目」的会话 cwd 一直空着，
// 它不能被当成"目录没了" —— 否则每发一条消息都会丢掉线程号、另起一条新线程。
func TestRunDirForNoProjectKeepsThread(t *testing.T) {
	fallback := t.TempDir()

	dir, drop := runDirFor("", fallback)
	if dir != fallback || drop {
		t.Fatalf("没绑目录时应落到默认目录且保留线程，实际 dir=%q drop=%v", dir, drop)
	}

	dir, drop = runDirFor("   ", fallback)
	if dir != fallback || drop {
		t.Fatalf("只有空格时也一样，实际 dir=%q drop=%v", dir, drop)
	}

	dir, drop = runDirFor(t.TempDir(), fallback)
	if dir == fallback || drop {
		t.Fatalf("目录还在就该用它、不该丢线程，实际 dir=%q drop=%v", dir, drop)
	}

	gone := filepath.Join(t.TempDir(), "已经删掉的目录")
	dir, drop = runDirFor(gone, fallback)
	if dir != fallback || !drop {
		t.Fatalf("目录没了就退回默认目录并丢掉旧线程，实际 dir=%q drop=%v", dir, drop)
	}
}
