//go:build windows

package main

// shortcut_reallink_windows_test.go —— 入口脚本（迁移 + 自愈）的真链路回归：
// 在临时目录 + 自有注册表子树里真跑 PowerShell/WScript.Shell，覆盖
// 「.lnk 扩展名」这一类错（2026-11-20 生产事故根因：CreateShortcut 对非
// .lnk 路径抛 COMException，脚本删完旧入口后炸掉，新入口一个没建出来）。
// 场景：迁移脚本（假卸载键端到端）与自愈脚本（失链重建/旧名死链清理/
// 不抢有效入口/缺失新建）。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func runScriptFile(t *testing.T, path string) string {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("脚本执行失败: %v\n%s", err, out)
	}
	return string(out)
}

func lnkTarget(t *testing.T, lnk string) string {
	snippet := fmt.Sprintf(`$t = New-Object -ComObject WScript.Shell; $s = $t.CreateShortcut('%s'); Write-Output $s.TargetPath`, strings.ReplaceAll(lnk, "'", "''"))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", snippet)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("读 lnk 失败: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeLnk(t *testing.T, lnk, target string) {
	dir := filepath.Dir(lnk)
	os.MkdirAll(dir, 0755)
	snippet := fmt.Sprintf(`$w = New-Object -ComObject WScript.Shell; $s = $w.CreateShortcut('%s'); $s.TargetPath = '%s'; $s.Save()`,
		strings.ReplaceAll(lnk, "'", "''"), strings.ReplaceAll(target, "'", "''"))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", snippet)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造 lnk 失败: %v\n%s", err, out)
	}
}

func TestShortcutEnsureScriptRealRunTemp(t *testing.T) {
	base := t.TempDir()
	fakeDesk := filepath.Join(base, "desk")
	fakeProg := filepath.Join(base, "prog")
	os.MkdirAll(fakeDesk, 0755)
	os.MkdirAll(fakeProg, 0755)
	exe := filepath.Join(base, "app", renamedExeName)
	os.MkdirAll(filepath.Dir(exe), 0755)
	os.WriteFile(exe, []byte("MZ-fake"), 0644)

	script := shortcutEnsureScript(exe)
	r1 := strings.Replace(script, "[Environment]::GetFolderPath('Desktop')", fmt.Sprintf(`'%s'`, fakeDesk), 1)
	r2 := strings.Replace(r1, "[Environment]::GetFolderPath('Programs')", fmt.Sprintf(`'%s'`, fakeProg), 1)
	if r1 == script || r2 == r1 {
		t.Fatalf("临时目录替换未命中\n%s", r2)
	}
	ps1 := filepath.Join(base, "ensure.ps1")
	// PS 5.1 无 BOM 按 ANSI 解析：临时脚本带 BOM 落盘
	os.WriteFile(ps1+".nobom", []byte(r2), 0644)
	if out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf(`$c=[IO.File]::ReadAllText('%s'); [IO.File]::WriteAllText('%s', $c, (New-Object Text.UTF8Encoding $true))`, ps1+".nobom", ps1)).CombinedOutput(); err != nil {
		t.Fatalf("转 BOM 失败: %v\n%s", err, out)
	}

	// 场景①：全新环境 → 桌面 + 开始菜单组各建一个，目标=当前 exe
	out := runScriptFile(t, ps1)
	t.Logf("首跑输出: %s", out)
	dLnk := filepath.Join(fakeDesk, renamedBaseNoExt()+".lnk")
	pLnk := filepath.Join(fakeProg, renamedBaseNoExt(), renamedBaseNoExt()+".lnk")
	for _, lnk := range []string{dLnk, pLnk} {
		if _, err := os.Stat(lnk); err != nil {
			t.Fatalf("入口没建出来 %s: %v", lnk, err)
		}
		if got := lnkTarget(t, lnk); !strings.EqualFold(got, exe) {
			t.Fatalf("入口目标不对 %s: %s", lnk, got)
		}
	}

	// 场景②：旧名桌面快捷方式目标失链 → 清掉；旧名开始菜单组同理
	deadTarget := filepath.Join(base, "gone", legacyExeName)
	makeLnk(t, filepath.Join(fakeDesk, legacyBaseNoExt()+".lnk"), deadTarget)
	oldGrp := filepath.Join(fakeProg, legacyBaseNoExt())
	os.MkdirAll(oldGrp, 0755)
	makeLnk(t, filepath.Join(oldGrp, legacyBaseNoExt()+".lnk"), deadTarget)
	runScriptFile(t, ps1)
	if _, err := os.Stat(filepath.Join(fakeDesk, legacyBaseNoExt()+".lnk")); !os.IsNotExist(err) {
		t.Fatalf("失链旧名桌面入口应被清掉")
	}
	if _, err := os.Stat(oldGrp); !os.IsNotExist(err) {
		t.Fatalf("失链旧名开始菜单组应被清掉")
	}

	// 场景③：桌面入口指向别的可用安装 → 不抢
	otherExe := filepath.Join(base, "other", renamedExeName)
	os.MkdirAll(filepath.Dir(otherExe), 0755)
	os.WriteFile(otherExe, []byte("MZ-other"), 0644)
	makeLnk(t, dLnk, otherExe)
	runScriptFile(t, ps1)
	if got := lnkTarget(t, dLnk); !strings.EqualFold(got, otherExe) {
		t.Fatalf("指向别的可用安装的入口不应被抢：got %s", got)
	}

	// 场景④：入口目标失链（模拟换名后旧路径不在）→ 就地重建回当前 exe
	makeLnk(t, dLnk, deadTarget)
	runScriptFile(t, ps1)
	if got := lnkTarget(t, dLnk); !strings.EqualFold(got, exe) {
		t.Fatalf("失链入口应重建回当前 exe：got %s", got)
	}
}

