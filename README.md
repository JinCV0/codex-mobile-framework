# Codex 随行

**在手机上继续使用、查看和监督运行于个人电脑上的 Codex。**

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
![Platform: Windows](https://img.shields.io/badge/platform-Windows%2010%2F11-0078D4)
![Android: 8.0+](https://img.shields.io/badge/Android-8.0%2B-3DDC84)

Codex 随行是一套自托管远程访问方案，由 Windows 常驻服务和 Android
WebView 客户端组成。它不把项目搬到云端，也不托管用户的 Codex 账号；手机连接的是
使用者自己的电脑、自己的 Codex 环境和自己的项目文件。

> 本项目是非官方项目，与 OpenAI、Tailscale 均无隶属关系。

## 项目目的

Codex 在个人电脑上执行任务时，经常会遇到以下情况：

- 任务需要运行较长时间，用户不可能一直守在电脑前；
- Codex 请求提升权限、执行命令或修改工作区之外的代码，需要及时审批；
- 用户离开桌面后，仍想查看生成过程、结果和改动文件；
- 项目目录、日志和图片都保存在电脑上，不适合直接暴露为公网服务。

Codex 随行的目标是补齐这一层移动访问能力：

1. 在手机上查看和继续电脑上的 Codex 对话；
2. 在手机上处理审批和选择；
3. 查看项目目录、文件和图片；
4. 仍然以个人电脑为唯一的执行环境；
5. 默认只在个人 Tailscale 网络内访问。

## 项目不是什么

- 不是云端 IDE，不提供远程计算资源。
- 不是多租户平台。一套部署对应一个电脑使用者。
- 不是通用远程桌面，不提供任意 Windows 桌面控制。
- 不是 Codex 账号共享工具。每个使用者需要自己的 Codex 账号和模型配置。
- 不是公网 SaaS。默认不开放 Funnel，也不预置任何第三方服务或通知频道。

## 主要功能

| 功能 | 说明 |
| --- | --- |
| 对话 | 查看历史、新建会话、继续对话、流式查看回复 |
| 审批 | 在手机上允许一次、允许当前会话或拒绝 Codex 的权限请求 |
| 项目 | 选择工作目录，查看当前项目上下文 |
| 文件 | 浏览工作区文件、查看图片和常见文本内容 |
| 服务页 | 添加自己的 tailnet 或局域网网址，通过电脑端代理打开 |
| 扫码配对 | 电脑生成二维码，手机扫码后自动保存访问地址和口令 |
| 通知 | 默认关闭；可由使用者自行配置推送渠道 |
| 更新 | 可从已配对的电脑下载新版本 APK |
| 托盘 | 显示二维码、复制地址、开机自启、打开日志 |

首次启动时，服务页为空，通知频道为空，不会继承任何其他使用者的配置。

## 系统架构

```text
┌──────────────────────────────┐
│ 手机                         │
│ 官方 Tailscale App           │
│ 随行 App（WebView）或浏览器  │
└──────────────┬───────────────┘
               │ Tailscale / WireGuard
               ▼
┌──────────────────────────────┐
│ Windows 个人电脑             │
│ codex-mobile.exe             │
│ ├─ 内嵌 tsnet                │
│ ├─ HTTP API 与内嵌网页       │
│ ├─ Codex app-server          │
│ └─ 本地项目文件与运行环境    │
└──────────────────────────────┘
```

电脑端使用 Tailscale 的 `tsnet`，不需要安装或配置系统级 Tailscale 客户端。
手机端使用官方 Tailscale App 登录账号。两者加入同一个 tailnet 后，通过
WireGuard 加密链路访问随行服务。

默认访问地址形如：

```text
http://<电脑节点名>.<tailnet>.ts.net/?k=<访问口令>
```

## 安全模型

- 默认仅监听 tailnet 内网入口，`-funnel=false`。
- 访问地址必须携带高熵口令；口令等同于该电脑的控制权限。
- Tailscale 身份、访问口令、会话数据和二维码全部保存在本机数据目录。
- 仓库不包含个人身份、签名密钥、运行日志或预先配置的服务。
- 不同使用者应使用各自的 Tailscale 账号、Codex 账号和独立部署。
- 不推荐把 `%LOCALAPPDATA%\CodexMobile` 整体发给其他人。

如果启用了可选 Funnel 入口，该地址可被互联网访问，应额外评估 Tailscale
账户政策、带宽限制和电脑上的 Codex 权限。

## 环境要求

| 项目 | 要求 |
| --- | --- |
| 电脑系统 | Windows 10/11 x64 |
| Codex | 已安装 Codex 桌面版或 Codex CLI，并完成登录 |
| Tailscale | 电脑和手机使用同一账号下的同一个 tailnet |
| Android | Android 8.0 及以上，安装官方 Tailscale App |
| 浏览器 | iPhone 等设备可通过浏览器使用，但没有 Android App 的扫码配对和系统通知 |
| 构建 Go 端 | Go 1.27 或更高版本 |
| 构建 Android 端 | JDK 17、Android SDK 35、Gradle 8.14 |

## 安装

当前首发版本提供源码。正式 APK 需要部署者使用自己的签名密钥构建，避免把个人
签名密钥带入仓库。

### 1. 准备 Codex 和 Tailscale

确认电脑上可以正常执行：

```powershell
codex
```

然后在手机上安装官方 Tailscale App，并登录与电脑相同的 Tailscale 账号。

### 2. 构建电脑端

```powershell
git clone https://github.com/JinCV0/codex-mobile-framework.git
cd codex-mobile-framework\codex-mobile

$env:GOPROXY='https://goproxy.cn,direct'
go test ./...
go build -ldflags "-H=windowsgui -s -w" -o codex-mobile.exe .
```

国内网络无法访问默认 Go 模块代理时，可以继续使用上面的 `GOPROXY` 设置。

### 3. 构建 Android 客户端

安装本地 Android 工具链：

```powershell
cd ..\android
powershell -ExecutionPolicy Bypass -File .\setup-android.ps1
powershell -ExecutionPolicy Bypass -File .\setup-android.ps1 -Verify
```

仅做本机功能测试时，可以生成调试包：

```powershell
cd app
gradle assembleDebug --no-daemon
```

正式分发前应创建自己的签名密钥，并在
`android/app/keystore.properties` 中配置 `storeFile`、`storePassword`、
`keyAlias` 和 `keyPassword`。该文件与密钥目录已被 `.gitignore` 排除。

```powershell
gradle assembleRelease --no-daemon
```

正式包位于：

```text
android/app/app/build/outputs/apk/release/app-release.apk
```

### 4. 启动电脑端

```powershell
cd ..\..\codex-mobile
.\codex-mobile.exe
```

首次启动会：

1. 创建 `%LOCALAPPDATA%\CodexMobile` 数据目录；
2. 生成只属于这台电脑的访问口令；
3. 打开 Tailscale 登录页面；
4. 启动本地 HTTP 服务和托盘图标。

登录完成后，托盘菜单可以查看节点状态、打开二维码和复制访问地址。

### 5. 配对手机

1. 确认手机上的官方 Tailscale App 已连接；
2. 确认手机和电脑处于同一个 tailnet；
3. 在电脑托盘菜单选择“显示二维码”；
4. 安装了随行 App 时扫左侧配对码；
5. 只使用浏览器时扫右侧网页码；
6. 发送一条测试消息，确认对话、流式输出和审批正常。

## 日常使用

### 对话

打开“对话”页可以查看已有会话、新建会话、继续历史对话，以及查看 Codex
逐字生成的回复。

### 审批

Codex 请求越过当前沙箱时，手机端会出现审批卡片。可以选择允许一次、允许当前
会话或拒绝。超过等待时间未处理时，请求会按拒绝处理。

### 项目

新建或修改会话时，可以指定工作目录。手机端只负责传递目录和消息，实际读写仍由
电脑上的 Codex 完成。

### 服务

“服务”页默认空白。使用者可以添加自己的 tailnet 网站或局域网地址，并交由电脑端
反向代理打开。这个功能用于访问使用者自己的内部工具，不会向仓库写入任何默认服务。

### 通知

通知默认关闭，也没有默认外部频道。需要远程推送时，可以在“我的 -> 通知”中自行
填写自己的推送渠道。

### 更新

电脑端和手机的更新彼此独立：

- 修改 Go 或网页代码后，替换电脑端 exe 并重启服务；
- 修改 Android 原生代码或资源后，重新构建并发布 APK；
- 已配对的手机会从自己的电脑检查更新。

使用发布脚本把 APK 放入更新通道：

```powershell
powershell -ExecutionPolicy Bypass -File .\发布新版本.ps1 -Notes "本次更新内容"
```

## 配置项

电脑端常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `-hostname` | `codex-mobile` | Tailscale 节点名 |
| `-funnel` | `false` | 是否额外开放公网 Funnel 入口 |
| `-tailnet` | `true` | 是否开放 tailnet 内网入口 |
| `-local` | `127.0.0.1:7799` | 本机调试入口，留空则关闭 |
| `-data` | `%LOCALAPPDATA%\CodexMobile` | 数据目录 |
| `-tray` | `true` | 是否显示托盘图标 |
| `-qr` | `false` | 启动后是否自动打开二维码页面 |
| `-autostart` | 空 | `on`、`off` 或 `status` |

## 数据目录

```text
%LOCALAPPDATA%\CodexMobile\
├─ token.txt          访问口令
├─ sessions.json      会话、常用项目和本机设置
├─ 入口.txt           当前访问地址
├─ 二维码.png         浏览器访问二维码
├─ 配对二维码.png     App 配对二维码
├─ 更新\              APK 更新通道
├─ tsnet\             Tailscale 节点身份
└─ 日志.log           运行日志
```

下列内容不得提交到仓库或转交他人：

```text
token.txt
tsnet\
sessions.json
二维码.png
配对二维码.png
入口.txt
日志.log
```

## 目录结构

```text
codex-mobile/       Windows 常驻服务、HTTP API 和内嵌网页
android/            Android WebView 客户端
brand/              图标与品牌资源
docs/DEPLOY.md      独立部署说明
发布新版本.ps1      发布 APK 到本机更新通道
重启电脑端.ps1      替换并重启常驻服务
桌面桥接.ps1        可选的 Codex 桌面版 app-server 桥接
```

## 开发与验证

```powershell
cd codex-mobile
go test ./...
go vet ./...
```

Android 调试构建：

```powershell
cd android\app
gradle assembleDebug --no-daemon
```

当前版本的 Android 测试包使用包名 `io.github.jincv0.suixing`，版本号 `1.0.0`。

## 已知限制

- 电脑端目前只支持 Windows。
- 手机 App 目前只发布 Android 版本。
- 手机端不内嵌 Tailscale，需要安装官方 Tailscale App。
- 一个部署对应一个 Codex 环境和一组本机权限，不提供多租户隔离。
- Funnel 为可选功能，默认关闭。
- 项目依赖 Codex app-server 的官方协议；Codex 未来变更协议时可能需要同步适配。
- 首发版本尚未提供带正式签名的二进制发布。

## 参与贡献

提交改动时请保持以下边界：

- 不提交个人服务地址、通知频道、Tailscale 身份或访问口令；
- 不提交签名密钥、APK、exe、日志或本机数据目录；
- 新增默认配置前，应确认它对所有新部署都成立；
- 涉及电脑系统权限或远程访问范围的功能，需要同时说明安全边界。

## License

本项目采用 [MIT License](LICENSE)。

第三方组件和许可证见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
