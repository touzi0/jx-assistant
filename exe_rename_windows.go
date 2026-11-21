//go:build windows

package main

// exe_rename_windows.go —— 产品 exe 换名迁移（坚祥数字助理GO.exe → AI数字助理.exe）。
//
// 背景（2026-11-19 exe 文件名切换）：安装包路径的换名由 安装包.iss 的 Inno
// 迁移段完成（[InstallDelete] 删旧名 exe/旧快捷方式，装新名、建新快捷方式）；
// 但存量老用户大多走**静默更新**——engine 原地交换按本机当前名落位，老机器上
// exe 还叫旧名。本迁移在新版首次启动时补齐换名：
//
//   1. 把自己改名（Windows 允许改名运行中的 exe，内存映像不受影响；改完后
//      进程内 os.Executable()/GetModuleFileName 返回新路径，热更交换、重启
//      接力、沙箱 helper 等按自身路径工作的机制自动跟随）；
//   2. 新名已存在时（装过新名包又残留旧名等罕见场景）旧名让路改名
//      .old-<unix>，下次启动即走新名；
//   3. 仅当当前 exe 就装在注册表卸载项登记的安装目录（安装版本体在跑）时，
//      同步迁移用户可见入口：删旧开始菜单组/旧桌面快捷方式、建新组与新快捷
//      方式、把卸载项 DisplayName 改成新名。开发/隔离实例不动真机安装注册
//      信息（注册表 InstallLocation 与 exe 目录不一致即跳过）。
//
// 注册表与 .lnk 都借 PowerShell 完成（WScript.Shell COM 写快捷方式、
// Set-ItemProperty 改注册表）——Go 侧保持零新增直接依赖；脚本经
// -EncodedCommand（base64 UTF-16LE）执行，中文路径不吃代码页亏。
//
// 刻意不做的事：互斥体名不变（desktop_windows.go 保留旧串）——换名后的 exe
// 与尚未换名的旧实例互相认识，过渡期不会双开；data\ 等用户数据与 exe 名无关
// （相对路径锚 exe 目录），无需迁移。
//
// 任务体纪律：exe 改名失败返回 error（不记账，下次启动重试）；快捷方式/注册
// 表的小失败记日志消化（返回 nil 记账——文件名已经换成功，入口问题不值得
// 反复重试，且下次装安装包会重建全部入口）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

