//go:build linux && !android

package main

// 注意：构建标签必须排除 android——GOOS=android 同样满足 linux 标签
// （安卓是 Linux 内核），不排除会导致安卓上同时编入两套运行时常量。

const (
	isWindowsRuntime = false
	isLinuxRuntime   = true
	isAndroidRuntime = false
	isDarwinRuntime  = false
)
