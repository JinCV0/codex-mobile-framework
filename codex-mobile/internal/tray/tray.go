// Package tray 提供 Windows 托盘图标与菜单。
package tray

import "fyne.io/systray"

// Handlers 是菜单项被点击时要做的事。
type Handlers struct {
	OpenPage  func()
	ShowQR    func()
	CopyURL   func()
	Autostart func(on bool)
	DataDir   func()
	LogFile   func()
	Quit      func()
}

// Run 启动托盘并阻塞，直到用户选择退出。
// 必须在主线程调用。
func Run(icon []byte, tooltip string, autostartOn bool, h Handlers) {
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTooltip(tooltip)

		status := systray.AddMenuItem("Codex 随行 · 运行中", "手机打开下面这个地址就能用")
		status.Disable()
		systray.AddSeparator()

		openPage := systray.AddMenuItem("打开手机页面", "在本机浏览器里打开界面")
		showQR := systray.AddMenuItem("显示二维码", "用手机扫一下就能打开")
		copyURL := systray.AddMenuItem("复制访问地址", "把地址复制到剪贴板")
		systray.AddSeparator()

		autostartItem := systray.AddMenuItemCheckbox("开机自启", "登录 Windows 后自动运行", autostartOn)
		systray.AddSeparator()

		dataDir := systray.AddMenuItem("打开数据目录", "会话、口令、二维码都在这里")
		logFile := systray.AddMenuItem("打开日志", "出问题时看这个")
		systray.AddSeparator()

		quit := systray.AddMenuItem("退出", "关掉服务")

		go func() {
			for {
				select {
				case <-openPage.ClickedCh:
					call(h.OpenPage)
				case <-showQR.ClickedCh:
					call(h.ShowQR)
				case <-copyURL.ClickedCh:
					call(h.CopyURL)
				case <-autostartItem.ClickedCh:
					on := !autostartItem.Checked()
					if on {
						autostartItem.Check()
					} else {
						autostartItem.Uncheck()
					}
					if h.Autostart != nil {
						h.Autostart(on)
					}
				case <-dataDir.ClickedCh:
					call(h.DataDir)
				case <-logFile.ClickedCh:
					call(h.LogFile)
				case <-quit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		call(h.Quit)
	})
}

func call(fn func()) {
	if fn != nil {
		fn()
	}
}