const (
	legacyExeName  = "坚祥数字助理GO.exe"
	renamedExeName = "AI数字助理.exe"
	// 安装包.iss 的 AppId（Inno 卸载注册表键，HKCU——PrivilegesRequired=lowest）
	uninstallRegKey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\{BE7768E7-93C5-49D6-9778-BC133A39A797}_is1`
)

// migrateExeName 迁移入口（appMigrations 条目调用）。
func migrateExeName(a *App) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	base := filepath.Base(exe)
	if !strings.EqualFold(base, legacyExeName) {
		return nil // 已是新名或异名（开发版/装包版）：无事可做，记账
	}
	newExe, err := renameLegacyExe(exe)
	if err != nil {
		return err
	}
	migrateShortcutsAndUninstall(newExe)
	return nil
}

// renameLegacyExe 把旧名 exe 换成新名；新名已被占时旧名让路（.old-<unix>）。
// 返回换名后的新名路径（让路场景返回已存在的新名路径）。
func renameLegacyExe(exe string) (string, error) {
	dir := filepath.Dir(exe)
	target := filepath.Join(dir, renamedExeName)
	if _, err := os.Stat(target); err == nil {
		// 新名已在（用户装过新名包又残留旧名）：旧名让路，下次启动走新名。
		// 运行中的自己不能删但能改名；带时间戳避免覆盖也占不住的极端残名。
		aside := fmt.Sprintf("%s.old-%d", exe, time.Now().Unix())
		if err := os.Rename(exe, aside); err != nil {
			return "", fmt.Errorf("旧名让路失败（%s → %s）: %w", exe, aside, err)
		}
		log.Printf("[迁移] 检测到 %s 已存在，旧名 exe 已让路为 %s", target, filepath.Base(aside))
		return target, nil
	}
	if err := os.Rename(exe, target); err != nil {
		return "", fmt.Errorf("exe 换名失败（%s → %s，下次启动重试）: %w", exe, target, err)
	}
	log.Printf("[迁移] exe 已换名：%s → %s（本次进程继续运行，下次启动即新名）", exe, target)
	return target, nil
}

// migrateShortcutsAndUninstall 迁移用户可见入口（快捷方式/卸载项显示名）。
// 全部动作在一段 PowerShell 里（含「是否安装版本体在跑」的判断：注册表
// InstallLocation 与 exe 目录一致才动，开发/隔离实例天然跳过）。
func migrateShortcutsAndUninstall(newExe string) {
	script := shortcutMigrationScript(newExe)
	enc := base64.StdEncoding.EncodeToString(utf16leBytes(script))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", enc)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		// 文件名已换成功：入口迁移失败不值得让整个迁移重跑（下次装安装包
		// 会重建全部入口），记日志收场
		log.Printf("[迁移] 快捷方式/卸载项迁移脚本失败（不影响换名结果）: %v\n%s", err, out)
		return
	}
	log.Printf("[迁移] 快捷方式/卸载项：%s", strings.TrimSpace(string(out)))
}

// shortcutMigrationScript 生成完整迁移脚本（经 -EncodedCommand 执行）：
//   1. 无卸载注册表项（未走安装包安装的裸 exe）→ 直接退出，只换文件名；
//   2. 注册表 InstallLocation ≠ 当前 exe 目录（开发/隔离实例）→ 退出；
//   3. DisplayName → 「AI数字助理」；删旧开始菜单组「坚祥数字助理GO」与旧
//      桌面快捷方式；建新组「AI数字助理」（主入口 + 卸载入口）；仅当原来
//      有桌面快捷方式时才建新桌面快捷方式（尊重没勾桌面图标的用户）。
//
// 开始菜单/桌面路径用 [Environment]::GetFolderPath 取（兼容 OneDrive 桌面
// 重定向），与 Inno 的 {autoprograms}/{autodesktop} 同源；卸载入口目标从
// 注册表 UninstallString 读（形如 "...unins000.exe"）。
func shortcutMigrationScript(newExe string) string {
	dir := filepath.Dir(newExe)
	ps := []string{
		`$ErrorActionPreference = 'Stop'`,
		`$key = 'HKCU:\` + uninstallRegKey + `'`,
		`if (-not (Test-Path -LiteralPath $key)) { Write-Output 'no-uninstall-key（未走安装包安装，跳过入口迁移）'; exit 0 }`,
		`$reg = Get-ItemProperty -LiteralPath $key`,
		`$loc = if ($reg.InstallLocation) { $reg.InstallLocation.TrimEnd('\') } else { '' }`,
		`$exeDir = '` + psQuoteSingle(dir) + `'`,
		`if ([string]::Compare($loc, $exeDir, $true) -ne 0) { Write-Output "not-install-dir（注册表 $loc ≠ $exeDir，开发/隔离实例不动安装入口）"; exit 0 }`,
		`Set-ItemProperty -LiteralPath $key -Name DisplayName -Value 'AI数字助理'`,
		`$ws = New-Object -ComObject WScript.Shell`,
		`$programs = [Environment]::GetFolderPath('Programs')`,
		`$desktop = [Environment]::GetFolderPath('Desktop')`,
		// 先记后删：桌面新快捷方式只建给原来就有的用户
		`$hadDesktop = Test-Path -LiteralPath (Join-Path $desktop '` + legacyBaseNoExt() + `.lnk') -PathType Leaf`,
		`Remove-Item -LiteralPath (Join-Path $programs '` + legacyBaseNoExt() + `') -Recurse -Force -ErrorAction SilentlyContinue`,
		`Remove-Item -LiteralPath (Join-Path $desktop '` + legacyBaseNoExt() + `.lnk') -Force -ErrorAction SilentlyContinue`,
		// 快捷方式文件名必须以 .lnk 结尾（CreateShortcut 对其他扩展名直接抛
		// COMException——2026-11-20 事故根因之一：这里曾写成 exe 名，脚本在
		// 删完旧入口后炸掉，新入口一个都没建出来）；命名与安装包.iss 对齐
		`$grp = Join-Path $programs '` + renamedBaseNoExt() + `'`,
		`New-Item -ItemType Directory -Path $grp -Force | Out-Null`,
		`$s = $ws.CreateShortcut((Join-Path $grp '` + renamedBaseNoExt() + `.lnk'))`,
		`$s.TargetPath = '` + psQuoteSingle(newExe) + `'`,
		`$s.Arguments = '-desktop'`,
		`$s.WorkingDirectory = $exeDir`,
		`$s.Description = '` + renamedBaseNoExt() + `'`,
		`$s.Save()`,
		`$u = $ws.CreateShortcut((Join-Path $grp ('卸载' + '` + renamedBaseNoExt() + `.lnk')))`,
		`$u.TargetPath = $reg.UninstallString`,
		`$u.WorkingDirectory = $exeDir`,
		`$u.Description = '卸载` + renamedBaseNoExt() + `'`,
		`$u.Save()`,
		`if ($hadDesktop) {`,
		`  $d = $ws.CreateShortcut((Join-Path $desktop '` + renamedBaseNoExt() + `.lnk'))`,
		`  $d.TargetPath = '` + psQuoteSingle(newExe) + `'`,
		`  $d.Arguments = '-desktop'`,
		`  $d.WorkingDirectory = $exeDir`,
		`  $d.Description = '` + renamedBaseNoExt() + `'`,
		`  $d.Save()`,
		`}`,
		`Write-Output ('入口已迁移：' + (Join-Path $grp '` + renamedBaseNoExt() + `.lnk') + '；DisplayName=AI数字助理')`,
	}
	return strings.Join(ps, "\n")
}

func legacyBaseNoExt() string  { return strings.TrimSuffix(legacyExeName, ".exe") }
func renamedBaseNoExt() string { return strings.TrimSuffix(renamedExeName, ".exe") }

// legacyExeResiduePrefixes 旧名 exe 的交换残名前缀（坚祥数字助理GO.exe.old /
// .old-<unix> / .new）。换名迁移后 FinalizeEngineSwap 按新名扫残留扫不到它们，
// 走热更换名的老机器会一直留着几十 MB 的死文件——自清时一并扫掉。
func legacyExeResiduePrefixes() []string {
	return []string{legacyExeName + ".old", legacyExeName + ".new"}
}

// ensureProductShortcuts 用户入口自愈（NewApp 每次启动后台跑，幂等，不走迁移
// 账本——这是常态自检不是一次性任务）。
//
// 事故背景（2026-11-20 生产）：换名迁移的入口迁移只认 Inno 安装目录（注册表
// 卸载键 + InstallLocation 一致），绿色目录/开发目录形态的机器换名后桌面与
// 开始菜单什么都没有——重启一失败用户就再也找不到程序入口。入口不该依赖
// 「怎么装的」：正式名 exe 跑起来就把两个入口补齐/修复；旧名入口只在目标
// 已失链（换名后指向的旧文件不在了）时顺手清掉，还能用的不碰（未升级机器
// 仍靠它进出）。开发/隔离实例（开发 exe 名，或带 JX_SKIP_MIGRATIONS /
// JX_DESKTOP_MUTEX / 冒烟自动关闭环境变量）一律不碰用户桌面。
func ensureProductShortcuts() {
	if os.Getenv("JX_SKIP_MIGRATIONS") != "" || os.Getenv("JX_DESKTOP_MUTEX") != "" || os.Getenv("JX_DESKTOP_AUTOCLOSE") != "" {
		return
	}
	exe := currentExePath()
	if exe == "" {
		return
	}
	if !shortcutEnsureEligible(filepath.Base(exe)) {
		return
	}
	script := shortcutEnsureScript(exe)
	enc := base64.StdEncoding.EncodeToString(utf16leBytes(script))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", enc)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// stdout/stderr 分开收：PS 在 stderr 被接管时会把自身流包装成 CLIXML
	// 噪音，合流会把脚本真正的结果行（shortcuts fixed=N）搅没
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err != nil {
		log.Printf("[入口自愈] 快捷方式自愈脚本失败（下次启动再试）: %v\n%s\n%s", err, out.String(), errOut.String())
		return
	}
	log.Printf("[入口自愈] %s", strings.TrimSpace(out.String()))
}

// shortcutEnsureEligible 正式产物名才自建入口：AI数字助理.exe（现名）与
// 坚祥数字助理GO.exe（过渡期旧名——换名迁移失败待重试的机器）。开发产物
// （AI数字助理-开发.exe / 开发桌面版.exe / jxassistant.exe 等）天然排除。
func shortcutEnsureEligible(base string) bool {
	return strings.EqualFold(base, renamedExeName) || strings.EqualFold(base, legacyExeName)
}

// shortcutEnsureScript 自愈脚本（经 -EncodedCommand 执行，中文路径不吃代码页
// 亏）：桌面与开始菜单组各确保一个「AI数字助理.lnk」。缺失或目标失链（换名
// 后旧路径不在了）才写；目标完好的不动——指向别的可用安装也尊重现状（用户
// 多份共存时不抢入口）。桌面/开始菜单路径用 GetFolderPath（兼容 OneDrive
// 桌面重定向）。刻意不设「原来有桌面图标才建」的限制：2026-11-20 事故的
// 优先级是用户任何情况下都有入口。
func shortcutEnsureScript(exe string) string {
	dir := filepath.Dir(exe)
	ps := []string{
		`$ErrorActionPreference = 'Continue'`,
		`$exe = '` + psQuoteSingle(exe) + `'`,
		`$dir = '` + psQuoteSingle(dir) + `'`,
		`$ws = New-Object -ComObject WScript.Shell`,
		`$desktop = [Environment]::GetFolderPath('Desktop')`,
		`$programs = [Environment]::GetFolderPath('Programs')`,
		`$fixed = 0`,
		`function Ensure-Link([string]$lnk) {`,
		`  $need = $true`,
		`  if (Test-Path -LiteralPath $lnk -PathType Leaf) {`,
		`    try {`,
		`      $t = $ws.CreateShortcut($lnk)`,
		`      if ($t.TargetPath -and (Test-Path -LiteralPath $t.TargetPath -PathType Leaf)) { $need = $false }`,
		`    } catch { }`,
		`  }`,
		`  if ($need) {`,
		`    $s = $ws.CreateShortcut($lnk)`,
		`    $s.TargetPath = $exe`,
		`    $s.Arguments = '-desktop'`,
		`    $s.WorkingDirectory = $dir`,
		`    $s.Description = '` + renamedBaseNoExt() + `'`,
		`    $s.IconLocation = $exe + ',0'`,
		`    $s.Save()`,
		`    $script:fixed = $script:fixed + 1`,
		`  }`,
		`}`,
		`$grp = Join-Path $programs '` + renamedBaseNoExt() + `'`,
		`New-Item -ItemType Directory -Path $grp -Force | Out-Null`,
		// 快捷方式文件必须以 .lnk 结尾（CreateShortcut 对其他扩展名抛
		// COMException）；命名与安装包.iss 一致：AI数字助理.lnk
		`Ensure-Link (Join-Path $desktop '` + renamedBaseNoExt() + `.lnk')`,
		`Ensure-Link (Join-Path $grp '` + renamedBaseNoExt() + `.lnk')`,
		`$oldD = Join-Path $desktop '` + legacyBaseNoExt() + `.lnk'`,
		`if (Test-Path -LiteralPath $oldD -PathType Leaf) {`,
		`  $t = $ws.CreateShortcut($oldD)`,
		`  if ($t.TargetPath -and -not (Test-Path -LiteralPath $t.TargetPath -PathType Leaf)) { Remove-Item -LiteralPath $oldD -Force }`,
		`}`,
		`$oldG = Join-Path $programs '` + legacyBaseNoExt() + `'`,
		`if (Test-Path -LiteralPath $oldG -PathType Container) {`,
		`  $oldM = Join-Path $oldG '` + legacyBaseNoExt() + `.lnk'`,
		`  if (Test-Path -LiteralPath $oldM -PathType Leaf) {`,
		`    $t = $ws.CreateShortcut($oldM)`,
		`    if ($t.TargetPath -and -not (Test-Path -LiteralPath $t.TargetPath -PathType Leaf)) { Remove-Item -LiteralPath $oldG -Recurse -Force }`,
		`  }`,
		`}`,
		`Write-Output ('shortcuts fixed=' + $fixed)`,
	}
	return strings.Join(ps, "\n")
}

// psQuoteSingle PowerShell 单引号字面量（内部单引号加倍）。
func psQuoteSingle(s string) string { return strings.ReplaceAll(s, "'", "''") }

// utf16leBytes UTF-16LE 编码（-EncodedCommand 的要求）。
func utf16leBytes(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	return b
}
