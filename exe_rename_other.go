//go:build !windows

package main

// exe_rename_other.go —— 非 Windows 无 exe 文件名迁移（Android/Linux 发行名
// jx-assistant，不在本次「坚祥数字助理GO.exe → AI数字助理.exe」切换范围内）。

func migrateExeName(a *App) error { return nil }

// ensureProductShortcuts 非 Windows 没有桌面快捷方式形态，无入口可建。
func ensureProductShortcuts() {}

// legacyExeResiduePrefixes 非 Windows 没有旧名 exe，无残名可清。
func legacyExeResiduePrefixes() []string { return nil }
