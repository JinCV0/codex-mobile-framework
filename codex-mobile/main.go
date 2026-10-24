// Codex 随行 · 电脑端程序
//
// 一个 exe 做四件事：
//  1. 把 Tailscale 用户态节点（tsnet）嵌进进程，不需要管理员权限
//  2. 默认只开 tailnet 内网入口，手机登录自己的 Tailscale 后访问
//  3. 内嵌手机网页界面，用官方 app-server 协议驱动本机 codex
//  4. 托盘常驻：看一眼就有地址和二维码，可选开机自启
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/sys/windows"

	"codexmobile/internal/appserver"
	"codexmobile/internal/autostart"
	"codexmobile/internal/bridge"
	"codexmobile/internal/codex"
	"codexmobile/internal/httpapi"
	"codexmobile/internal/netenv"
	"codexmobile/internal/notify"
	"codexmobile/internal/qr"
	"codexmobile/internal/store"
	"codexmobile/internal/sysmon"
	"codexmobile/internal/taskwatch"
	"codexmobile/internal/tray"
	"codexmobile/internal/tsnode"
)

//go:embed web
var webRoot embed.FS

// out 是全部输出的去处：日志文件 +（如果有）控制台。
var out io.Writer = os.Stdout

func main() {
	// 被桌面版 Codex 当成 CLI 拉起时走桥接这条路：
	// 让桌面端接到常驻服务上，两边共用同一个 app-server（详见 internal/bridge）。
	//
	// 注意这一步必须排在 attachParentConsole 之前：代理模式下 stdin/stdout
	// 就是桌面版的 JSON-RPC 通道，一个字都不能往上面写。
	//
	// 也必须排在 netenv.Strip 之前：桥接要把桌面版的环境原样带给 app-server。
	// 桌面版是 ChatGPT 桌面应用，它的 app-server 要去 chatgpt.com 取"组织设置"，
	// 而这台机器上 chatgpt.com 只能经系统代理（Clash 127.0.0.1:7877）访问。
	// 一旦把代理洗掉，桌面端就会卡在"无法加载组织设置"。
	if mode := bridge.DesktopMode(os.Args[1:]); mode != "" {
		os.Exit(bridge.RunDesktop(mode, os.Args[1:]))
	}

	// 这台电脑上挂着 Clash（系统代理 127.0.0.1:7877）。服务自己已经接了
	// Tailscale，出站走 tsnet 用户态网络栈，不需要再经一层代理，
	// 这里把环境里的代理变量清干净，之后拉起的 codex 子进程会继承这份环境。
	//
	// 注意：只对"我们自己起的服务"生效。桌面版那条路在上面已经退出，
	// 不受影响——否则会破坏桌面端的组织设置加载。
	netenv.Strip()

	// 以 GUI 子系统编译时没有控制台，但用户从终端启动时应该能看到输出。
	attachParentConsole()

	var (
		hostname  = flag.String("hostname", "codex-mobile", "在 tailnet 里显示的节点名")
		funnel    = flag.Bool("funnel", false, "开启 Funnel：额外提供公网 HTTPS 地址")
		tailnet   = flag.Bool("tailnet", true, "开启 tailnet 内网 HTTP 入口（:80）")
		localAddr = flag.String("local", "127.0.0.1:7799", "本机 HTTP 入口，留空则关闭")
		dataDir   = flag.String("data", "", "数据目录，默认 %LOCALAPPDATA%\\CodexMobile")
		useTray   = flag.Bool("tray", true, "显示托盘图标")
		openQR    = flag.Bool("qr", false, "启动后自动打开扫码页面")
		autoStart = flag.String("autostart", "", "只改开机自启设置然后退出：on / off / status")
	)
	flag.Parse()

	// 单独的开机自启开关：设完就退出，方便脚本调用。
	if *autoStart != "" {
		switch *autoStart {
		case "on":
			if err := autostart.Enable(); err != nil {
				log.Fatalf("开启开机自启失败: %v", err)
			}
			fmt.Println("开机自启：已开启")
		case "off":
			if err := autostart.Disable(); err != nil {
				log.Fatalf("关闭开机自启失败: %v", err)
			}
			fmt.Println("开机自启：已关闭")
		default:
			fmt.Printf("开机自启：%v\n", autostart.Enabled())
		}
		return
	}

	dir := *dataDir
	if dir == "" {
		dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "CodexMobile")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	out = setupOutput(dir)
	log.SetOutput(out)

	// 单实例：如果已经有一个在跑，就别再起一个（会抢端口和节点身份），
	// 直接把扫码页调出来——这样"双击启动"永远能得到那个码。
	if !acquireInstance(dir) {
		return
	}
	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()

	token := loadOrCreateToken(filepath.Join(dir, "token.txt"))
	db, err := store.Open(filepath.Join(dir, "sessions.json"))
	if err != nil {
		log.Fatalf("打开会话存储失败: %v", err)
	}
	db.DefaultCwd = defaultCwd()

	codexBin := codex.FindBinary()
	runner := codex.NewRunner(codexBin)
	fmt.Fprintf(out, "\n===== Codex 随行 %s =====\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(out, "codex 可执行文件: %s\n", codexBin)

	web, err := fs.Sub(webRoot, "web")
	if err != nil {
		log.Fatalf("读取内嵌界面失败: %v", err)
	}

	api := &httpapi.Server{
		DB:         db,
		Runner:     runner,
		App:        appserver.New(codexBin),
		Token:      token,
		Web:        web,
		DataDir:    dir,
		UpdateDir:  filepath.Join(dir, "update"),
		DefaultCwd: db.DefaultCwd,
		StartedAt:  time.Now(),
		Build:      buildStamp(),
	}
	_ = os.MkdirAll(api.UpdateDir, 0o700)

	// 桌面端在某条对话里发起的轮次，要能让手机也实时看到。
	// 这条链只在"线程号能对上手机会话"时才推，对不上的直接丢。
	api.App.SetObserver(api.OnThreadActivity)
	// 电脑端发起的那一轮要审批、要选择题时，手机也要收得到、答得了
	api.InitFallbackAsker()

	// ntfy 缓存：电脑一直开着，替手机把频道消息存下来（免费版只留 12 小时）
	// 「通知」默认就是开着的，指向预置频道；用户手动关掉后不会自己再打开。
	feed := notify.NewFeed(filepath.Join(dir, "notifications.json"), store.DefaultNtfyURL())
	api.Feed = feed
	// 任务播报：计划任务（每日文献收集、日报精读、概念提炼、周报、月报……）
	// 跑完会把成果写进知识库目录，这里替手机盯着那几个目录，
	// 一有新产出就写进「通知」（手机能弹系统通知）并推到 ntfy 频道。
	tasks := taskwatch.New(filepath.Join(dir, "task-watch.json"), taskWatchFromDB(db))
	tasks.Add = feed.AddLocal
	tasks.Push = func(title, body string) { pushNotify(db, feed, title, body) }
	api.Tasks = tasks
	// 电脑状态：常驻采一份 CPU / 内存 / 磁盘 / 网络，手机在「服务」里点开就能看
	api.Mon = sysmon.New()

	// 官方 app-server 改成"用到了才起"。
	//
	// 这样有个关键好处：桌面版 Codex 连上来的时候会把它自己那串启动参数
	// （插件开关、code_mode_host 之类）带过来，晚一点起就能用上同一套参数，
	// 桌面端看到的环境和以前直接拉起 codex 时一模一样。
	api.App.SetDefaultCwd(db.DefaultCwd)

	// 桌面端接入点。桌面版启动时会把我们当成 codex CLI 拉起来（靠 CODEX_CLI_PATH），
	// 它随后连到这里 —— 于是桌面端和手机共用同一个 app-server，
	// 那条"一条对话只能有一个写入者"的锁不再是问题。
	go func() {
		err := bridge.ServeAttach(sigCtx, dir, api.App, func(format string, a ...any) {
			fmt.Fprintf(out, format+"\n", a...)
		})
		if err != nil {
			fmt.Fprintf(out, "桌面端接入点启动失败: %v\n", err)
		}
	}()

	go feed.Run(sigCtx)
	go tasks.Run(sigCtx)
	go api.Mon.Run(sigCtx)
	// 发布了新版本就把更新内容写进「通知」并推出去
	go api.WatchUpdates(sigCtx)

	var (
		servers  []*http.Server
		shutOnce sync.Once
	)
	serve := func(ln net.Listener, label string, h2 bool) {
		srv := &http.Server{
			Handler: api,
			// Funnel 入口会不断被各种客户端探测握手，刷这些日志没意义。
			ErrorLog: log.New(handshakeFilter{out}, "", log.LstdFlags),
		}
		if h2 {
			// 这条 TLS 监听要支持 HTTP/2：手机那些请求全部挤在一条连接上，
			// 只付一次 Funnel 隧道里的握手（见 tsnode.ListenFunnelHTTP 的说明）。
			http2.ConfigureServer(srv, &http2.Server{})
		}
		servers = append(servers, srv)
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(out, "[%s] 服务结束: %v\n", label, err)
			}
		}()
	}

	localURL := ""
	if *localAddr != "" {
		if ln, err := net.Listen("tcp", *localAddr); err != nil {
			fmt.Fprintf(out, "本机与局域网入口监听失败（忽略）: %v\n", err)
		} else {
			serve(ln, "local", false)
			// 监听绑在 0.0.0.0 上（手机在同一个 WiFi 下要能直连），
			// 但给本机自己用的地址永远是 127.0.0.1，不然页面会去连 0.0.0.0。
			_, port, splitErr := net.SplitHostPort(*localAddr)
			if splitErr != nil || port == "" {
				port = "7799"
			}
			api.LANPort = port
			localURL = fmt.Sprintf("http://%s/?k=%s", net.JoinHostPort("127.0.0.1", port), token)
			fmt.Fprintf(out, "本机调试入口: %s\n", localURL)
		}
	}

	fmt.Fprintln(out, "正在启动 tsnet 节点 ...")
	node, err := tsnode.Start(sigCtx, filepath.Join(dir, "tsnet"), *hostname, out)
	if err != nil {
		log.Fatalf("tsnet 启动失败: %v", err)
	}
	// 内网服务一律走这个节点的网络栈：电脑本机没装 Tailscale 也能用。
	//
	// 这里必须显式打开 HTTP/2。tsnet 用的是自定义拨号器，而 Go 的
	// http.Transport 一旦看到自定义 DialContext 就会保守地把 HTTP/2 关掉；
	// tailnet 里的 HTTPS 服务通常是支持的。打开以后，一个页面几十个
	// 静态文件可以复用同一条连接并行取下来；关着的话每个文件都要重新握手，
	// 手机上就是盯着启动图等十几秒。
	api.TSClient = &http.Client{Transport: &http.Transport{
		DialContext:           node.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}

	domain := node.Domain()
	publicURL := ""
	lanURL := ""
	funnelReady := false
	if domain != "" {
		publicURL = fmt.Sprintf("https://%s/?k=%s", domain, token)
		lanURL = fmt.Sprintf("http://%s/?k=%s", domain, token)
		fmt.Fprintf(out, "节点名 : %s\n节点 IP: %v\n状态   : %s\n\n", domain, node.IPs(), node.BackendState())
	}

	if *tailnet {
		if ln, err := node.Listen("tcp", ":80"); err != nil {
			fmt.Fprintf(out, "tailnet 入口监听失败: %v\n", err)
		} else {
			serve(ln, "tailnet", false)
			fmt.Fprintf(out, "tailnet 入口（需装 Tailscale）: %s\n", lanURL)
		}
	}

	if *funnel && domain != "" {
		if ln, err := node.ListenFunnelHTTP("tcp", ":443"); err != nil {
			fmt.Fprintf(out, "Funnel 未开启（手机只能在 tailnet 内访问）: %v\n", err)
		} else {
			serve(ln, "funnel", true)
			funnelReady = true
			fmt.Fprintf(out, "公网固定入口（手机直接打开）  : %s\n", publicURL)
			go func() {
				if err := node.WarmCert(sigCtx); err != nil {
					fmt.Fprintf(out, "证书预热未完成（稍后会自动补上）: %v\n", err)
				} else {
					fmt.Fprintln(out, "HTTPS 证书已就绪")
				}
			}()
		}
	}
	entryURL := chooseEntryURL(publicURL, lanURL, funnelReady)

	writeEntryFile(dir, publicURL, lanURL, localURL, api.LANURLs())

	// 安装包二维码：手机相机扫一下直接在浏览器里下 APK。
	// App 内那条"检查更新"偶尔会被手机上的下载缓存搅乱，这个入口是兜底。
	if entryURL != "" {
		apkURL := strings.Replace(entryURL, "/?k=", "/apk?k=", 1)
		if png, err := qr.PNG(apkURL, 640); err != nil {
			fmt.Fprintf(out, "生成安装包二维码失败: %v\n", err)
		} else {
			api.APKPNG = png
			if err := qr.WriteFile(filepath.Join(dir, "安装包二维码.png"), apkURL, 640); err != nil {
				fmt.Fprintf(out, "安装包二维码落盘失败: %v\n", err)
			}
		}
	}

	// 二维码：一份落盘备用，一份放进内存由 /qr 页面直接显示。
	var qrErr error
	if entryURL != "" {
		var qrPNG []byte
		qrPNG, qrErr = qr.PNG(entryURL, 640)
		if qrErr != nil {
			fmt.Fprintf(out, "生成二维码失败: %v\n", qrErr)
		} else {
			api.QRPNG = qrPNG
			if err := qr.WriteFile(filepath.Join(dir, "二维码.png"), entryURL, 640); err != nil {
				fmt.Fprintf(out, "二维码落盘失败: %v\n", err)
			}
		}
	}

	// 配对二维码：手机 App 扫它就能自动拿到完整网址，不用手输。
	//
	// 注意这里带的是完整 URL（含口令），App 拿到直接加载即可。
	// 手机端不做任何原生网络组件，就是个浏览器壳，稳得多。
	if domain != "" && entryURL != "" {
		api.PairURI = fmt.Sprintf("codexmobile://pair?u=%s&h=%s&p=80&k=%s&n=%s",
			url.QueryEscape(entryURL), url.QueryEscape(domain),
			url.QueryEscape(token), url.QueryEscape(db.Machine))
		if png, err := qr.PNG(api.PairURI, 640); err != nil {
			fmt.Fprintf(out, "生成配对二维码失败: %v\n", err)
		} else {
			api.PairPNG = png
			if err := qr.WriteFile(filepath.Join(dir, "配对二维码.png"), api.PairURI, 640); err != nil {
				fmt.Fprintf(out, "配对二维码落盘失败: %v\n", err)
			}
		}
	}
	api.PublicURL, api.LANURL, api.LocalURL = entryURL, lanURL, localURL

	// 早先版本会在数据目录里生成一个 html 文件再双击打开，
	// 但这台机器上 .html 未必有默认打开方式，会弹"选择一个应用"的框。
	// 现在改成走 /qr 页面，旧文件顺手清掉。
	_ = os.Remove(filepath.Join(dir, "扫码打开.html"))

	qrPageURL := entryURL
	if localURL != "" {
		qrPageURL = strings.Replace(localURL, "/?", "/qr?", 1)
	}

	shutdown := func() {
		shutOnce.Do(func() {
			fmt.Fprintln(out, "正在退出 ...")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			for _, srv := range servers {
				_ = srv.Shutdown(ctx)
			}
			_ = api.App.Close()
			_ = node.Close()
		})
	}

	openPath := func(p string) {
		if p == "" {
			return
		}
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			_ = exec.Command("explorer", p).Start()
			return
		}
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", p).Start()
	}
	logPath := filepath.Join(dir, "日志.log")

	// 首次运行（还没有标记文件）时自动把扫码页面弹出来，省得找。
	firstRunMark := filepath.Join(dir, "已初始化")
	if _, err := os.Stat(firstRunMark); os.IsNotExist(err) {
		_ = os.WriteFile(firstRunMark, []byte(time.Now().Format(time.RFC3339)), 0o600)
		if qrErr == nil {
			go openPath(qrPageURL)
		}
	} else if *openQR && qrErr == nil {
		go openPath(qrPageURL)
	}

	fmt.Fprintln(out, "服务运行中")

	if *useTray {
		// 托盘用带圆角、外面透明的那张，通知区域里不会出现一个生硬的方块
		iconPNG, _ := fs.ReadFile(web, "tray.png")
		tray.Run(tray.ICOFromPNG(iconPNG, 192), "Codex 随行 · "+domain, autostart.Enabled(), tray.Handlers{
			OpenPage: func() { openPath(localURL) },
			ShowQR:   func() { openPath(qrPageURL) },
			CopyURL:  func() { copyToClipboard(entryURL) },
			Autostart: func(on bool) {
				if _, err := autostart.Toggle(on); err != nil {
					fmt.Fprintf(out, "设置开机自启失败: %v\n", err)
				} else {
					fmt.Fprintf(out, "开机自启已%s\n", map[bool]string{true: "开启", false: "关闭"}[on])
				}
			},
			DataDir: func() { openPath(dir) },
			LogFile: func() { openPath(logPath) },
		})
	} else {
		<-sigCtx.Done()
	}
	shutdown()
}

