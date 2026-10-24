// Package autostart 管理开机自启。
//
// 只用 HKCU（当前用户）下的注册表项，不需要管理员权限，
// 也不会影响其他用户。
package autostart

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

const (
	runKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
	valueName = "CodexMobile"
)

// Enabled 报告是否已经设置了开机自启。
func Enabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	_, _, err = key.GetStringValue(valueName)
	return err == nil
}

// Enable 把当前程序写进开机自启列表。
func Enable() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue(valueName, `"`+exe+`"`)
}

// Disable 取消开机自启。
func Disable() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.DeleteValue(valueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

// Toggle 按需要打开或关闭，返回最终状态。
func Toggle(on bool) (bool, error) {
	if on {
		return true, Enable()
	}
	return false, Disable()
}
