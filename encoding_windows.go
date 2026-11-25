//go:build windows

package main

import (
	"sync"
	"syscall"
)

var (
	ansiCodePageOnce sync.Once
	ansiCodePage     uint32
)

// systemANSICodePage 返回系统 ANSI 代码页（中文 Windows=936、日文=932、韩文=949、
// 俄文=866、西欧=1252）。GetACP 是纯内核查询、无 IO 也不起子进程，但仍缓存一次：
// 控制台输出解码在热路径上（每条命令、每次截断都会问一次），缓存后零成本。
//
// 特意**不**用 `cmd /c chcp` 探测"控制台当前代码页"：那要起子进程（zcode 每条
// 命令探一次、1s 超时），而我们要的只是"这台机器的本地代码页是哪一族"，
// GetACP 足以定位解码器（见 consoleCodePageDecoder 的映射表）。
func systemANSICodePage() uint32 {
	ansiCodePageOnce.Do(func() {
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		if r, _, _ := kernel32.NewProc("GetACP").Call(); r != 0 {
			ansiCodePage = uint32(r)
		}
	})
	return ansiCodePage
}
