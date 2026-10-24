package httpapi

import (
	"net"
	"sort"
	"strings"
)

// LANURLs 是 lanURLs 的导出包装：main 里写「入口.txt」也要一份同样的清单。
func (s *Server) LANURLs() []string { return s.lanURLs() }

// lanURLs 返回这台电脑在局域网里的入口，形如
// ["http://100.81.133.27:7799/?k=xxx"]。
//
// 为什么每次请求现算、而不是启动时算一次：这个地址是路由器 DHCP 发的，
// 换网络、续租、插拔网线都会变。缓存住反而会把手机指到一个已经失效的地址，
// 那还不如不给 —— 手机探不通就会自己退回公网入口。
//
// 排序：名字像 WiFi / 以太网的排前面，Clash 的 TUN、WSL、Hyper-V 这些
// 虚拟网卡垫后面。手机是挨个试的，先试最可能通的那个。
func (s *Server) lanURLs() []string {
	if s.Token == "" || s.LANPort == "" {
		return nil
	}
	infos, err := net.Interfaces()
	if err != nil {
		return nil
	}
	type cand struct {
		url   string
		score int
	}
	var cands []cand
	for _, ifi := range infos {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(ifi.Name)
		// 内嵌的 tsnet 节点没有系统网卡，但系统自己装的 Tailscale 有。
		// 那条 100.x 走的是 tailnet，手机当普通局域网地址是连不上的。
		if strings.Contains(name, "tailscale") {
			continue
		}
		// Clash 的 TUN、WSL、Hyper-V 这些虚拟网卡，手机永远连不上。
		// 报给手机只会让它白白多等几次 1.2 秒的探测超时（实测手机上
		// 一次要试 3 个地址）。宁可漏报也不要误报 —— 漏了就会安静地
		// 走公网入口，误报就是实打实的启动变慢。
		if ifaceScore(name) < 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			cands = append(cands, cand{
				url:   "http://" + net.JoinHostPort(ip.String(), s.LANPort) + "/?k=" + s.Token,
				score: ifaceScore(name),
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.url)
	}
	return out
}

// ifaceScore 给网卡排个优先级。纯启发式：名字认错了也不致命，
// 手机那边是"挨个探，谁先通用谁"。
//
// 注意 "vEthernet" 里也含 "eth"，所以虚拟网卡必须排在以太网前面判，
// 否则 WSL / Hyper-V 的网卡会被当成真网卡排到前头。
func ifaceScore(name string) int {
	switch {
	case strings.Contains(name, "wlan"), strings.Contains(name, "wifi"),
		strings.Contains(name, "wi-fi"), strings.Contains(name, "wireless"):
		return 30
	case strings.Contains(name, "vethernet"), strings.Contains(name, "wsl"),
		strings.Contains(name, "hyper-v"), strings.Contains(name, "vgate"),
		strings.Contains(name, "clash"), strings.Contains(name, "tun"),
		strings.Contains(name, "tap"), strings.Contains(name, "bluetooth"):
		return -10
	case strings.Contains(name, "ethernet"), strings.Contains(name, "eth"):
		return 20
	default:
		return 0
	}
}