// ---------- 输出与日志 ----------

// taskWatchFromDB 把存档里的设置翻译成播报器的初始配置。
func taskWatchFromDB(db *store.DB) taskwatch.Config {
	cfg := db.TaskWatchConfig()
	return taskwatch.Config{On: cfg.On, Roots: cfg.Roots}
}

// pushNotify 把一条消息推给"用户配好的第三方渠道"，再单独往 ntfy 频道推一份。
//
// 为什么要单独推 ntfy：任务产出是用户明确要"在手机上看"的东西，
// 而他配的推送渠道可能是微信。两份都发，手机上随便用哪种方式都能收到。
// 同一地址只推一次，避免重复。
func pushNotify(db *store.DB, feed *notify.Feed, title, body string) {
	sent := ""
	if cfg := db.NotifyConfig(); notify.Resolve(cfg) != nil {
		if u, err := notify.Send(cfg, title, body); err != nil {
			fmt.Fprintf(out, "任务播报推送失败：%v\n", err)
		} else if u == 0 {
			fmt.Fprintln(out, "任务播报：第三方渠道没发出去")
		} else {
			sent = notify.Resolve(cfg).URL
		}
	}
	if feed == nil {
		return
	}
	topic, _ := feed.State()["url"].(string)
	if topic == "" || topic == sent {
		return
	}
	if _, err := notify.Send(store.Notify{Channel: "ntfy", Value: topic, OnDone: true}, title, body); err != nil {
		fmt.Fprintf(out, "任务播报推 ntfy 失败：%v\n", err)
	}
}

