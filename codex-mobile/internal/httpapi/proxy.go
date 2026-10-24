package httpapi

import (
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"codexmobile/internal/store"
)

// proxyService 让手机能访问 tailnet 内部的服务。
//
// 手机只是个浏览器壳，进不了 tailnet；而这台电脑在 tailnet 里，
// 所以由电脑去取内容再回传。
// 手机上看到的地址是 https://<电脑>.ts.net/proxy/<服务id>/...
func (s *Server) proxyService(w http.ResponseWriter, r *http.Request, id, rest string) {
	svc := s.serviceByID(id)
	if svc == nil {
		http.Error(w, "没有这个服务", http.StatusNotFound)
		return
	}
	target, err := url.Parse(svc.URL)
	if err != nil || target.Host == "" {
		http.Error(w, "服务地址不对", http.StatusBadGateway)
		return
	}

	prefix := "/proxy/" + id
	targetPath := target.Path
	aliases := s.proxyHostAliases()
	// 这个页面在目标站点上的目录。绝大多数服务在根目录，但有的不是——
	// 比如 Jellyfin 的界面挂在 /web/ 下，页面里的资源都是相对路径，
	// 我们注入的 <base> 必须跟着它，不然整套静态文件都会找错地方。
	dir := dirOf(rest)

	rp := &httputil.ReverseProxy{
		// 一律走内嵌的 tailnet 网络栈去取内容：这台电脑就算没装 Tailscale、
		// 或者系统里配了 HTTP 代理，也不影响它连上同一 tailnet 里的服务。
		Transport: s.tailnetTransport(),
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = joinPath(targetPath, rest)
			if target.RawQuery != "" {
				if req.URL.RawQuery == "" {
					req.URL.RawQuery = target.RawQuery
				} else {
					req.URL.RawQuery = target.RawQuery + "&" + req.URL.RawQuery
				}
			}
			req.Host = target.Host
			// 不要压缩：我们要改 HTML 和 Location
			req.Header.Del("Accept-Encoding")
			req.Header.Del("Connection")
			// 口令只属于我们这一层，别带给被代理的服务
			if q := req.URL.Query(); q.Get("k") == s.Token {
				q.Del("k")
				req.URL.RawQuery = q.Encode()
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); loc != "" {
				resp.Header.Set("Location", rewriteLocation(loc, target, prefix))
			}
			// 目标站点要是声明了 Referrer-Policy: no-referrer，下面那条
			// "根路径兜底"就认不出请求是从哪个服务发出来的了，直接摘掉。
			resp.Header.Del("Referrer-Policy")
			ct := resp.Header.Get("Content-Type")
			kind := textKind(ct)
			if kind == "" {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if err != nil {
				return err
			}
			out := string(body)
			switch kind {
			case "html":
				out = rewriteHTML(out, prefix, dir, aliases)
			case "css":
				out = rewriteCSS(out, prefix)
			case "js":
				out = rewriteJS(out, prefix, dir, aliases)
			}
			resp.Body = io.NopCloser(strings.NewReader(out))
			resp.ContentLength = int64(len(out))
			resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
			resp.Header.Del("Content-Encoding")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, proxyErrorPage(svc.Name, svc.URL, err))
		},
	}
	rp.ServeHTTP(w, r)
}

// dirOf 取一个路径所在的目录（含结尾的斜杠）。
// "/web/index.html" -> "/web/"；"/" -> "/"；"/a/b" -> "/a/"。
func dirOf(p string) string {
	if p == "" {
		return "/"
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	return "/"
}

// serviceByID 按 id 找一个服务入口。
func (s *Server) serviceByID(id string) *store.Service {
	for _, x := range s.DB.ListServices() {
		if x.ID == id {
			c := x
			return &c
		}
	}
	return nil
}

// proxyReferer 认出"这个请求是被哪个代理页面发出来的"。
//
// 被代理的网站常把地址写成根路径（fetch('/api/...')），那种请求绕不过
// 我们的前缀，会直接打到电脑端的根路径上。这时靠 Referer / Origin
// 里带着的 /proxy/<id>/ 认出来，再替他转给对应的服务。
func proxyReferer(r *http.Request) (string, bool) {
	for _, raw := range []string{r.Header.Get("Referer"), r.Header.Get("Origin")} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		p := u.Path
		if !strings.HasPrefix(p, "/proxy/") {
			continue
		}
		rest := strings.TrimPrefix(p, "/proxy/")
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		if rest != "" {
			return rest, true
		}
	}
	return "", false
}

