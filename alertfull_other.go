//go:build !windows

package main

// 久等提醒「桌面全屏蒙层」的非 Windows 桩（真正实现见 alertfull_windows.go）：
// 与 alertfront_other.go 的空实现同一模式——提醒仍然叮声 + 主窗口层照常，
// 只是开不了原生全屏蒙层。

func alertScreenShow(title, text, qid string, onDismiss func()) {}
func alertScreenHideIf(qid string)                              {}
