//go:build !windows

package main

// voicetype_spawn_other.go —— 独立语音输入法（VoiceTypeGo）自动拉起的非 Windows 空桩。
//
// 真实现（voicetype_spawn.go）依赖 Windows 的单实例互斥体、Job 对象与 taskkill，
// 且输入法本体目前只有 Windows 构建。其他平台如实降级：
//   · 启动时静默跳过（只记一行日志，不影响主程序）；
//   · 停止函数为空操作——没有拉起过，也就没有需要收掉的子进程。
//（macOS 对应形态（全局听写输入法）将来单独立项，不在这层做。）

import "log"

// spawnVoiceType 非 Windows：语音输入法组件没有可执行体，跳过自动拉起。
func (a *App) spawnVoiceType() {
	if a.voiceComponentOn() {
		log.Printf("[voicetype] 当前平台没有独立语音输入法构建，跳过自动拉起")
	}
}

// stopVoiceTypeByComponent 非 Windows：没有拉起过，无需按组件关闭收尾。
func stopVoiceTypeByComponent() {}

// stopVoiceType 非 Windows：没有拉起过，退出时无需收子进程。
func stopVoiceType() {}

// voiceTypeRunning 非 Windows 恒 false：本平台没有输入法构建，不可能在运行。
//（voicetype_history.go 据此决定「重新识别」是否只能由主程序直调 ASR worker。）
func voiceTypeRunning() bool { return false }
