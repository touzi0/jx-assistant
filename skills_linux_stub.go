//go:build !linux || android

package main

// 非 Linux 平台（含安卓）的编译占位（运行时不会走到；见 skills_linux.go）。

import "fmt"

func (a *App) linuxPythonExe() string { return "" }

func (a *App) ensureLinuxSkillRuntime(modules, wheels []string, emit func(string)) (*pythonRuntime, error) {
	return nil, fmt.Errorf("sandbox unavailable: Linux 技能运行时不可用")
}
