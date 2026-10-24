package httpapi

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 页面里必须带上"这一份外壳是哪个版本"的标记。
//
// 手机端的 Service Worker 是缓存优先，页面自己 reload 拿到的是旧缓存；
// 靠这个标记和 /api/status 的 build 一比，页面才知道该让 SW 去换新外壳
// （见 web/index.html 的 forceShellRefresh）。这里把它钉住，免得哪天
// 顺手删了替换、又回到"电脑上改好了手机十一个小时看不到"。
func TestServeHTMLInjectsBuildStamp(t *testing.T) {
	s := &Server{
		Token: "secret-token",
		Build: "24547328-1791571929",
		Web: fstest.MapFS{
			"index.html": &fstest.MapFile{
				Data: []byte(`const TOKEN = "__TOKEN__"; const SHELL_BUILD = "__BUILD__";`),
			},
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.serveHTML(rec, req, "index.html")

	body := rec.Body.String()
	if strings.Contains(body, "__TOKEN__") || strings.Contains(body, "__BUILD__") {
		t.Fatalf("占位符没被替换掉：%s", body)
	}
	if !strings.Contains(body, `SHELL_BUILD = "24547328-1791571929"`) {
		t.Fatalf("外壳版本号没注进去：%s", body)
	}
	if !strings.Contains(body, `TOKEN = "secret-token"`) {
		t.Fatalf("口令没注进去：%s", body)
	}

	// 带 ETag 回来要能拿到 304（手机重复打开不该重传整个外壳）
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("没给 ETag")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", etag)
	s.serveHTML(rec2, req2, "index.html")
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("带 ETag 复访应该回 304，实际 %d", rec2.Code)
	}
	_ = fs.ValidPath
}
