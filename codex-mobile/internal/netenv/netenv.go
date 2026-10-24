// Package netenv 让电脑端服务彻底绕开系统代理。
//
// 为什么需要它：这台电脑上装着 Clash（系统代理 127.0.0.1:7877，还有一个 TUN
// 网卡）。Codex 的 CLI 会读系统代理设置，于是它发出的每一个请求都被塞进 Clash，
// 再由 Clash 决定走直连还是走境外。可我们这台机器本身已经接了 Tailscale，
// 该直连的直连、该走 tailnet 的走 tailnet，中间再夹一层代理只会变慢、变不确定。
//
// 所以：本进程和它拉起的所有子进程，一律拿到一份"没有代理"的环境变量。
// 实测（2026-10-04）：改过环境之后，codex 子进程不再连 127.0.0.1:7877，
// 而模型接口 api.deepseek.com 直连握手 0.33 秒，正常。
package netenv

import (
	"os"
	"strings"
)

// keys 是需要从环境里剔除的代理相关变量（大小写都要，Windows 上不区分大小写）。
var keys = map[string]bool{
	"http_proxy":        true,
	"https_proxy":       true,
	"all_proxy":         true,
	"ftp_proxy":         true,
	"rsync_proxy":       true,
	"git_proxy_command": true,
}

// WithoutProxy 基于当前进程环境，返回一份"任何地址都不走代理"的副本。
// 子进程用它当 cmd.Env。
func WithoutProxy() []string {
	return filter(os.Environ())
}

// Clean 把一份外部传进来的环境变量（比如桌面版代理那边带过来的）收拾干净：
// 去掉代理相关项，补上 NO_PROXY。传 nil 表示用当前进程的环境。
func Clean(environ []string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	return filter(environ)
}

// Strip 就地把当前进程的环境变量改干净。进程启动时调用一次，
// 之后 os.Environ() 拉出来的就已经是没有代理的版本了。
func Strip() {
	environ := filter(os.Environ())
	os.Clearenv()
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			_ = os.Setenv(kv[:i], kv[i+1:])
		}
	}
}

func filter(environ []string) []string {
	out := make([]string, 0, len(environ)+2)
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if keys[strings.ToLower(kv[:i])] {
			continue
		}
		out = append(out, kv)
	}
	// NO_PROXY=* 是各语言里最通用的"全部直连"写法：
	// Go 的 net/http、Node、Python requests、Rust reqwest 都认。
	out = append(out, "NO_PROXY=*", "no_proxy=*")
	return out
}

// Set 在环境副本里覆盖某个变量（Windows 上环境变量不分大小写，
// 直接 append 会留下两份，子进程取哪份看运气，所以要按名字替换）。
func Set(environ []string, key, value string) []string {
	out := make([]string, 0, len(environ)+1)
	replaced := false
	for _, kv := range environ {
		i := strings.IndexByte(kv, '=')
		if i > 0 && strings.EqualFold(kv[:i], key) {
			if !replaced {
				out = append(out, key+"="+value)
				replaced = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return out
}
