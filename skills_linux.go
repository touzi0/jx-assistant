//go:build linux && !android

package main

// Linux 技能 Python 运行时：解释器候选 + 依赖补装。
//
// 与 darwin 侧（skills_darwin.go）同一目标、同一形态：
//   · 首选「内置 Linux 运行时」（技能/_runtime/linux，随发行包分发，离线
//     可用——python-build-standalone 布局：bin/python3 + lib/python3.x）；
//   · 缺失时退系统 python3（/usr/bin/python3 或 PATH 里的）——主流发行版
//     都自带，比 Windows「新装机只有商店占位符」的坑少得多；
//   · 依赖缺什么经 pip（清华镜像）补装：内置运行时装它自己的
//     site-packages；系统解释器装 --user（PEP 668 管控的发行版还要
//     --break-system-packages，Ubuntu 24.04 实测需要）。
//
// 探测与装配走 runToolChildEnv（python_env.go / darwin 侧同款口径）：
// 装配是引擎自身的可信操作（模型无法触达），且 pip 要写运行时目录/用户
// site——那在沙箱的写白名单之外。模型触发的脚本执行仍走沙箱。

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// linuxPythonExe 技能解释器候选（按优先级），找不到返回空。
func (a *App) linuxPythonExe() string {
	if dir, err := a.SkillsDir(); err == nil {
		if p := filepath.Join(dir, "_runtime", "linux", "bin", "python3"); fileExists(p) {
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

// ensureLinuxSkillRuntime 保证 Linux 技能解释器 + 依赖可用（幂等，结果进程级缓存）。
func (a *App) ensureLinuxSkillRuntime(modules, wheels []string, emit func(string)) (*pythonRuntime, error) {
	exe := a.linuxPythonExe()
	if exe == "" {
		return nil, fmt.Errorf("Linux 上没有可用的 python3：内置运行时（技能/_runtime/linux）缺失，" +
			"且系统未安装（Debian/Ubuntu: apt install python3；RHEL 系: dnf install python3），装好或随发行包补上运行时后重试")
	}
	py := &pythonRuntime{Exe: exe, Version: "Linux 运行时"}
	missing := []string{}
	for _, m := range modules {
		if !py.hasModule(m) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return py, nil
	}
	emit(fmt.Sprintf("正在为 Linux 运行时补装技能依赖（%s，走清华镜像）…", strings.Join(wheels, " ")))
	if err := linuxPipInstall(exe, wheels, exe == "/usr/bin/python3"); err != nil {
		return nil, err
	}
	py.clearModuleCache()
	if !pyModulesReady(py, modules) {
		return nil, fmt.Errorf("依赖补装后仍不可用（%s）", strings.Join(missing, ","))
	}
	return py, nil
}

// linuxPipInstall 经 pip 镜像装一组 wheel。systemExe=true 表示系统解释器：
// 装 --user（不动系统 site，无 root 也能装）；PEP 668 管控（externally-
// managed-environment，Ubuntu 24.04 起普遍）第一次会拒绝，识别后带
// --break-system-packages 重试一次（装进用户 site，对系统 Python 无实际影响）。
func linuxPipInstall(exe string, wheels []string, systemExe bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()
	base := []string{"-m", "pip", "install", "--quiet", "--no-warn-script-location", "-i", pipMirror}
	if systemExe {
		base = append(base, "--user")
	}
	out, err := runCommandCapture(ctx, exe, append(base, wheels...), "")
	if err != nil {
		if systemExe && strings.Contains(out, "externally-managed-environment") {
			retry := append(append([]string{}, base[:len(base)-1]...), "--user", "--break-system-packages")
			out, err = runCommandCapture(ctx, exe, append(retry, wheels...), "")
		}
		if err != nil {
			tail := out
			if len(tail) > 800 {
				tail = tail[len(tail)-800:]
			}
			return fmt.Errorf("pip 补装失败（%v）: %s", err, tail)
		}
	}
	return nil
}
