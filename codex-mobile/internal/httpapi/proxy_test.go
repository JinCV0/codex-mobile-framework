package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestRewriteHTMLAddsBaseOnce(t *testing.T) {
	src := `<html><head><title>首页</title></head><body><a href="/photos">相册</a></body></html>`
	out := rewriteHTML(src, "/proxy/remote", "/", nil)
	if n := strings.Count(out, "<base "); n != 1 {
		t.Fatalf("base tags = %d, want 1: %s", n, out)
	}
	if !strings.Contains(out, `<base href="/proxy/remote/">`) {
		t.Fatalf("base href wrong: %s", out)
	}
	if strings.Contains(out, "/proxy/remote/proxy/remote") {
		t.Fatalf("base href got double prefix: %s", out)
	}
	if !strings.Contains(out, `href="/proxy/remote/photos"`) {
		t.Fatalf("absolute link not rewritten: %s", out)
	}
}

func TestRewriteHTMLKeepsOwnBase(t *testing.T) {
	src := `<html><head><base href="/app/"><link rel="stylesheet" href="/app/x.css"></head><body></body></html>`
	out := rewriteHTML(src, "/proxy/x", "/", nil)
	if !strings.Contains(out, `<base href="/proxy/x/app/">`) {
		t.Fatalf("own base not rewritten: %s", out)
	}
	if n := strings.Count(out, "<base "); n != 1 {
		t.Fatalf("base tags = %d, want 1: %s", n, out)
	}
}

