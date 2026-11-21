//go:build windows

package main

// exe_rename_windows_test.go —— exe 换名迁移的单测：文件换名三分支（改名/
// 让路/异名跳过）与快捷方式脚本的生成内容。注册表真键与 COM 快捷方式的
// 真链路不做单测（不能动本机安装注册信息），由装机验证覆盖。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenameLegacyExeRenames(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, legacyExeName)
	if err := os.WriteFile(old, []byte("PE-ish"), 0644); err != nil {
		t.Fatal(err)
	}
	newExe, err := renameLegacyExe(old)
	if err != nil {
		t.Fatalf("换名失败: %v", err)
	}
	if want := filepath.Join(dir, renamedExeName); !stringsEqualFold(newExe, want) {
		t.Fatalf("返回路径 %s，期望 %s", newExe, want)
	}
	if _, err := os.Stat(newExe); err != nil {
		t.Fatalf("新名文件不存在: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("旧名文件应已消失，stat err=%v", err)
	}
	b, err := os.ReadFile(newExe)
	if err != nil || string(b) != "PE-ish" {
		t.Fatalf("内容应原样保留，got %q err=%v", b, err)
	}
}

func TestRenameLegacyExeYieldsWhenTargetExists(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, legacyExeName)
	if err := os.WriteFile(old, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, renamedExeName)
	if err := os.WriteFile(target, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := renameLegacyExe(old)
	if err != nil {
		t.Fatalf("让路失败: %v", err)
	}
	if !stringsEqualFold(got, target) {
		t.Fatalf("应返回已存在的新名路径，got %s", got)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("新名文件内容不应被动过，got %q", b)
	}
	// 旧名应变成 .old-<unix> 残名（运行中让路的标准形态）
	entries, _ := os.ReadDir(dir)
	aside := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), legacyExeName+".old-") {
			aside = e.Name()
		}
	}
	if aside == "" {
		t.Fatalf("旧名应让路成 %s.old-<unix>，目录里只有 %v", legacyExeName, entries)
	}
}

func TestShortcutMigrationScriptContent(t *testing.T) {
	s := shortcutMigrationScript(`D:\123\坚祥数字助理GO\AI数字助理.exe`)
	for _, want := range []string{
		`HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\{BE7768E7-93C5-49D6-9778-BC133A39A797}_is1`,
		`Set-ItemProperty -LiteralPath $key -Name DisplayName -Value 'AI数字助理'`,
		`GetFolderPath('Programs')`,
		`GetFolderPath('Desktop')`,
		`Test-Path -LiteralPath (Join-Path $desktop '坚祥数字助理GO.lnk')`,
		`Remove-Item -LiteralPath (Join-Path $programs '坚祥数字助理GO')`,
		`$grp = Join-Path $programs 'AI数字助理'`,
		`$s = $ws.CreateShortcut((Join-Path $grp 'AI数字助理.lnk'))`,
		`$u = $ws.CreateShortcut((Join-Path $grp ('卸载' + 'AI数字助理.lnk')))`,
		`$d = $ws.CreateShortcut((Join-Path $desktop 'AI数字助理.lnk'))`,
		`$s.TargetPath = 'D:\123\坚祥数字助理GO\AI数字助理.exe'`,
		`$s.Arguments = '-desktop'`,
		`$u.TargetPath = $reg.UninstallString`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("迁移脚本缺关键片段 %q\n脚本:\n%s", want, s)
		}
	}
}

func TestShortcutEnsureEligible(t *testing.T) {
	for _, yes := range []string{renamedExeName, legacyExeName, strings.ToUpper(renamedExeName)} {
		if !shortcutEnsureEligible(yes) {
			t.Errorf("%s 应视为正式产物名", yes)
		}
	}
	for _, no := range []string{"AI数字助理-开发.exe", "开发桌面版.exe", "jxassistant.exe", "jx-assistant.exe", "AI数字助理.exe.bak"} {
		if shortcutEnsureEligible(no) {
			t.Errorf("%s 不应自建用户入口", no)
		}
	}
}

func TestShortcutEnsureScriptContent(t *testing.T) {
	s := shortcutEnsureScript(`D:\绿色目录\AI数字助理.exe`)
	for _, want := range []string{
		`$exe = 'D:\绿色目录\AI数字助理.exe'`,
		`GetFolderPath('Desktop')`,
		`GetFolderPath('Programs')`,
		`$grp = Join-Path $programs 'AI数字助理'`,
		`Ensure-Link (Join-Path $desktop 'AI数字助理.lnk')`,
		`Ensure-Link (Join-Path $grp 'AI数字助理.lnk')`,
		`$s.TargetPath = $exe`,
		`$s.Arguments = '-desktop'`,
		// 目标完好的入口不重写（尊重指向别的可用安装的现状）
		`if ($t.TargetPath -and (Test-Path -LiteralPath $t.TargetPath -PathType Leaf)) { $need = $false }`,
		// 旧名入口只在目标失链时清
		`$oldD = Join-Path $desktop '坚祥数字助理GO.lnk'`,
		`if ($t.TargetPath -and -not (Test-Path -LiteralPath $t.TargetPath -PathType Leaf)) { Remove-Item -LiteralPath $oldD -Force }`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("自愈脚本缺关键片段 %q\n脚本:\n%s", want, s)
		}
	}
}