func joinPath(a, b string) string {
	a = strings.TrimRight(a, "/")
	if b == "" {
		return a + "/"
	}
	return a + "/" + strings.TrimLeft(b, "/")
}

// rewriteLocation 把目标站点的跳转改回我们自己的代理前缀下。
func rewriteLocation(loc string, target *url.URL, prefix string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	if u.IsAbs() {
		if !strings.EqualFold(u.Host, target.Host) {
			return loc
		}
		u.Scheme, u.Host = "", ""
	}
	p := u.Path
	if !strings.HasPrefix(p, prefix) {
		p = prefix + "/" + strings.TrimLeft(p, "/")
	}
	if u.RawQuery != "" {
		return p + "?" + u.RawQuery
	}
	return p
}

var (
	// 单引号和双引号都要管：有的页面 href 和 src 就是单引号写的，
	// 只认双引号会漏掉一批。
	attrRe = regexp.MustCompile(`(?i)(\s(?:href|src|action|poster)\s*=\s*)(["'])/`)
	jsRe   = regexp.MustCompile("([\"'`])/(api|static|assets|js|css|img|media|ws|_app)/")
	// 前端框架都会在页面里声明"我从哪个路径提供服务"，空值或根值时我们补成
	// 代理地址：
	//
	//   const pageData = { baseURL: "" }        Glance：拿去拼 /api/...
	//   __sveltekit_xxx = { base: "" }          SvelteKit（Immich / Arcane）的路由基准
	//   window.BASE_PATH = '/'                  n8n
	//   globalThis.BESZEL = {"BASE_PATH":"/"}   Beszel
	//
	// 不补的话，前端路由会把 /proxy/<id> 当成业务路径，直接给一屏
	// "Not found: /proxy/xxx"（Immich 就是这么错的）。
	// 键名后面可能还跟一个引号（`"base": ""` 这种 JSON 写法），所以要放开。
	baseRe = regexp.MustCompile(`(?i)(\b(?:baseURL|base)\b\s*["']?\s*[:=]\s*)(["'])/?(["'])`)
	pathRe = regexp.MustCompile(`(?i)(\bBASE_PATH\b\s*["']?\s*[:=]\s*)(["'])/?(["'])`)
)

// textKind 判断一段正文要不要改写。javascript 也要改：好多单页应用
// 把接口地址写死在脚本里（fetch('/api/...')），不改的话请求会打到
// 电脑端自己身上，直接 403。
func textKind(ct string) string {
	ct = strings.ToLower(ct)
	switch {
	case strings.Contains(ct, "text/html"), strings.Contains(ct, "application/xhtml"):
		return "html"
	case strings.Contains(ct, "text/css"):
		return "css"
	case strings.Contains(ct, "javascript"), strings.Contains(ct, "ecmascript"):
		return "js"
	}
	return ""
}

// hostAlias 是"某个地址要换成某个代理前缀"。
type hostAlias struct{ from, to string }

// proxyHostAliases 收集所有服务入口的绝对地址。
//
// 被代理的页面里经常写死另一台端口上的地址（Glance 就用 iframe 嵌 Immich、
// Jellyfin、n8n），手机直连这些地址进不去，必须换成走电脑中转的地址。
func (s *Server) proxyHostAliases() []hostAlias {
	var out []hostAlias
	seen := map[string]bool{}
	for _, svc := range s.DB.ListServices() {
		u, err := url.Parse(svc.URL)
		if err != nil || u.Host == "" || u.Scheme == "" {
			continue
		}
		base := u.Scheme + "://" + u.Host
		if seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, hostAlias{from: base, to: "/proxy/" + svc.ID})
	}
	// 长的先换，免得 https://主机:8443 被 https://主机 先啃掉一截
	sort.Slice(out, func(i, j int) bool { return len(out[i].from) > len(out[j].from) })
	return out
}

func applyAliases(s string, aliases []hostAlias) string {
	for _, a := range aliases {
		s = strings.ReplaceAll(s, a.from, a.to)
	}
	return s
}

func mountOf(prefix, dir string) string {
	part := strings.Trim(dir, "/")
	if part == "" {
		return prefix
	}
	return prefix + "/" + part
}

