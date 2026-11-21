//go:build darwin

package main

// macOS Seatbelt 沙箱的行为测试：三个档位边界各取一个代表场景——
// workspace-write 写工作区（放行）/ 写工作区外（拒绝）/ read-only 写工作区
// （同样拒绝）。直接调 spawnConfinedDarwin，不起 App、不碰真实工作区，
// 用 t.TempDir() 的隔离目录。
//
// 环境敏感说明：依赖系统 sandbox-exec（macOS 自带）。拒绝形态断言用
// 「退出码非零 + 输出含 Operation not permitted」双门，只按返回文本断言，
// 不依赖具体 shell 的报错措辞。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runDarwinSbx 跑一条受限命令，返回合并输出与 err。
func runDarwinSbx(t *testing.T, policy, ws, dir, command string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return spawnConfinedDarwin(ctx, sandboxRun{
		Dir: dir, Command: command, Policy: policy, Workspace: ws, Timeout: 30 * time.Second,
	})
}

func TestSandboxDarwinWorkspaceWriteAllowsWorkspace(t *testing.T) {
	ws := t.TempDir()
	out, err := runDarwinSbx(t, permWorkspace, ws, ws,
		"echo sandbox-ok > "+shellQuote(filepath.Join(ws, "落盘.txt"))+" && cat "+shellQuote(filepath.Join(ws, "落盘.txt")))
	if err != nil {
		t.Fatalf("工作区内写入应成功，err=%v out=%s", err, out)
	}
	if !strings.Contains(out, "sandbox-ok") {
		t.Fatalf("回读内容异常: %q", out)
	}
	if _, serr := os.Stat(filepath.Join(ws, "落盘.txt")); serr != nil {
		t.Fatalf("文件应真实落盘: %v", serr)
	}
}

func TestSandboxDarwinWorkspaceWriteDeniesOutside(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "外泄.txt") // 另一个临时目录：工作区外
	out, err := runDarwinSbx(t, permWorkspace, ws, ws,
		"echo leak > "+shellQuote(outside))
	if err == nil && !strings.Contains(strings.ToLower(out), "operation not permitted") {
		t.Fatalf("工作区外写入应被拒: out=%q err=%v", out, err)
	}
	if _, serr := os.Stat(outside); serr == nil {
		t.Fatal("工作区外文件不应存在（写入被内核拒绝）")
	}
	// 读不受限：工作区外的文件应能正常读
	probe := filepath.Join(t.TempDir(), "读我.txt")
	if werr := os.WriteFile(probe, []byte("readable"), 0o644); werr != nil {
		t.Fatalf("准备探针文件失败: %v", werr)
	}
	out2, err2 := runDarwinSbx(t, permWorkspace, ws, ws, "cat "+shellQuote(probe))
	if err2 != nil || !strings.Contains(out2, "readable") {
		t.Fatalf("只限写不限读：读取工作区外文件应成功: out=%q err=%v", out2, err2)
	}
}

func TestSandboxDarwinReadOnlyDeniesWorkspace(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "只读档.txt")
	out, err := runDarwinSbx(t, permReadOnly, ws, ws,
		"echo x > "+shellQuote(target))
	if err == nil && !strings.Contains(strings.ToLower(out), "operation not permitted") {
		t.Fatalf("read-only 档位写工作区应被拒: out=%q err=%v", out, err)
	}
	if _, serr := os.Stat(target); serr == nil {
		t.Fatal("read-only 档位不应有文件落盘")
	}
}
