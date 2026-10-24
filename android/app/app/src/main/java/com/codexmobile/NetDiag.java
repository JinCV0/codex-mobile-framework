package com.codexmobile;

import android.content.Context;
import android.content.SharedPreferences;
import android.net.ConnectivityManager;
import android.net.LinkAddress;
import android.net.LinkProperties;
import android.net.Network;
import android.net.NetworkCapabilities;

import java.net.Inet4Address;
import java.net.Inet6Address;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.Socket;
import java.net.URI;
import java.util.ArrayList;
import java.util.List;

/**
 * 网络现场取证：出问题时把"当时挂在什么网、解析到哪里、哪条路走不通"记下来。
 *
 * 为什么要有它：2026-10-11 查"手机用 WiFi 连不上、切蜂窝就好了"这个毛病时，
 * 日志里只有干巴巴一句 net::ERR_SOCKET_NOT_CONNECTED，完全看不出当时是 WiFi 还是
 * 蜂窝、域名解析成了哪几个地址、IPv4 和 IPv6 里哪条断。光凭那句话只能猜。
 *
 * 所以这里补齐三段，一次诊断写几行日志：
 *   1. 网络类型 / VPN / 本机 v4 v6 / 接口名 / 系统 DNS —— 分清是哪张网
 *   2. 服务器域名解析成**全部** A + AAAA 记录并记耗时 —— 看 DNS 有没有被动手脚
 *   3. 对解析出的每个地址单独做 TCP 连接测试 —— IPv6 黑洞和地址段被墙一眼区分
 *
 * 全部在后台线程跑，探测总耗时上限 4 秒 × 地址数，绝不能卡住界面。
 */
public final class NetDiag {

    /** 单个地址的 TCP 探测超时。太短会把慢链路误判成不通，太长会把诊断拖成十几秒。 */
    private static final int PROBE_TIMEOUT_MS = 4000;

    private NetDiag() {}

    /**
     * 诊断入口：后台跑一次完整取证，结果写进日志。
     * 调用方不用管线程，本方法立刻返回。
     */
    public static void log(Context ctx, String reason) {
        final Context app = ctx == null ? null : ctx.getApplicationContext();
        if (app == null) return;
        new Thread(() -> {
            try {
                DebugLog.add("net", "[" + reason + "] " + snapshot(app));
                String server = serverBase(app);
                if (server == null || server.isEmpty()) {
                    DebugLog.add("net", "  还没配对过服务器地址，跳过探测");
                    return;
                }
                for (String line : probe(server)) DebugLog.add("net", "  " + line);
            } catch (Throwable t) {
                DebugLog.add("net", "  诊断本身出错：" + t);
            }
        }, "netdiag").start();
    }

