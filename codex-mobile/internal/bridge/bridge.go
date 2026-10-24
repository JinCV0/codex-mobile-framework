// Package bridge 让桌面版 Codex 和手机端共用同一个 app-server。
//
// 背景：Codex 的"一条对话只能有一个写入者"是进程级锁。桌面版自己会拉起一个
// app-server，我们的服务也拉起一个，两边抢同一条对话，谁后到谁报
// "already has an active writer"，手机上就表现为"这条对话现在正在电脑上用"。
//
// 做法（思路来自 MIT 许可的 DusuWorks/codex-webui，我们在 Go 里重做了一遍，
// 并省掉了它的命名管道，因为服务端和 app-server 本来就在同一个进程里）：
//
//	把用户环境变量 CODEX_CLI_PATH 指向本程序
//	-> 桌面版启动时拉起的是我们
//	-> 我们作为代理连回正在跑的服务
//	-> 服务用桌面端原来的参数起唯一一个 app-server
//	-> 桌面端和手机同时用这一个进程
//
// 锁的问题因此从根上消失：只有一个进程加载这条对话。
//
// 安全阀：代理模式只要有任何一步不顺利，就回退成直接运行真正的 codex，
// 保证桌面上的 Codex 永远不会因为我们而打不开。
package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"codexmobile/internal/appserver"
	"codexmobile/internal/netenv"
)

// 数据目录里的三个小文件。
const (
	realCliFile = "desktop-bridge.json"
	attachFile  = "attach.json"
	logFile     = "桥接.log"
)

// 我们自己的命令行开关。看到这些就说明是用户或开机自启在启动服务，
// 不是桌面版把我们当成 codex。
var ourFlags = map[string]bool{
	"-hostname": true, "-funnel": true, "-tailnet": true, "-local": true,
	"-data": true, "-tray": true, "-qr": true, "-autostart": true,
	"-h": true, "-help": true, "--help": true,
}

// codex 的顶层子命令。出现在第一个位置，就说明是"以 CLI 的身份被调用"。
var codexCommands = map[string]bool{
	"exec": true, "e": true, "review": true, "login": true, "logout": true,
	"mcp": true, "plugin": true, "remote-control": true, "app": true,
	"completion": true, "update": true, "doctor": true, "sandbox": true,
	"debug": true, "apply": true, "a": true, "resume": true, "queue": true,
	"archive": true, "delete": true, "migrate-rollouts": true, "unarchive": true,
	"fork": true, "cloud": true, "exec-server": true, "features": true,
	"help": true, "agents": true,
}

// app-server 自己的子命令是工具性的，原样透传给真正的 codex。
var appServerSubcommands = map[string]bool{
	"daemon": true, "proxy": true, "generate-ts": true,
	"generate-json-schema": true, "help": true,
}

// DataDir 返回服务的数据目录，允许用环境变量覆盖（自测用）。
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("CODEX_MOBILE_DATA_DIR")); v != "" {
		return v
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "CodexMobile")
}