// fanout 同时写日志文件和控制台；控制台不存在时也不算失败。
type fanout struct {
	file    io.Writer
	console io.Writer
}

func (f fanout) Write(p []byte) (int, error) {
	if f.file != nil {
		_, _ = f.file.Write(p)
	}
	if f.console != nil {
		_, _ = f.console.Write(p)
	}
	return len(p), nil
}

func setupOutput(dir string) io.Writer {
	path := filepath.Join(dir, "日志.log")
	if info, err := os.Stat(path); err == nil && info.Size() > 4<<20 {
		// 简单滚动：超过 4MB 就重新开始。
		_ = os.Rename(path, path+".old")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return os.Stdout
	}
	return fanout{file: file, console: os.Stdout}
}

// handshakeFilter 丢掉 TLS 握手探测的噪音，保留其它错误。
type handshakeFilter struct{ w io.Writer }

func (f handshakeFilter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "TLS handshake error") {
		return len(p), nil
	}
	_, _ = f.w.Write(p)
	return len(p), nil
}

// attachParentConsole 让 GUI 子系统编译出来的程序，从终端启动时也能打印。
func attachParentConsole() {
	const attachParentProcess = 0xFFFFFFFF
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	if r, _, _ := kernel32.NewProc("AttachConsole").Call(uintptr(attachParentProcess)); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
}

