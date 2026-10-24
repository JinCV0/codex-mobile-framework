package tray

import "encoding/binary"

// ICOFromPNG 把一张正方形 PNG 包成单条目 ICO。
// Windows Vista 以后支持 ICO 里直接放 PNG，所以不需要额外解码。
func ICOFromPNG(png []byte, size int) []byte {
	if size <= 0 || size > 255 {
		size = 0 // 0 表示 256
	}
	const headerSize = 6
	const entrySize = 16
	out := make([]byte, headerSize+entrySize+len(png))

	// ICONDIR
	binary.LittleEndian.PutUint16(out[0:], 0) // reserved
	binary.LittleEndian.PutUint16(out[2:], 1) // type = icon
	binary.LittleEndian.PutUint16(out[4:], 1) // count

	// ICONDIRENTRY
	out[6] = byte(size)                         // width
	out[7] = byte(size)                         // height
	out[8] = 0                                  // 调色板颜色数，0 = 不使用
	out[9] = 0                                  // reserved
	binary.LittleEndian.PutUint16(out[10:], 1)  // color planes
	binary.LittleEndian.PutUint16(out[12:], 32) // bits per pixel
	binary.LittleEndian.PutUint32(out[14:], uint32(len(png)))
	binary.LittleEndian.PutUint32(out[18:], headerSize+entrySize)

	copy(out[headerSize+entrySize:], png)
	return out
}
