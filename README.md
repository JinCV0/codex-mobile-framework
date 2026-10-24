# Codex 随行 Framework

把电脑上的 Codex 接到手机：电脑端运行一个常驻服务，手机端通过一个轻量 Android WebView 壳访问。

这是通用框架版，不包含任何个人服务入口、通知频道、Tailscale 身份、访问口令或签名密钥。

## 工作方式

```text
手机（官方 Tailscale App + 随行 App / 浏览器）
  │  WireGuard 加密的 tailnet
  ▼
http://<电脑节点>.<tailnet>.ts.net/?k=<访问口令>
  │
  ▼
codex-mobile.exe
  ├─ 内嵌 tsnet，登录电脑所有者的 Tailscale 账号
  ├─ 内置手机网页
  ├─ 官方 Codex app-server
  └─ 可选 Funnel 公网入口，默认关闭
```

电脑和手机必须登录到同一个人所属的同一个 tailnet。多个使用者各自部署时，每个人使用自己的 Tailscale 账号、Codex 账号和数据目录，互不共享会话。

## 功能

- 对话列表、历史记录、流式回复
- 手机端审批
- 项目目录切换
- 文件与图片查看
- 服务页，可手动添加自己的 tailnet / 局域网网址
- 扫码配对
- 可选的应用更新通道
- 可选的桌面版 Codex app-server 桥接

首次启动服务页为空，不预置任何第三方服务，也不预置 ntfy 或其他外部通知频道。

## 环境要求

- Windows 10/11 x64
- Go 1.27+，仅源码构建时需要
- 已安装并登录 Codex 桌面版或 Codex CLI
- Tailscale 账号
- Android 8.0+，并安装官方 Tailscale App
- iPhone 也可以使用浏览器访问，但 App 只提供 Android 版本

## 首次运行

1. 确认电脑上的 Codex 已登录，并且能够正常执行 `codex`。
2. 运行 `codex-mobile.exe`，或双击 `启动 Codex 手机端.bat`。
3. 首次启动时按提示在浏览器里登录电脑所有者的 Tailscale 账号。
4. 在手机上安装并登录官方 Tailscale App，确认手机和电脑在同一个 tailnet。
5. 在电脑托盘菜单里选择“显示二维码”。
6. 手机安装了随行 App 时，扫左侧配对码；只使用浏览器时，扫右侧网页码。
7. 发一条测试消息，检查流式回复和手机审批。

电脑启动后默认只提供 tailnet 内的 HTTP 入口：

```text
http://<节点名>.<tailnet>.ts.net/?k=<访问口令>
```

流量在 Tailscale 的 WireGuard 隧道内加密。Funnel 默认关闭；确实需要从没有 Tailscale 的设备访问时，可手动加 `-funnel`。

## 构建

### 电脑端

```powershell
cd codex-mobile
$env:GOPROXY='https://goproxy.cn,direct'
go test ./...
go build -ldflags "-H=windowsgui -s -w" -o codex-mobile.exe .
```

### Android 壳

先安装本地工具链：

```powershell
cd android
powershell -ExecutionPolicy Bypass -File .\setup-android.ps1
powershell -ExecutionPolicy Bypass -File .\setup-android.ps1 -Verify
```

为自己的构建创建签名密钥，并把路径与口令写入不入库的 `android/app/keystore.properties`。密钥和口令不要提交到 GitHub。

```powershell
cd android\app
gradle assembleRelease --no-daemon
```

没有签名配置时，`assembleRelease` 只会生成不可安装的未签名包；本地功能测试可以使用 `assembleDebug`。

## 数据目录

运行数据位于：

```text
%LOCALAPPDATA%\CodexMobile\
```

其中的下列内容属于本机身份或私密数据，不要提交、打包或转给别人：

```text
token.txt
tsnet\
sessions.json
二维码.png
配对二维码.png
入口.txt
日志.log
```

`token.txt` 等同于这台电脑的控制口令。电脑端数据目录整份复制只适用于同一台机器的备份或迁移，不属于通用部署包内容。

## 发布更新

`发布新版本.ps1` 会把已构建的 APK 放入本机更新目录，供已经配对的手机检查更新：

```powershell
powershell -ExecutionPolicy Bypass -File .\发布新版本.ps1 -Notes "release notes"
```

更新目录必须放在运行程序实际使用的数据目录下。通过 Codex 桌面版桥接启动时，Windows 可能把 `%LOCALAPPDATA%` 重定向到 MSIX 数据目录，脚本会把更新文件同步到现有 MSIX 数据目录。

## 公网入口

Funnel 是可选项，不是默认部署方式。开启前应确认：

- 访问口令不会泄露
- 电脑上的 Codex 权限符合预期
- Tailscale 账户、地域和带宽限制可以接受

更推荐只用 tailnet 内网入口，让手机通过自己的 Tailscale 账号访问。

## 目录

```text
codex-mobile/       Windows 常驻服务与内置网页
android/            Android WebView 壳
brand/              图标与品牌资源
发布新版本.ps1      发布 APK 到更新通道
重启电脑端.ps1      延迟换装与重启
桌面桥接.ps1        可选的 Codex 桌面版桥接
```

## License

MIT。第三方依赖说明见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
