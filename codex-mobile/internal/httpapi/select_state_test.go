package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"codexmobile/internal/codex"
	"codexmobile/internal/store"
)

// 切对话必须把整份状态一起带回去。
//
// 手机打开一条对话原来是"select → state → 历史"三趟串行，走公网实测 1.7 秒；
// select 顺带回 state 之后少等一趟。这个回包要是丢了 sessions/active，
// 客户端就还得再问一次，优化等于没做。
func TestSessionSelectReturnsFullState(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	first := db.NewSession(t.TempDir(), "")
	target := db.NewSession(t.TempDir(), "")
	s := &Server{DB: db, Runner: &codex.Runner{}}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/session/select",
		strings.NewReader(`{"id":"`+target.ID+`"}`))
	s.sessionSelect(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d：%s", rec.Code, rec.Body.String())
	}
	var st store.State
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Active == nil || st.Active.ID != target.ID {
		t.Fatalf("select 之后 active 应该是刚选的那条，实际 %+v", st.Active)
	}
	if len(st.Sessions) < 2 {
		t.Fatalf("回包里必须带全 sessions，实际只有 %d 条", len(st.Sessions))
	}
	if st.ActiveID != target.ID {
		t.Fatalf("activeId 没跟着切：%q != %q", st.ActiveID, target.ID)
	}
	_ = first
}