func TestShortcutMigrationScriptRealRunTemp(t *testing.T) {
	base := t.TempDir()
	fakeDesk := filepath.Join(base, "desk")
	fakeProg := filepath.Join(base, "prog")
	os.MkdirAll(fakeDesk, 0755)
	os.MkdirAll(fakeProg, 0755)
	dir := filepath.Join(base, "app")
	newExe := filepath.Join(dir, renamedExeName)
	os.MkdirAll(dir, 0755)
	os.WriteFile(newExe, []byte("MZ-fake"), 0644)
	// 假卸载键（自有子树，不碰真安装注册信息）+ 旧入口在场
	fakeKey := `HKCU:\Software\jx-test-uninstall-20261121`
	unins := filepath.Join(base, "unins000.exe")
	os.WriteFile(unins, []byte("MZ"), 0644)
	if out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf(`New-Item -Path '%s' -Force | Out-Null; Set-ItemProperty -Path '%s' -Name InstallLocation -Value '%s'; Set-ItemProperty -Path '%s' -Name UninstallString -Value '"%s"'`,
			fakeKey, fakeKey, dir, fakeKey, unins)).CombinedOutput(); err != nil {
		t.Fatalf("造假键失败: %v\n%s", err, out)
	}
	defer func() {
		exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
			fmt.Sprintf(`Remove-Item -Path '%s' -Recurse -Force`, fakeKey)).Run()
	}()
	oldDeskLnk := filepath.Join(fakeDesk, legacyBaseNoExt()+".lnk")
	makeLnk(t, oldDeskLnk, filepath.Join(dir, legacyExeName)) // 旧桌面入口（目标随后会失链）
	oldGrp := filepath.Join(fakeProg, legacyBaseNoExt())
	os.MkdirAll(oldGrp, 0755)
	makeLnk(t, filepath.Join(oldGrp, legacyBaseNoExt()+".lnk"), filepath.Join(dir, legacyExeName))

	script := shortcutMigrationScript(newExe)
	r := strings.ReplaceAll(script, "[Environment]::GetFolderPath('Desktop')", fmt.Sprintf(`'%s'`, fakeDesk))
	r = strings.ReplaceAll(r, "[Environment]::GetFolderPath('Programs')", fmt.Sprintf(`'%s'`, fakeProg))
	r = strings.Replace(r, `HKCU:\`+uninstallRegKey, fakeKey, 1)
	ps1 := filepath.Join(base, "migrate.ps1")
	os.WriteFile(ps1+".nobom", []byte(r), 0644)
	if out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf(`$c=[IO.File]::ReadAllText('%s'); [IO.File]::WriteAllText('%s', $c, (New-Object Text.UTF8Encoding $true))`, ps1+".nobom", ps1)).CombinedOutput(); err != nil {
		t.Fatalf("转 BOM 失败: %v\n%s", err, out)
	}
	out := runScriptFile(t, ps1)
	t.Logf("迁移脚本输出: %s", out)

	// 断言：DisplayName 已改、旧组/旧桌面入口已删、新组+主入口+卸载入口+桌面入口已建
	chk := func(snippet string) string {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", snippet)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		o, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("检查失败: %v\n%s\n%s", err, snippet, o)
		}
		return strings.TrimSpace(string(o))
	}
	if got := chk(fmt.Sprintf(`(Get-ItemProperty -Path '%s').DisplayName`, fakeKey)); got != "AI数字助理" {
		t.Fatalf("DisplayName 应改为 AI数字助理，got %q", got)
	}
	if _, err := os.Stat(oldGrp); !os.IsNotExist(err) {
		t.Fatalf("旧开始菜单组应被删除")
	}
	if _, err := os.Stat(oldDeskLnk); !os.IsNotExist(err) {
		t.Fatalf("旧桌面入口应被删除")
	}
	newLnk := filepath.Join(fakeProg, renamedBaseNoExt(), renamedBaseNoExt()+".lnk")
	if got := lnkTarget(t, newLnk); !strings.EqualFold(got, newExe) {
		t.Fatalf("新主入口目标不对: %s", got)
	}
	unLnk := filepath.Join(fakeProg, renamedBaseNoExt(), "卸载"+renamedBaseNoExt()+".lnk")
	if got := lnkTarget(t, unLnk); !strings.Contains(got, "unins000.exe") {
		t.Fatalf("卸载入口目标不对: %s", got)
	}
	if got := lnkTarget(t, filepath.Join(fakeDesk, renamedBaseNoExt()+".lnk")); !strings.EqualFold(got, newExe) {
		t.Fatalf("新桌面入口目标不对: %s", got)
	}
}
