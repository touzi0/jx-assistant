//go:build windows

package main

// 立即重启的接力助手：主进程挂在 kill-on-close Job 里（processguard_windows.go），
// 自己拉起自己会随 Job 陪葬。这里经 WMI 把一个一次性 cmd 接力命令托管到 Job 外
// （WmiPrvSE 系统服务代为创建，与主进程的 Job 无任何从属关系）：等约 8 秒旧进程
// 退出 → start 拉起新 exe（连试 3 次、间隔约 8 秒，竞态兜底见 restartRelayCmd）
// → 追加 restart.log。不删 .old：留给新版启动自清（万一
// 新版起不来，.old 还在可手工救回）。
//
// 2026-10-15 实测教训（安装机 WMI→powershell 全链路排查，三层雷）：
//  1. WMI 派生进程不认裸名 powershell.exe（System32 根目录下没有它），必须全路径；
//  2. WMI 派生进程的工作目录是 C:\Windows\System32，相对路径脚本/日志全部失配；
//  3. 部分机器（安全软件/反作弊驱动）直接拦 WMI 派生进程执行 .ps1 文件与
//     cmd→powershell 链（"拒绝访问"），但 -Command 内联与 cmd 本体全程可用。
// 结论：接力不落地脚本文件、不依赖 powershell，纯 cmd 命令行内联（已在本机
// 7/7 实测通过，含中文路径与 D 盘安装目录）；powershell 只当 WMI 调用器（主进程
// 上下文里执行，不受上述拦截影响）。等待用 ping 计时（timeout 在无控制台环境
// 有翻车史）。WMI 侧带 Win32_ProcessStartup.ShowWindow=0 隐藏接力窗口。

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// absUpdatePath 把 DataDir 相对路径（安装版默认 "data"）解析成绝对路径——WMI 派生
// 的接力进程工作目录在 System32，相对路径在那边必然失配。
func absUpdatePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}

// spawnRestartHelper 生成接力命令并经 WMI 托管启动。调用方（powershell）允许随
// Job 陪葬：WMI 受理即成（本函数同步等它返回，应答在受理确认之后才发、进程才
// 退，无竞态）。WMI 不可用回退 CREATE_BREAKAWAY_FROM_JOB 逃逸。
func spawnRestartHelper(a *App, st updateBlockState) error {
	exePath := currentExePath()
	if exePath == "" {
		return fmt.Errorf("取不到本程序路径")
	}
	exePath = absUpdatePath(exePath)
	argv := st.EngineArgv
	if len(argv) == 0 {
		argv = os.Args
	}
	logPath := absUpdatePath(filepath.Join(a.Cfg.DataDir, "updates", "restart.log"))
	relayCmd := restartRelayCmd(exePath, argv[1:], logPath)
	// WMI 调用器：在主进程上下文里跑 powershell（正常环境，不受 WMI 派生拦截影
	// 响）。ShowWindow=0 让接力窗口不闪；Create 返回非 0 视为失败好走回退。
	caller := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$si = ([wmiclass]'Win32_ProcessStartup').CreateInstance(); $si.ShowWindow = 0; "+
			"$r = ([wmiclass]'Win32_Process').Create("+restartPSQuote(relayCmd)+", "+
			restartPSQuote(filepath.Dir(exePath))+", $si); "+
			"if ($r.ReturnValue -ne 0) { exit 1 }")
	caller.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := caller.Run(); err == nil {
		log.Printf("[更新] 重启接力已交 WMI 托管（旧进程退出后自动拉起新版，过程记于 %s）", logPath)
		return nil
	} else {
		log.Printf("[更新] WMI 托管失败（%v），回退 breakaway 接力", err)
	}
	return createBreakawayProcess(relayCmd)
}

// restartRelayCmd 拼一次性接力命令：ping 计时约 8 秒（等旧进程退出）→ start
// 拉起新版，共试 3 次、间隔约 8 秒 → 全程留痕 restart.log。start 首个引号参数
// 是窗口标题，必须给空串占位。路径全部绝对且带引号（含中文没问题，命令行
// 全程 UTF-16）；日志行保持 ASCII，避免 cmd 代码页把中文写成乱码。
//
// 多次拉起的安全性：单实例互斥体（desktop_windows.go）会把撞上已运行实例
// 的新进程收敛为「唤起旧窗口后退出」，不产生第二份窗口/服务——所以无条件
// 重试没有副作用。2026-11-20 生产事故：接力只拉一次，旧进程退得慢的机器上
// 新实例被互斥体顶掉之后再无人拉起，用户看到「重启起不来」且桌面无入口。
func restartRelayCmd(exePath string, args []string, logPath string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"") {
			quoted[i] = `"` + strings.ReplaceAll(a, `"`, `""`) + `"`
		} else {
			quoted[i] = a
		}
	}
	argLine := strings.Join(quoted, " ")
	dir := filepath.Dir(exePath)
	wait := `ping -n 9 127.0.0.1 >nul`
	mark := `echo [%date% %time%] relay: launching ` + exePath + ` >>"` + logPath + `" 2>&1`
	start := `start "" /d "` + dir + `" "` + exePath + `"`
	if argLine != "" {
		start += " " + argLine
	}
	cmdExe := os.Getenv("SystemRoot") + `\System32\cmd.exe`
	if os.Getenv("SystemRoot") == "" {
		cmdExe = "cmd.exe"
	}
	parts := []string{cmdExe + ` /c ` + wait, mark}
	for i := 0; i < 3; i++ {
		if i > 0 {
			parts = append(parts, wait)
		}
		parts = append(parts, start)
	}
	return strings.Join(parts, ` & `)
}

// createBreakawayProcess 以 CREATE_BREAKAWAY_FROM_JOB 启动（Job 已配
// BREAKAWAY_OK）。进程不在 Job 里时该标志被系统忽略；在 Job 里但 Job 不许
// 逃逸会报错，去掉标志重试一次（此时助手会随主进程一起被清，重启接力失败，
// 用户仍可手动重启——磁盘文件已就位）。
func createBreakawayProcess(cmdLine string) error {
	argv16, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return err
	}
	var si windows.StartupInfo
	si.Flags = windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	flags := uint32(windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_NO_WINDOW)
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, argv16, nil, nil, false, flags, nil, nil, &si, &pi); err != nil {
		if err := windows.CreateProcess(nil, argv16, nil, nil, false, windows.CREATE_NO_WINDOW, nil, nil, &si, &pi); err != nil {
			return err
		}
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}

// restartPSQuote PowerShell 单引号字面量（内部单引号加倍）。
func restartPSQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
