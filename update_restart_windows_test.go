//go:build windows

package main

// update_restart_windows_test.go —— 重启接力命令的形态单测：等约 8 秒再
// 拉起、连试 3 次（2026-11-20 事故加固，竞态兜底见 restartRelayCmd 注释）。
// WMI/接力真链路不做单测（要真进程退出），由装机验证覆盖。

import (
	"strings"
	"testing"
)

func TestRestartRelayCmdRetries(t *testing.T) {
	cmd := restartRelayCmd(`D:\装目录\AI数字助理.exe`, []string{"-desktop"}, `D:\装目录\data\updates\restart.log`)
	if got := strings.Count(cmd, `ping -n 9 127.0.0.1 >nul`); got != 3 {
		t.Errorf("等待计时应出现 3 次（首发 1 + 重试间隔 2），got %d\ncmd:\n%s", got, cmd)
	}
	if got := strings.Count(cmd, `start ""`); got != 3 {
		t.Errorf("拉起应连试 3 次，got %d\ncmd:\n%s", got, cmd)
	}
	if !strings.Contains(cmd, `relay: launching D:\装目录\AI数字助理.exe`) {
		t.Errorf("缺留痕行\ncmd:\n%s", cmd)
	}
	// 拉起必须带工作目录与启动参数
	for i, want := range []string{`start "" /d "D:\装目录" "D:\装目录\AI数字助理.exe" -desktop`} {
		if c := strings.Count(cmd, want); c != 3 {
			t.Errorf("第 %d 个拉起片段应出现 3 次，got %d：%q\ncmd:\n%s", i, c, want, cmd)
		}
	}
	// 顺序：等待在最前、留痕在第一次拉起之前
	if strings.Index(cmd, `ping -n 9`) > strings.Index(cmd, `relay: launching`) {
		t.Errorf("应先等待再留痕\ncmd:\n%s", cmd)
	}
}