    /** 当前网络摘要。同步执行，很快（不联网）。 */
    public static String snapshot(Context ctx) {
        StringBuilder sb = new StringBuilder();
        try {
            ConnectivityManager cm = (ConnectivityManager) ctx.getSystemService(Context.CONNECTIVITY_SERVICE);
            if (cm == null) return "type=unknown（取不到 ConnectivityManager）";
            Network net = cm.getActiveNetwork();
            NetworkCapabilities cap = net == null ? null : cm.getNetworkCapabilities(net);
            sb.append("type=").append(kind(cap));
            sb.append(" vpn=").append(cap != null && cap.hasTransport(NetworkCapabilities.TRANSPORT_VPN));
            sb.append(" validated=").append(cap != null
                    && cap.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED));
            if (cap != null) {
                sb.append(" downKbps=").append(cap.getLinkDownstreamBandwidthKbps());
            }
            LinkProperties lp = net == null ? null : cm.getLinkProperties(net);
            if (lp != null) {
                List<String> v4 = new ArrayList<>();
                List<String> v6 = new ArrayList<>();
                for (LinkAddress la : lp.getLinkAddresses()) {
                    InetAddress a = la.getAddress();
                    if (a == null || a.isLoopbackAddress() || a.isLinkLocalAddress()
                            || a.isAnyLocalAddress()) {
                        continue;
                    }
                    if (a instanceof Inet4Address) v4.add(a.getHostAddress());
                    else if (a instanceof Inet6Address) v6.add(a.getHostAddress());
                }
                sb.append(" ip4=").append(v4.isEmpty() ? "-" : String.join(",", v4));
                sb.append(" ip6=").append(v6.isEmpty() ? "-" : String.join(",", v6));
                sb.append(" if=").append(lp.getInterfaceName());
                sb.append(" dns=").append(joinAddresses(lp.getDnsServers()));
            }
            sb.append(" server=").append(serverBase(ctx));
        } catch (Throwable t) {
            sb.append(" snapshot失败=").append(t);
        }
        return sb.toString();
    }

    /** 解析服务器域名，再对每个地址单独测 TCP。返回若干行，由调用方落日志。 */
    static List<String> probe(String serverBase) {
        List<String> out = new ArrayList<>();
        String host = hostOf(serverBase);
        int port = portOf(serverBase);
        if (host.isEmpty()) {
            out.add("服务器地址里没有主机名：" + serverBase);
            return out;
        }

        InetAddress[] addrs;
        long t0 = System.currentTimeMillis();
        try {
            addrs = InetAddress.getAllByName(host);
        } catch (Throwable t) {
            out.add("解析 " + host + " 失败，耗时 " + (System.currentTimeMillis() - t0)
                    + "ms：" + t);
            return out;
        }
        long dnsMs = System.currentTimeMillis() - t0;
        StringBuilder list = new StringBuilder();
        for (InetAddress a : addrs) {
            if (list.length() > 0) list.append(' ');
            list.append(bracket(a));
        }
        out.add("解析 " + host + " " + dnsMs + "ms -> " + list);
        for (InetAddress a : addrs) out.add(tcpProbe(a, port));
        return out;
    }

    /** 对一个地址测一次 TCP 连接。返回一行人话。 */
    private static String tcpProbe(InetAddress addr, int port) {
        Socket s = new Socket();
        long t0 = System.currentTimeMillis();
        try {
            s.connect(new InetSocketAddress(addr, port), PROBE_TIMEOUT_MS);
            return "tcp " + bracket(addr) + ":" + port + " 通 " + (System.currentTimeMillis() - t0) + "ms";
        } catch (Throwable t) {
            return "tcp " + bracket(addr) + ":" + port + " 不通 "
                    + (System.currentTimeMillis() - t0) + "ms " + t.getClass().getSimpleName();
        } finally {
            try {
                s.close();
            } catch (Throwable ignored) {
            }
        }
    }

    /** 配对时存下来的服务器地址，形如 https://xxx.ts.net 。 */
    static String serverBase(Context ctx) {
        try {
            SharedPreferences p = ctx.getSharedPreferences(App.PREFS, Context.MODE_PRIVATE);
            String s = p.getString("serverBase", "");
            return s == null ? "" : s;
        } catch (Throwable t) {
            return "";
        }
    }

    /** 当前是不是连着 WiFi。局域网直连只在 WiFi 下值得试。 */
    public static boolean onWifi(Context ctx) {
        try {
            ConnectivityManager cm = (ConnectivityManager) ctx.getSystemService(Context.CONNECTIVITY_SERVICE);
            if (cm == null) return false;
            Network net = cm.getActiveNetwork();
            NetworkCapabilities cap = net == null ? null : cm.getNetworkCapabilities(net);
            return cap != null && cap.hasTransport(NetworkCapabilities.TRANSPORT_WIFI);
        } catch (Throwable t) {
            return false;
        }
    }

    /**
     * 快速判断一个入口通不通：只做一次 TCP 连接，超时就算不通。
     * 用来在"局域网直连"和"公网入口"之间选路 —— 说通不算，连得上才算。
     */
    public static boolean reachable(String url, int timeoutMs) {
        Socket s = new Socket();
        try {
            URI u = URI.create(url);
            String host = u.getHost();
            if (host == null || host.isEmpty()) return false;
            int port = u.getPort() > 0 ? u.getPort() : ("http".equalsIgnoreCase(u.getScheme()) ? 80 : 443);
            s.connect(new InetSocketAddress(host, port), timeoutMs);
            return true;
        } catch (Throwable t) {
            return false;
        } finally {
            try {
                s.close();
            } catch (Throwable ignored) {
            }
        }
    }

    /** 传输类型。先看 WiFi/蜂窝/以太网，VPN 单独用一个字段表示（它会叠在底层传输上）。 */
    private static String kind(NetworkCapabilities cap) {
        if (cap == null) return "NONE";
        if (cap.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) return "WIFI";
        if (cap.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) return "CELLULAR";
        if (cap.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) return "ETHERNET";
        if (cap.hasTransport(NetworkCapabilities.TRANSPORT_BLUETOOTH)) return "BLUETOOTH";
        if (cap.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) return "VPN";
        return "OTHER";
    }

    private static String joinAddresses(List<InetAddress> list) {
        if (list == null || list.isEmpty()) return "-";
        StringBuilder sb = new StringBuilder();
        for (InetAddress a : list) {
            if (sb.length() > 0) sb.append(',');
            sb.append(bracket(a));
        }
        return sb.toString();
    }

    /** IPv6 加方括号显示，跟 URL 里的写法一致；顺手去掉 %wlan0 这种 scope 后缀。 */
    private static String bracket(InetAddress a) {
        String h = a.getHostAddress();
        if (a instanceof Inet6Address && h.indexOf(':') >= 0) {
            int pct = h.indexOf('%');
            return "[" + (pct >= 0 ? h.substring(0, pct) : h) + "]";
        }
        return h;
    }

    private static String hostOf(String base) {
        try {
            URI u = URI.create(base.contains("://") ? base : "https://" + base);
            String h = u.getHost();
            return h == null ? "" : h;
        } catch (Throwable t) {
            return "";
        }
    }

    private static int portOf(String base) {
        try {
            URI u = URI.create(base.contains("://") ? base : "https://" + base);
            if (u.getPort() > 0) return u.getPort();
            return "http".equalsIgnoreCase(u.getScheme()) ? 80 : 443;
        } catch (Throwable t) {
            return 443;
        }
    }
}
