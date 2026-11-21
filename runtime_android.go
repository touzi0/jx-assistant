//go:build android

package main

// 安卓运行时：与 linux 同为类 Unix，但内核禁用 Landlock 与非特权
// 用户命名空间（实测 android15-6.6 内核 CONFIG 均未启用），OS 级命令
// 沙箱不可用——命令执行对齐社区方案（DSH 手机版）直接运行，隔离由
// 安卓应用沙箱（app uid + SELinux）承担。

const (
	isWindowsRuntime = false
	isLinuxRuntime   = false
	isAndroidRuntime = true
	isDarwinRuntime  = false
)