func rewriteHTML(html, prefix, dir string, aliases []hostAlias) string {
	mount := mountOf(prefix, dir)
	if mount == "" {
		mount = prefix
	}
	html = baseRe.ReplaceAllString(html, `${1}${2}`+mount+`${3}`)
	// BASE_PATH 指的是"这个应用挂在哪个路径"，是服务根目录，不是这个文件
	// 所在的目录（n8n 的 base-path.js 放在 /static/ 下，但它说的是整个应用）。
	html = pathRe.ReplaceAllString(html, `${1}${2}`+prefix+`/${3}`)
	// 先改属性，再插 <base>。反过来的话，刚插进去的 base 也会被
	// 属性正则再改一次，变成 /proxy/x/proxy/x/ 这种双重前缀。
	out := attrRe.ReplaceAllString(html, `${1}${2}`+prefix+`/`)
	out = jsRe.ReplaceAllString(out, `${1}`+prefix+`/${2}/`)
	// 绝对地址放在最后换：换出来的 /proxy/... 不能再被上面两条规则碰一次，
	// 否则会变成 /proxy/glance/proxy/immich 这种双重前缀。
	out = applyAliases(out, aliases)
	if hasOwnBase(out) {
		// 页面自带 <base>（常见于挂在 CDN 上的站点），别把它顶掉
		return out
	}
	// <base> 指到"这个页面所在的目录"而不是服务根目录：Jellyfin 的界面挂在
	// /web/ 下，页面里的资源都是相对路径，指错了整套静态文件都会 404。
	base := `<base href="` + mount + `/">`
	low := strings.ToLower(out)
	if i := strings.Index(low, "<head>"); i >= 0 {
		out = out[:i+6] + base + out[i+6:]
	} else if i := strings.Index(low, "<html"); i >= 0 {
		if j := strings.Index(out[i:], ">"); j >= 0 {
			pos := i + j + 1
			out = out[:pos] + base + out[pos:]
		}
	}
	return out
}

// rewriteJS 把脚本里写死的根路径搬到代理前缀下。
//
// 这里只补 BASE_PATH 这一种明确的配置项；像 `base: ""` 那种写法在打包好的
// vendor 代码里到处都是，不能按名字去动。
func rewriteJS(js, prefix, dir string, aliases []hostAlias) string {
	out := pathRe.ReplaceAllString(js, `${1}${2}`+prefix+`/${3}`)
	out = jsRe.ReplaceAllString(out, `${1}`+prefix+`/${2}/`)
	return applyAliases(out, aliases)
}

// hasOwnBase 判断页面头部是不是已经有 <base>。
func hasOwnBase(html string) bool {
	low := strings.ToLower(html)
	end := strings.Index(low, "</head>")
	if end < 0 {
		if len(low) > 4096 {
			end = 4096
		} else {
			end = len(low)
		}
	}
	return strings.Contains(low[:end], "<base")
}

// rewriteCSS 把样式表里的绝对路径（url(/static/...)）也搬到代理前缀下，
// 否则字体、图标这类资源会去根路径找，直接 404。
// 注意不能吞掉协议相对地址（url(//cdn...）），那要原样留着。
func rewriteCSS(css, prefix string) string {
	var b strings.Builder
	rest := css
	for {
		i := strings.Index(strings.ToLower(rest), "url(")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:i+4])
		rest = rest[i+4:]
		j := 0
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t' || rest[j] == '"' || rest[j] == '\'') {
			j++
		}
		b.WriteString(rest[:j])
		rest = rest[j:]
		if len(rest) > 0 && rest[0] == '/' && (len(rest) == 1 || rest[1] != '/') {
			b.WriteString(prefix)
		}
	}
	return b.String()
}

func proxyErrorPage(name, rawURL string, err error) string {
	return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width,initial-scale=1"><title>连不上</title></head>` +
		`<body style="margin:0;min-height:100dvh;display:flex;align-items:center;justify-content:center;` +
		`background:#101114;color:#e6e7e9;font:15px/1.6 system-ui,-apple-system,'Microsoft YaHei',sans-serif">` +
		`<div style="text-align:center;padding:28px;max-width:420px">` +
		`<div style="font-size:17px;font-weight:600;margin-bottom:10px">连不上 ` + htmlEscape(name) + `</div>` +
		`<div style="color:#9a9ca3;font-size:13.5px">电脑访问 ` + htmlEscape(rawURL) + ` 失败</div>` +
		`<div style="color:#6b6d75;font-size:12.5px;margin-top:10px">` + htmlEscape(err.Error()) + `</div>` +
		`</div></body></html>`
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
