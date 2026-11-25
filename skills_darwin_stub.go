//go:build !darwin

package main

// 非 macOS 平台的编译占位（运行时不会走到；见 skills_darwin.go）。

import "fmt"

func (a *App) darwinPythonExe() string { return "" }

func (a *App) ensureDarwinSkillRuntime(modules, wheels []string, emit func(string)) (*pythonRuntime, error) {
	return nil, fmt.Errorf("sandbox unavailable: macOS 技能运行时不可用")
}

// fixupDarwinGUIPath 仅 macOS 有 GUI PATH 残缺问题，其他平台空操作。
func fixupDarwinGUIPath() {}
