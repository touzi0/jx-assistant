package main

// voicetype_paths.go —— 独立语音输入法（VoiceTypeGo）可执行体的路径探测。
//
// 原在 voicetype_spawn.go（Windows 拉起子进程用）；voicetype_history.go
//（跨平台）读「最近语音记录」也走它，故挪到平台无关文件。探测顺序与
// 注释见函数体，非 Windows 平台上这些 .exe 候选自然一个都不存在，
// voicetypeHistoryDir() 如实返回空、相关端点优雅降级。

import (
	"os"
	"path/filepath"
)

// voiceTypeCandidates 输入法 exe 的探测顺序。
//
// 同时覆盖两种部署形态：
//   - 开发/仓库内：主程序在仓库根，输入法在同一工作区的兄弟目录 VoiceTypeGo/
//   - 安装版：安装目录下的 voicetype\ 子目录（打包时把整个 VoiceTypeGo 目录放进去）
func voiceTypeCandidates() []string {
	if p := os.Getenv("JX_VOICETYPE_EXE"); p != "" {
		return []string{p}
	}
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	parent := filepath.Dir(dir)
	return []string{
		// 标准位置：语音组件内部（<安装目录>/组件/voice/voicetype/），开发与安装都适用
		filepath.Join(dir, "组件", "voice", "voicetype", "voicetype_go.exe"),
		// 历史/兼容位置
		filepath.Join(dir, "voicetype", "voicetype_go.exe"),      // <安装目录>\voicetype\
		filepath.Join(dir, "VoiceTypeGo", "voicetype_go.exe"),    // <安装目录>\VoiceTypeGo\
		filepath.Join(parent, "VoiceTypeGo", "voicetype_go.exe"), // 仓库根的兄弟目录（迁移前）
	}
}

// appInstallDir 主程序所在目录（开发=仓库根，安装版=安装目录）。
func appInstallDir() string {
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}