// ---------- 杂项 ----------

// instanceLock 只要进程活着就一直占着这个端口，用来判断是否已有实例。
var instanceLock net.Listener

const instancePort = "127.0.0.1:7798"

func acquireInstance(dir string) bool {
	ln, err := net.Listen("tcp", instancePort)
	if err == nil {
		instanceLock = ln
		return true
	}
	// 端口被占：多半是已经有一个实例在跑。
	if raw, err := os.ReadFile(filepath.Join(dir, "入口.txt")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "本机:") {
				continue
			}
			u := strings.TrimSpace(strings.TrimPrefix(line, "本机:"))
			u = strings.Replace(u, "/?", "/qr?", 1)
			if u != "" {
				_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
			}
			return false
		}
	}
	return false
}

// buildStamp 用可执行文件本身算一个标识：换了新的 exe，这个值就变。
//
// 手机界面每次轮询 /api/status 都会带上它，一旦发现变了就自动重新加载页面。
// 这样"服务端换了新界面，手机还在跑旧页面"这类问题不会再出现，
// 也就不用再让用户去下拉刷新。
func buildStamp() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	info, err := os.Stat(self)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().Unix())
}

func defaultCwd() string {
	if v := os.Getenv("CODEX_DEFAULT_CWD"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}

// loadOrCreateToken 首次运行时生成访问口令，之后固定不变。
// 口令等同于门钥匙：即使流量只在 tailnet 内，也必须带对口令。
func loadOrCreateToken(path string) string {
	if raw, err := os.ReadFile(path); err == nil {
		if s := strings.TrimSpace(string(raw)); len(s) >= 16 {
			return s
		}
	}
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("生成访问口令失败: %v", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		log.Fatalf("保存访问口令失败: %v", err)
	}
	return token
}

func writeEntryFile(dir, publicURL, lanURL, localURL string, lanURLs []string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Codex 随行 · 访问入口\r\n")
	fmt.Fprintf(&b, "生成时间: %s\r\n\r\n", time.Now().Format("2006-01-02 15:04:05"))
	if publicURL != "" {
		fmt.Fprintf(&b, "公网（手机任意网络）: %s\r\n", publicURL)
		fmt.Fprintf(&b, "内网（需装 Tailscale）: %s\r\n", lanURL)
	}
	for _, u := range lanURLs {
		fmt.Fprintf(&b, "局域网直连（同一 WiFi）: %s\r\n", u)
	}
	if localURL != "" {
		fmt.Fprintf(&b, "本机: %s\r\n", localURL)
	}
	_ = os.WriteFile(filepath.Join(dir, "入口.txt"), []byte(b.String()), 0o600)
}

// chooseEntryURL 优先使用已成功监听的 Funnel 入口；否则回到 tailnet
// 内网入口。手机必须登录自己的 Tailscale 才能打开后者。
func chooseEntryURL(publicURL, lanURL string, funnelReady bool) string {
	if funnelReady && publicURL != "" {
		return publicURL
	}
	return lanURL
}

func copyToClipboard(text string) {
	if text == "" {
		return
	}
	cmd := exec.Command("cmd", "/c", "clip")
	cmd.Stdin = strings.NewReader(text)
	_ = cmd.Run()
}
