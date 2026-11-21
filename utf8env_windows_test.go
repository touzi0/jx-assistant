//go:build windows

package main

// buildShellCmdline（PS 5.1 编码注入）的纯函数级测试——测的函数本体在
// sandbox_windows.go，仅 Windows 参与编译；其余编码相关测试在跨平台的
// utf8env_test.go。

import (
	"strings"
	"testing"
)

func TestBuildShellCmdlinePs51Prefix(t *testing.T) {
	if !isWindowsRuntime {
		t.Skip("buildShellCmdline 仅 Windows")
	}
	// 空 shell / 显式 powershell：前缀在场；pwsh / cmd：原样
	for _, shell := range []string{"", "powershell", "PowerShell"} {
		_, cmdline, ps51, err := buildShellCmdline(shell, "echo ok")
		if err != nil {
			t.Fatalf("shell=%q 解析失败: %v", shell, err)
		}
		if !ps51 {
			t.Fatalf("shell=%q 应判定为 PS 5.1 路径", shell)
		}
		if !strings.Contains(cmdline, "$OutputEncoding") {
			t.Fatalf("shell=%q 命令行应含 UTF-8 前置设置:\n%s", shell, cmdline)
		}
		if !strings.Contains(cmdline, "echo ok") {
			t.Fatalf("shell=%q 原命令被破坏:\n%s", shell, cmdline)
		}
	}
	_, cmdLine2, ps51Cmd, err := buildShellCmdline("cmd", "echo ok")
	if err != nil || ps51Cmd || strings.Contains(cmdLine2, "$OutputEncoding") {
		t.Fatalf("cmd 不应注入: ps51=%v err=%v cmdline=%s", ps51Cmd, err, cmdLine2)
	}
	if _, _, _, err := buildShellCmdline("pwsh", "echo ok"); err != nil {
		// 本机没装 pwsh 时 LookPath 失败属预期：ps51 判定逻辑无法经此验证，跳过
		if !strings.Contains(err.Error(), "pwsh") {
			t.Fatalf("pwsh 解析错误异常: %v", err)
		}
		t.Log("本机未装 pwsh，跳过 pwsh 分支断言")
	} else if _, cmdline, ps51, _ := buildShellCmdline("pwsh", "echo ok"); ps51 || strings.Contains(cmdline, "$OutputEncoding") {
		t.Fatalf("pwsh 不应注入: %s", cmdline)
	}
}
