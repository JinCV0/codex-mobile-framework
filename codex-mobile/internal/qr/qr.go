// Package qr 把手机访问地址做成二维码，方便直接扫码打开。
package qr

import (
	"os"

	"github.com/skip2/go-qrcode"
)

// PNG 生成二维码图片数据。
func PNG(url string, size int) ([]byte, error) {
	return qrcode.Encode(url, qrcode.Medium, size)
}

// WriteFile 把二维码落盘，方便放进文档或聊天里。
func WriteFile(path, url string, size int) error {
	png, err := PNG(url, size)
	if err != nil {
		return err
	}
	return os.WriteFile(path, png, 0o600)
}
