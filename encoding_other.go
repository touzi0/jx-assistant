//go:build !windows

package main

// systemANSICodePage 在非 Windows 平台没有"ANSI 代码页"这个概念：返回 0，
// 解码照旧走 encoding.go 的 GB18030 兜底（与修复前行为一致）。
func systemANSICodePage() uint32 { return 0 }
