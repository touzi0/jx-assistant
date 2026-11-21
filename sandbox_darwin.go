//go:build darwin

package main

// macOS 沙箱后端：Seatbelt（sandbox-exec + SBPL profile）。
//
// 与 Windows 受限令牌+ACL、Linux Landlock 同一语义：读与联网不受限，
// 写只放行工作区与本次运行的私有临时目录（read-only 档位则全盘只读）。
// 机制：`sandbox-exec -p <profile> /bin/sh -c 'cd … && { 命令 ;}'`，
// profile 经内核对整棵进程树生效（孙进程同样受限），进程组终止沿用
// proctree_other.go（Setpgid + 杀 -pgid）。
//
// 已知边界（与官方 DSH 同款取舍，另见 sandbox_windows.go 头注）：
//   · sandbox-exec 是系统二进制（/usr/bin），Apple 标注为非公开 API、
//     未来 macOS 可能收紧：找不到/起不来时如实报「沙箱执行器故障」，
//     不降级为不受限执行（fail closed）；
//   · profile 按字面路径匹配：工作区/临时目录同时放行「原路径」与
//     「符号链接解析后路径」（/tmp → /private/tmp 两种写法都可能出现在
//     进程的 open 调用里，只放一种会被内核按另一种判拒）；
//   · 与 Windows 版相同：只限文件写，不挡读与网络。
//
// 环境变量：TMPDIR/TMP/TEMP 统一重指到私有临时目录——macOS 的 mktemp、
// python tempfile 默认写 $TMPDIR（/var/folders/…），不重指的话命令的
// 合法临时文件会被「全盘禁写」误伤。

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// seatbeltEscape SBPL 字符串字面量转义（反斜杠与双引号；UTF-8 原样保留）。
func seatbeltEscape(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	return strings.ReplaceAll(p, `"`, `\"`)
}

// seatbeltAllowWrite 生成一行「放行写入」规则（subpath：目录树内全部路径）。
func seatbeltAllowWrite(p string) string {
	return `(allow file-write* (subpath "` + seatbeltEscape(p) + `"))` + "\n"
}

// seatbeltPaths 返回 p 的「原路径 + 符号链接解析路径」去重列表。
func seatbeltPaths(p string) []string {
	out := []string{p}
	if resolved, err := filepath.EvalSymlinks(p); err == nil && resolved != p {
		out = append(out, resolved)
	}
	return out
}

// seatbeltProfile 生成 SBPL：默认全放行（读/联网/进程操作不受限）→
// 全盘禁写 → 按档位放行写入点。SBPL 后匹配的规则生效，deny 在前、
// allow 在后即构成白名单（Chromium 的 macOS 渲染进程沙箱同款手法）。
func seatbeltProfile(policy, workspace, tempDir string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString(`(deny file-write* (subpath "/"))` + "\n")
	// 设备写入点：/dev/null 是 shell 重定向的标准去处，dtrace 通道是部分
	// 命令行工具握手要写的桩——都是「只进不出」的槽，放行不构成落盘面。
	for _, dev := range []string{"/dev/null", "/dev/dtrace", "/dev/dtrace/helper"} {
		b.WriteString(`(allow file-write* (literal "` + seatbeltEscape(dev) + `"))` + "\n")
	}
	if policy != permReadOnly {
		if workspace != "" {
			for _, p := range seatbeltPaths(workspace) {
				b.WriteString(seatbeltAllowWrite(p))
			}
		}
		if tempDir != "" {
			for _, p := range seatbeltPaths(tempDir) {
				b.WriteString(seatbeltAllowWrite(p))
			}
		}
	}
	return b.String()
}

// confinedDarwinCmd 组装 sandbox-exec 包装的受限命令（同步/异步共用）。
func confinedDarwinCmd(ctx context.Context, r sandboxRun, tempDir string) (*exec.Cmd, error) {
	sbx, err := exec.LookPath("sandbox-exec")
	if err != nil {
		return nil, fmt.Errorf("找不到 sandbox-exec: %w", err)
	}
	profile := seatbeltProfile(r.Policy, r.Workspace, tempDir)
	wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
	cmd := exec.CommandContext(ctx, sbx, "-p", profile, "/bin/sh", "-c", wrapped)
	cmd.Dir = r.Dir
	cmd.Env = appendEnvUnique(sandboxEnv(false),
		"TMPDIR="+tempDir, "TMP="+tempDir, "TEMP="+tempDir)
	return cmd, nil
}

// spawnConfinedDarwin 在 Seatbelt 沙箱内执行一条命令（同步、合并输出）。
func spawnConfinedDarwin(ctx context.Context, r sandboxRun) (string, error) {
	tempDir, err := os.MkdirTemp("", "jx-sbx-")
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: 创建私有临时目录失败: %w", err)
	}
	defer os.RemoveAll(tempDir)
	cmd, err := confinedDarwinCmd(ctx, r, tempDir)
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: %w", err)
	}
	tree := armTreeKill(cmd)
	out, err := cmd.CombinedOutput()
	tree.finish()
	note := ""
	if len(out) > 64*1024 {
		out = out[:64*1024]
		note += "\n[输出过长已截断]"
	}
	res := strings.TrimRight(decodeCmdOutput(out), "\r\n") + note
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res += fmt.Sprintf("\n[exit code %d]", ee.ExitCode())
		} else {
			return "", fmt.Errorf("sandbox unavailable: 启动受限进程失败: %w", err)
		}
	}
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}

// startConfinedDarwinAsync 在 Seatbelt 沙箱内启动后台作业：输出增量写 w，
// ctx 取消时整进程组终止。返回等待函数（作业侧关 done）。
func startConfinedDarwinAsync(ctx context.Context, r sandboxRun, w io.Writer) (func() bool, error) {
	tempDir, err := os.MkdirTemp("", "jx-sbx-")
	if err != nil {
		return nil, fmt.Errorf("sandbox unavailable: 创建私有临时目录失败: %w", err)
	}
	cmd, err := confinedDarwinCmd(ctx, r, tempDir)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: %w", err)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	tree := armTreeKill(cmd)
	if err := cmd.Start(); err != nil {
		tree.finish()
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("sandbox unavailable: %w", err)
	}
	tree.attach(cmd)
	waitDone := make(chan struct{}, 1)
	go func() { _ = cmd.Wait(); tree.finish(); os.RemoveAll(tempDir); close(waitDone) }()
	return func() bool { <-waitDone; return true }, nil
}

// darwinDenialSigs Seatbelt 沙箱的拒绝方言：EPERM 在 macOS shell 里报
// "Operation not permitted"（Landlock 侧是 EACCES 的 "Permission denied"）。
// 其余沿用 posix 列表；收窄原则同 winDenialSigs 的注：不收泛 Unix 错误
// 字样，避免普通程序错误被标成「策略拒绝」把模型引向申请升级。
var darwinDenialSigs = append([]string{"operation not permitted"}, posixDenialSigs...)