// realDataDir 把 MSIX 重定向过的 %LOCALAPPDATA% 还原成真实目录。
//
// 桌面版是打包应用，它拉起的代理看到的 %LOCALAPPDATA% 是
//
//	…\AppData\Local\Packages\OpenAI.Codex_<hash>\LocalCache\Local
//
// 而服务也可能是从真实目录起来的（开机自启那条路就是）。
func realDataDir() string {
	la := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	low := strings.ToLower(la)
	if i := strings.Index(low, `\appdata\local\packages\`); i > 0 {
		return filepath.Join(la[:i], "AppData", "Local", "CodexMobile")
	}
	return filepath.Join(la, "CodexMobile")
}

// candidateDirs 是放 attach.json 的候选目录，先看自己这侧，再看另一侧。
//
// 两边都找是必须的：服务在哪个目录取决于它是被谁拉起来的
// （桌面版拉起 → 重定向目录；开机自启 → 真实目录）。只认一个，
// 代理就会以为"服务没起来"，再拉起第二个服务去抢端口和数据目录。
func candidateDirs() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{DataDir(), realDataDir()} {
		if p == "" {
			continue
		}
		key := strings.ToLower(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// DesktopMode 判断这次启动到底是什么身份。
//
//	"proxy"       桌面版把我们当成 codex，参数里有 app-server
//	"passthrough" 桌面版在调别的 codex 子命令，原样转发即可
//	""            我们自己的服务端启动
func DesktopMode(args []string) string {
	if len(args) == 0 {
		return ""
	}
	arg0 := strings.ToLower(args[0])
	if idx := strings.Index(arg0, "="); idx != -1 {
		arg0 = arg0[:idx]
	}
	if ourFlags[arg0] {
		return ""
	}
	for i, a := range args {
		if a != "app-server" {
			continue
		}
		if i+1 < len(args) && appServerSubcommands[args[i+1]] {
			return "passthrough"
		}
		return "proxy"
	}
	if codexCommands[args[0]] || strings.HasPrefix(args[0], "-") {
		return "passthrough"
	}
	return ""
}

// realCliConfig 是安装脚本写下来的记录。
type realCliConfig struct {
	RealCli         string `json:"realCli"`
	PreviousCliPath string `json:"previousCliPath"`
}

// RealCli 找出真正该跑的 codex 可执行文件。
//
// 优先用安装时记下的路径（最准），退化到扫 %LOCALAPPDATA%\OpenAI\Codex\bin\*。
// 刻意不用 PATH：PATH 上的 codex 很可能就是桌面版的 AppX 别名，
// 那样会绕回桌面应用自己，形成死循环。
func RealCli(dir string) string {
	self, _ := os.Executable()
	var cfg realCliConfig
	if raw, err := os.ReadFile(filepath.Join(dir, realCliFile)); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	if p := validCli(cfg.RealCli, self); p != "" {
		return p
	}
	root := filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin")
	matches, _ := filepath.Glob(filepath.Join(root, "*", "codex.exe"))
	sort.Slice(matches, func(i, j int) bool {
		fi, err1 := os.Stat(matches[i])
		fj, err2 := os.Stat(matches[j])
		if err1 != nil || err2 != nil {
			return matches[i] > matches[j]
		}
		return fi.ModTime().After(fj.ModTime())
	})
	for _, m := range matches {
		if p := validCli(m, self); p != "" {
			return p
		}
	}
	return ""
}

func validCli(path, self string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	if self != "" && strings.EqualFold(filepath.Clean(path), filepath.Clean(self)) {
		return ""
	}
	return path
}

// attachInfo 是服务端写下来、代理用来连回来的信息。
type attachInfo struct {
	Port   int    `json:"port"`
	Secret string `json:"secret"`
}

// hello 是代理连上来发的第一行。
type hello struct {
	Type   string   `json:"type"`
	Secret string   `json:"secret"`
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
}

type helloReply struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	Err  string `json:"error,omitempty"`
}

// RunDesktop 是"被桌面版当成 codex 拉起"时的入口，返回值就是进程退出码。
func RunDesktop(mode string, args []string) int {
	dir := DataDir()
	_ = os.MkdirAll(dir, 0o700)
	logPath := filepath.Join(dir, logFile)
	logf := func(format string, a ...any) {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"),
			fmt.Sprintf(format, a...))
	}
	logf("桥接启动 mode=%s args=%q", mode, args)

	real := RealCli(dir)
	if real == "" {
		logf("找不到真正的 codex，原样交给系统处理")
		return runReal("", args, logf)
	}
	logf("真正的 codex: %s", real)

	if mode == "passthrough" {
		return runReal(real, args, logf)
	}
	if err := proxyToService(dir, args, logf); err == nil {
		logf("桌面端连接正常结束")
		return 0
	} else {
		logf("接回服务失败（%v），回退成直接运行 codex", err)
	}
	return runReal(real, args, logf)
}

// runReal 原样运行真正的 codex，stdin/stdout/stderr 全部照搬。
// 这是安全阀：桥接没弄好也不会让桌面版用不了。
func runReal(real string, args []string, logf func(string, ...any)) int {
	if real == "" {
		logf("没有可用的 codex 可执行文件")
		return 127
	}
	cmd := exec.Command(real, args...)
	cmd.Env = netenv.WithoutProxy()
	// 桌面版把 CODEX_CLI_PATH 指向了我们自己；真正要跑的是 codex，
	// 所以这里必须把它纠正回去，否则 codex 内部再拉进程会绕回本程序。
	cmd.Env = netenv.Set(cmd.Env, "CODEX_CLI_PATH", real)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		logf("运行 codex 失败: %v", err)
		return 1
	}
	return 0
}

// proxyToService 连回服务，把 stdio 交给它。服务没在跑就顺手拉起来。
func proxyToService(dir string, args []string, logf func(string, ...any)) error {
	if err := attachOnce(dir, args, logf); err == nil {
		return nil
	} else {
		logf("直连服务失败（%v），尝试启动服务", err)
	}
	if err := startService(logf); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		time.Sleep(time.Second)
		if lastErr = attachOnce(dir, args, logf); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func attachOnce(dir string, args []string, logf func(string, ...any)) error {
	// 服务和代理可能各自活在真实目录 / MSIX 重定向目录里，两边都试。
	dirs := candidateDirs()
	var lastErr error
	for _, d := range dirs {
		if err := attachFromDir(d, args, logf); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func attachFromDir(dir string, args []string, logf func(string, ...any)) error {
	raw, err := os.ReadFile(filepath.Join(dir, attachFile))
	if err != nil {
		return err
	}
	var info attachInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return err
	}
	if info.Port == 0 || info.Secret == "" {
		return errors.New("接入信息不完整")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", info.Port), 3*time.Second)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	// 诊断：桌面端给 CLI 注入的环境里到底有没有代理变量。本机 chatgpt.com /
	// api.openai.com 直连都不通，只有经代理才行；如果这份环境里没有代理，
	// 那"组织设置加载失败"就得从别处找原因。
	env := os.Environ()
	var prox []string
	for _, kv := range env {
		lk := strings.ToLower(kv)
		for _, p := range []string{"http_proxy=", "https_proxy=", "all_proxy=", "no_proxy=", "proxy_server="} {
			if strings.HasPrefix(lk, p) {
				prox = append(prox, kv)
				break
			}
		}
	}
	logf("桌面端环境：共 %d 项，代理相关 %v", len(env), prox)
	line, _ := json.Marshal(hello{
		Type: "hello", Secret: info.Secret, Args: args,
		// 桌面版为它拉起的 CLI 注入了一批环境变量（沙箱、git、MCP node 路径，
		// 以及它自己那份代理设置…），子进程由服务端起，所以要把这份环境
		// **原样**带过去：服务端那边还会再做一次清洗判断，这里保真即可。
		Env: env,
		Cwd: cwd,
	})
	line = append(line, '\n')
	if _, err := conn.Write(line); err != nil {
		_ = conn.Close()
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)
	reply, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return err
	}
	var rep helloReply
	if err := json.Unmarshal(reply, &rep); err != nil {
		_ = conn.Close()
		return err
	}
	if !rep.OK {
		_ = conn.Close()
		return fmt.Errorf("服务拒绝接入: %s", rep.Err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	logf("已接上服务，桌面端与手机共用同一个 app-server")

	// 之后这条连接就是纯字节通道：一头是桌面版的 stdio，一头是服务。
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(conn, os.Stdin)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(os.Stdout, br)
		_ = conn.Close()
		done <- struct{}{}
	}()
	<-done
	// 一个方向结束就把另一侧也拆掉：对端断开时，卡在 os.Stdin 上的 Read
	// 不会自己返回，不主动关就会一直挂着，进程留着不放（2026-10-10 见到的
	// 桥接进程堆积就是这么来的）。关掉之后另一个 goroutine 立刻返回。
	_ = conn.Close()
	_ = os.Stdin.Close()
	<-done
	return nil
}

// startService 把常驻服务拉起来（不带托盘、不弹二维码）。
func startService(logf func(string, ...any)) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "-tray=false", "-qr=false")
	cmd.Env = netenv.WithoutProxy()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	logf("已拉起服务进程 pid=%d", cmd.Process.Pid)
	_ = cmd.Process.Release()
	return nil
}

// ServeAttach 在服务端跑：接住桌面端的连接，交给 app。
func ServeAttach(ctx context.Context, dir string, app *appserver.Client, logf func(string, ...any)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := 0
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		port = tcp.Port
	}
	secret := randomSecret()
	raw, _ := json.Marshal(attachInfo{Port: port, Secret: secret})
	if err := os.WriteFile(filepath.Join(dir, attachFile), raw, 0o600); err != nil {
		_ = ln.Close()
		return err
	}
	logf("桌面端接入点: %s", ln.Addr())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return err
		}
		go handleAttach(ctx, conn, secret, app, logf)
	}
}

func handleAttach(ctx context.Context, conn net.Conn, secret string, app *appserver.Client, logf func(string, ...any)) {
	defer func() {
		if r := recover(); r != nil {
			logf("接入处理异常: %v", r)
		}
	}()
	fail := func(msg string) {
		reply, _ := json.Marshal(helloReply{Type: "hello", OK: false, Err: msg})
		_, _ = conn.Write(append(reply, '\n'))
		_ = conn.Close()
	}

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(conn, 64*1024)
	line, err := br.ReadBytes('\n')
	if err != nil {
		_ = conn.Close()
		return
	}
	var h hello
	if err := json.Unmarshal(line, &h); err != nil || h.Type != "hello" || h.Secret != secret {
		fail("口令不对")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	// 用桌面端原来的参数和环境把子进程拉起来（或换掉旧的那条）。
	if len(h.Args) > 0 || len(h.Env) > 0 {
		app.SetLaunchContext(ctx, h.Args, h.Env, h.Cwd)
	}
	if err := app.Ensure(ctx, ""); err != nil {
		fail(err.Error())
		return
	}
	reply, _ := json.Marshal(helloReply{Type: "hello", OK: true})
	if _, err := conn.Write(append(reply, '\n')); err != nil {
		_ = conn.Close()
		return
	}
	logf("桌面版已接入，开始与手机共用同一个 app-server")

	started := time.Now()
	// 双向记录报文是排查"接上就断"这类问题最有效的手段，但报文里可能带
	// 文件内容（base64），不适合常开。需要时设 CODEX_MOBILE_IO_DEBUG=1 打开。
	var tunnel io.ReadWriteCloser = &bufferedConn{Conn: conn, r: br}
	if os.Getenv("CODEX_MOBILE_IO_DEBUG") != "" {
		tunnel = &teeConn{Conn: conn, r: br, logf: logf}
		logf("IO 调试已开启（CODEX_MOBILE_IO_DEBUG）")
	}
	err = app.Attach(tunnel)
	logf("桌面版连接已断开（持续 %s，err=%v）", time.Since(started).Round(time.Millisecond), err)
}

// teeConn 在桌面端连接上做一层双向记录。
type teeConn struct {
	net.Conn
	r     *bufio.Reader
	logf  func(string, ...any)
	mu    sync.Mutex
	seen  int
	limit int
}

func (t *teeConn) record(dir string, p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.limit == 0 {
		t.limit = 40
	}
	if t.seen >= t.limit {
		return
	}
	t.seen++
	s := string(p)
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	t.logf("IO %s %s", dir, s)
}

func (t *teeConn) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.record("<<", p[:n])
	}
	if err != nil {
		t.logf("IO << 读结束: %v", err)
	}
	return n, err
}

func (t *teeConn) Write(p []byte) (int, error) {
	t.record(">>", p)
	return t.Conn.Write(p)
}

// bufferedConn 把已经读进 bufio 的那一段接回连接，
// 免得握手之后紧跟的 JSON-RPC 数据被丢掉。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func randomSecret() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