func TestRewriteCSS(t *testing.T) {
	src := `@font-face{src:url('../fonts/a.woff2')}`
	if got := rewriteCSS(src, "/proxy/x"); got != src {
		t.Fatalf("relative url should stay as is, got %s", got)
	}
	src = `body{background:url(/static/bg.png) no-repeat}a{mask:url("/static/m.svg")}`
	want := `body{background:url(/proxy/x/static/bg.png) no-repeat}a{mask:url("/proxy/x/static/m.svg")}`
	if got := rewriteCSS(src, "/proxy/x"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	proto := `@import url(//cdn.example.com/a.css);`
	if got := rewriteCSS(proto, "/proxy/x"); got != proto {
		t.Fatalf("protocol-relative url should stay as is, got %s", got)
	}
}

// 单页应用把接口地址写死在脚本里（fetch('/api/...')），这种请求绕不过
// 代理前缀，会打到电脑端自己身上变成 403。脚本正文也要一起改。
func TestRewriteJS(t *testing.T) {
	src := "const r = await fetch('/api/pages/1/content/');\n" +
		"fetch(`/api/set-theme/dark`, {method:'POST'});\n" +
		"const keep = 'https://example.com/api/x';\n"
	out := rewriteJS(src, "/proxy/remote", "/", nil)
	if !strings.Contains(out, "fetch('/proxy/remote/api/pages/1/content/')") {
		t.Fatalf("js absolute api path not rewritten: %s", out)
	}
	if !strings.Contains(out, "fetch(`/proxy/remote/api/set-theme/dark`") {
		t.Fatalf("js template literal not rewritten: %s", out)
	}
	if !strings.Contains(out, "https://example.com/api/x") {
		t.Fatalf("foreign url should stay as is: %s", out)
	}
}

// 被代理的页面里写死同一台服务器另一个端口的地址（Glance 用 iframe 嵌
// Immich、n8n），这些地址必须换成走电脑中转的入口，否则手机点进去就是空的。
func TestRewriteHTMLCrossPortAlias(t *testing.T) {
	aliases := []hostAlias{
		{from: "https://remote.example.ts.net:8443", to: "/proxy/immich"},
		{from: "https://remote.example.ts.net", to: "/proxy/glance"},
	}
	src := `<iframe src="https://remote.example.ts.net:8443/photos"></iframe>` +
		`<a href="https://remote.example.ts.net/">首页</a>`
	out := rewriteHTML(src, "/proxy/glance", "/", aliases)
	if !strings.Contains(out, `src="/proxy/immich/photos"`) {
		t.Fatalf("cross-port iframe not rewritten: %s", out)
	}
	if !strings.Contains(out, `href="/proxy/glance/"`) {
		t.Fatalf("same-host link not rewritten: %s", out)
	}
}

// 面板先声明一个空的"起始地址"，再拿它拼接口路径：baseURL 是空的时候
// fetch 出去的是根路径，我们接不住。把它补成代理前缀，这才是根治。
func TestRewriteHTMLFillsEmptyBaseURL(t *testing.T) {
	src := `<script>const pageData = { slug: "首页", baseURL: "", theme: "default" };</script>`
	out := rewriteHTML(src, "/proxy/remote-glance", "/", nil)
	if !strings.Contains(out, `baseURL: "/proxy/remote-glance"`) {
		t.Fatalf("empty baseURL not filled: %s", out)
	}
	// 已经有值的不能动
	src2 := `<script>const pageData = { baseURL: "/app" };</script>`
	out2 := rewriteHTML(src2, "/proxy/x", "/", nil)
	if !strings.Contains(out2, `baseURL: "/app"`) {
		t.Fatalf("non-empty baseURL should stay: %s", out2)
	}
}

// 界面挂在子目录下的服务（Jellyfin 的 /web/），<base> 要指到那个目录，
// 不然页面里的相对资源全会去找代理根，整套静态文件 404。
func TestRewriteHTMLSubdirBase(t *testing.T) {
	src := `<html><head></head><body><script src="runtime.bundle.js"></script></body></html>`
	out := rewriteHTML(src, "/proxy/jellyfin", "/web/", nil)
	if !strings.Contains(out, `<base href="/proxy/jellyfin/web/">`) {
		t.Fatalf("base should point at the sub directory: %s", out)
	}
}

// 前端框架把自己挂在哪个路径写在页面里，空值时要把代理前缀填进去，
// 否则路由会把 /proxy/<id> 当成业务路径（Immich 就是这样报 Not found 的）。
func TestRewriteHTMLFillsFrameworkBase(t *testing.T) {
	for _, src := range []string{
		`<script>__sveltekit_abc = { base: "" };</script>`,
		`<script>__sveltekit_abc = { base: "/" };</script>`,
		`<script>window.BASE_PATH = '/';</script>`,
		`<script>globalThis.BESZEL = {"BASE_PATH":"/","HUB_VERSION":"0.21.0"}</script>`,
	} {
		out := rewriteHTML(src, "/proxy/immich", "/", nil)
		if !strings.Contains(out, "/proxy/immich") {
			t.Fatalf("framework base not filled: %s -> %s", src, out)
		}
	}
	// 已经有值的不动
	src := `<script>__sveltekit_abc = { base: "/app" };</script>`
	if out := rewriteHTML(src, "/proxy/x", "/", nil); !strings.Contains(out, `base: "/app"`) {
		t.Fatalf("non-empty framework base should stay: %s", out)
	}
}

// n8n 把 BASE_PATH 单独放在 /static/base-path.js 里，它指的是整个应用挂在
// 哪个路径（服务根目录），不是这个脚本自己所在的目录。
func TestRewriteJSBasePathUsesServiceRoot(t *testing.T) {
	out := rewriteJS("window.BASE_PATH = '/';", "/proxy/n8n", "/static/", nil)
	if !strings.Contains(out, "window.BASE_PATH = '/proxy/n8n/';") {
		t.Fatalf("BASE_PATH should be the service root: %s", out)
	}
}

func TestDirOf(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "/"}, {"/", "/"}, {"web/", "web/"}, {"web/index.html", "web/"},
		{"a/b/c", "a/b/"}, {"photos", "/"},
	} {
		if got := dirOf(c.in); got != c.want {
			t.Fatalf("dirOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestProxyReferer(t *testing.T) {
	r, _ := http.NewRequest("GET", "https://pc.example.ts.net/api/pages/1/content/", nil)
	r.Header.Set("Referer", "https://pc.example.ts.net/proxy/remote/%E9%A6%96%E9%A1%B5")
	id, ok := proxyReferer(r)
	if !ok || id != "remote" {
		t.Fatalf("referer not recognised: %q %v", id, ok)
	}
	r2, _ := http.NewRequest("GET", "https://pc.example.ts.net/api/state", nil)
	r2.Header.Set("Referer", "https://pc.example.ts.net/")
	if _, ok := proxyReferer(r2); ok {
		t.Fatal("plain page referer should not match")
	}
}
