//go:build darwin

package main

// macOS 技能 Python 运行时：解释器候选 + 依赖补装。
//
// 与 Windows 便携运行时（自包含、绝不碰系统 python）同一目标、不同路径：
//   · 首选「内置 mac 运行时」（技能/_runtime/macos，随仓库/安装包分发，
//     离线可用——对齐 Windows 侧「克隆即用」的意图）；
//   · 缺失时退到系统 python3（/usr/bin/python3 或 PATH 里的）——macOS 没有
//     Windows「新装机只有商店占位符」的坑，CLT 装好即有真解释器；
//   · 依赖缺什么经 pip（清华镜像）补装到运行时目录（内置）或 --user（系统）。
//
// 探测与装配都走 runToolChildEnv（不受限直跑，python_env.go 同款）：
// 装配是引擎自身的可信操作（模型无法触达），且 pip 要写运行时目录/
// 用户 site——那在沙箱的写白名单之外。模型触发的脚本执行仍走沙箱，
// 沙箱内 import 对 __pycache__ 写失败是静默跳过（CPython 语义），不受影响。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// darwinPythonExe 技能解释器候选（按优先级），找不到返回空。
func (a *App) darwinPythonExe() string {
	if dir, err := a.SkillsDir(); err == nil {
		if p := filepath.Join(dir, "_runtime", "macos", "bin", "python3"); fileExists(p) {
			return p
		}
	}
	if fileExists("/usr/bin/python3") {
		return "/usr/bin/python3"
	}
	if p, err := exec.LookPath("python3"); err == nil {
		return p
	}
	return ""
}

// ensureDarwinSkillRuntime 保证 macOS 技能解释器 + 依赖可用（幂等，结果进程级缓存）。
func (a *App) ensureDarwinSkillRuntime(modules, wheels []string, emit func(string)) (*pythonRuntime, error) {
	exe := a.darwinPythonExe()
	if exe == "" {
		return nil, fmt.Errorf("macOS 上没有可用的 python3：内置运行时（技能/_runtime/macos）缺失，" +
			"且系统未安装（装 Xcode 命令行工具：xcode-select --install，或 brew install python3）后重试")
	}
	py := &pythonRuntime{Exe: exe, Version: "macOS 运行时"}
	missing := []string{}
	for _, m := range modules {
		if !py.hasModule(m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return py, nil
	}
	emit(fmt.Sprintf("正在为 macOS 运行时补装技能依赖（%s，走清华镜像）…", strings.Join(wheels, " ")))
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()
	args := []string{"-m", "pip", "install", "--quiet", "--no-warn-script-location", "-i", pipMirror}
	if exe == "/usr/bin/python3" {
		// 系统解释器：装到用户 site（系统 site 目录受 SIP 保护不可写）
		args = append(args, "--user")
	}
	args = append(args, wheels...)
	out, err := runCommandCapture(ctx, exe, args, "")
	if err != nil {
		tail := out
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return nil, fmt.Errorf("pip 补装失败（%v）: %s", err, tail)
	}
	py.clearModuleCache()
	if !pyModulesReady(py, modules) {
		return nil, fmt.Errorf("依赖补装后仍不可用（%s）", strings.Join(missing, ","))
	}
	return py, nil
}

// fixupDarwinGUIPath 补全 GUI 形态下的 PATH（main 早期调用一次）。
//
// 从访达/Dock 启动的 App 只继承系统最小 PATH（/usr/bin:/bin:…），用户 shell
// 里配的开发工具（go/git/brew python 等）全都看不到——命令执行会成片报
// 「command not found」。把 mac 常规工具目录补到最前（幂等；终端启动时它们
// 本来就在，重复无害）。
func fixupDarwinGUIPath() {
	home, _ := os.UserHomeDir()
	extra := []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin"}
	if home != "" {
		extra = append(extra,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "go", "bin"),
		)
	}
	cur := os.Getenv("PATH")
	parts := strings.Split(cur, ":")
	seen := map[string]bool{}
	for _, p := range parts {
		seen[p] = true
	}
	add := []string{}
	for _, e := range extra {
		if !seen[e] {
			add = append(add, e)
			seen[e] = true
		}
	}
	if len(add) > 0 {
		_ = os.Setenv("PATH", strings.Join(add, ":")+":"+cur)
	}
}
