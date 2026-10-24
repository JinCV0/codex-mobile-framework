// Package tsnode 封装内嵌的 Tailscale 用户态节点（tsnet）。
//
// 用它的意义：电脑端不需要安装 Tailscale、不需要管理员权限，
// 而网络身份会持久化在本地目录里，重启后依然是同一个节点、同一个地址。
package tsnode

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Node 是一个已经上线（或正在等待登录）的 tsnet 节点。
type Node struct {
	s      *tsnet.Server
	status *ipnstate.Status
	login  string
}

// Start 启动节点。如果返回的节点带登录链接，调用方应引导用户点一次。
func Start(ctx context.Context, stateDir, hostname string, out io.Writer) (*Node, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建节点状态目录失败: %w", err)
	}
	if out == nil {
		out = io.Discard
	}

	s := &tsnet.Server{
		Hostname: hostname,
		Dir:      stateDir,
		Logf: func(format string, args ...any) {
			line := fmt.Sprintf(format, args...)
			low := strings.ToLower(line)
			if strings.Contains(low, "auth") || strings.Contains(low, "error") || strings.Contains(low, "funnel") {
				fmt.Fprintf(out, "[tsnet] %s\n", line)
			}
		},
	}

	st, err := s.Up(ctx)
	if err != nil {
		_ = s.Close()
		return nil, err
	}

	n := &Node{s: s, status: st}
	if st.AuthURL != "" {
		n.announceAuth(st.AuthURL, stateDir, out)
	}
	if st.BackendState != "Running" {
		if err := n.waitRunning(ctx, stateDir, out); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return n, nil
}

// waitRunning 轮询节点状态，直到变成 Running。
// 首次登录可能拖很久（用户要去浏览器点一下），所以给足 20 分钟。
func (n *Node) waitRunning(ctx context.Context, stateDir string, out io.Writer) error {
	lc, err := n.s.LocalClient()
	if err != nil {
		return fmt.Errorf("获取本地客户端失败: %w", err)
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for i := 0; i < 400; i++ {
		cur, err := lc.Status(ctx)
		if err == nil {
			if cur.BackendState == "Running" {
				n.status = cur
				return nil
			}
			// 状态里可能出现新的登录链接，及时提示用户。
			if cur.AuthURL != "" && cur.AuthURL != n.login {
				n.announceAuth(cur.AuthURL, stateDir, out)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return fmt.Errorf("等待节点上线超时，请重新运行程序")
}

func (n *Node) announceAuth(url, stateDir string, out io.Writer) {
	n.login = url
	fmt.Fprintf(out, "\n================ 需要登录 ================\n%s\n", url)
	fmt.Fprintln(out, "在浏览器里打开上面的链接并登录，登录一次以后就不用再登了。")
	fmt.Fprintln(out, "=========================================")
	go func() {
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}()
	_ = os.WriteFile(filepath.Join(filepath.Dir(stateDir), "登录链接.txt"), []byte(url), 0o600)
}

// LoginURL 返回待完成的登录链接，已登录或已完成后为空。
func (n *Node) LoginURL() string { return n.login }

// Domain 返回形如 codex-mobile.tailxxxx.ts.net 的固定域名。
func (n *Node) Domain() string {
	if n.status == nil || n.status.Self == nil {
		return ""
	}
	return strings.TrimSuffix(n.status.Self.DNSName, ".")
}

// IPs 返回节点在 tailnet 里的地址。
func (n *Node) IPs() []string {
	if n.status == nil {
		return nil
	}
	out := make([]string, 0, len(n.status.TailscaleIPs))
	for _, ip := range n.status.TailscaleIPs {
		out = append(out, ip.String())
	}
	return out
}

// BackendState 返回节点状态，正常时为 Running。
func (n *Node) BackendState() string {
	if n.status == nil {
		return "unknown"
	}
	return n.status.BackendState
}

// Listen 在 tailnet 内监听端口：只有加入同一 tailnet 的设备能访问。
func (n *Node) Listen(network, addr string) (net.Listener, error) {
	return n.s.Listen(network, addr)
}

// ListenFunnel 监听公网入口：互联网上任何设备都能访问，地址永久固定。
// 需要 tailnet 策略里给该节点开启 Funnel。
func (n *Node) ListenFunnel(network, addr string) (net.Listener, error) {
	return n.s.ListenFunnel(network, addr)
}

// ListenFunnelHTTP 和 ListenFunnel 一样，但把 ALPN 打开（h2 + http/1.1）。
//
// 为什么必须自己递一份 tls.Config：tsnet 默认那份没有 NextProtos，
// 实测 ALPN 协商不到任何协议，只能退回 HTTP/1.1。而 Funnel 的 TLS 是在
// 本机节点上终止的，握手的每个往返都要穿过那条隧道（实测握手段 0.9~1.1 秒，
// 而一次请求往返才 0.38 秒），HTTP/1.1 下一个域名最多开 6 条连接，等于把
// 这 1 秒付好几遍。开了 h2 就只用付一次，其余请求全部多路复用。
func (n *Node) ListenFunnelHTTP(network, addr string) (net.Listener, error) {
	lc, err := n.s.LocalClient()
	if err != nil {
		return nil, err
	}
	return n.s.ListenFunnel(network, addr, tsnet.FunnelTLSConfig(&tls.Config{
		GetCertificate: lc.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}))
}

// WarmCert 提前申请 HTTPS 证书。证书签发要 1～2 分钟，
// 在这之前访问域名会 TLS 握手失败，属于正常空窗期，不是故障。
func (n *Node) WarmCert(ctx context.Context) error {
	domain := n.Domain()
	if domain == "" {
		return fmt.Errorf("节点尚未就绪")
	}
	lc, err := n.s.LocalClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, _, err = lc.CertPair(ctx, domain)
	return err
}

// HTTPClient 返回走该节点网络栈的 HTTP 客户端，用于自测。
func (n *Node) HTTPClient() *http.Client { return n.s.HTTPClient() }

// DialContext 走这个节点的网络栈拨一个 TCP 连接。
// 访问同一 tailnet 里的其它机器（读取服务状态、反向代理）都该用它，
// 这样电脑本机装没装 Tailscale 都不影响。
func (n *Node) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return n.s.Dial(ctx, network, addr)
}

// Close 关闭节点。节点身份保存在磁盘上，下次启动仍是同一个。
func (n *Node) Close() error { return n.s.Close() }
