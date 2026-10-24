# 独立部署

这套方案按“一人一套”部署。每个使用者都有自己的 codex-mobile 程序、Tailscale
账户、Codex 账户和数据目录。

## 1. 准备

- Windows 电脑一份
- 已安装并登录 Codex
- Tailscale 账户
- 手机安装官方 Tailscale App
- Android 手机安装随行 APK，或直接使用浏览器

## 2. 让电脑和手机进入同一个 tailnet

电脑端程序首次启动时会打开 Tailscale 登录页。用手机也要登录的同一个 Tailscale
账户完成登录。

手机打开官方 Tailscale App，确认 VPN 已连接，并能在设备列表中看到运行
`codex-mobile` 的电脑。

## 3. 启动电脑端

```powershell
.\codex-mobile.exe
```

默认参数：

```text
-funnel=false
-tailnet=true
-local=127.0.0.1:7799
```

首次登录和节点上线需要一点时间。托盘菜单可以查看二维码、复制访问地址和打开日志。

## 4. 配对手机

电脑托盘菜单 -> 显示二维码：

- 已安装随行 App：扫左侧配对码
- 只用浏览器：扫右侧网页码

也可手动打开：

```text
http://<节点名>.<tailnet>.ts.net/?k=<访问口令>
```

## 5. 验收

- 对话列表能读取
- 发送消息能看到流式回复
- Codex 需要审批时，手机能收到审批卡片
- 手机 Tailscale 断开后页面不可访问；重新连接后恢复
- 服务页为空，手动添加自己的网址后能打开

## 6. 更新

替换电脑端 exe 后重启服务。修改 Go 或网页代码通常不需要重装 APK；修改 Java、
Manifest 或 Android 资源时才需要重新构建 APK。

签名密钥由每个发布者自行保管。不要把密钥提交到仓库，也不要使用他人的签名密钥。
