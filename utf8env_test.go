package main

// 编码环境注入与 write/edit 回读注记的纯函数级测试（零环境敏感：不起子
// 进程、不碰真实沙箱——那条路历史上就环境敏感，真执行类用例已随
// 9b4e43f282 归档）。PS 5.1 命令行注入的测试在 utf8env_windows_test.go
//（buildShellCmdline 仅 Windows 参与编译）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendEnvUnique(t *testing.T) {
	env := []string{"PATH=C:\\bin", "PYTHONUTF8=0", "TEMP=C:\\tmp"}
	out := appendEnvUnique(env, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8")
	var pyUTF8, pyIO int
	for _, kv := range out {
		switch {
		case strings.EqualFold(kv, "PYTHONUTF8=1"):
			pyUTF8++
		case strings.EqualFold(kv, "PYTHONUTF8=0"):
			t.Fatalf("旧值应被摘除: %s", kv)
		case strings.HasPrefix(strings.ToUpper(kv), "PYTHONIOENCODING="):
			pyIO++
		}
	}
	if pyUTF8 != 1 || pyIO != 1 {
		t.Fatalf("注入应恰好一份: PYTHONUTF8×%d PYTHONIOENCODING×%d", pyUTF8, pyIO)
	}
	if len(out) != 4 { // PATH、TEMP + 两条注入（旧 PYTHONUTF8=0 被摘）
		t.Fatalf("长度应为 4，实得 %d: %v", len(out), out)
	}
	if appendEnvUnique(env) == nil || len(appendEnvUnique(env)) != len(env) {
		t.Fatal("零注入应原样返回")
	}
}

func TestReadBackNote(t *testing.T) {
	cases := []struct {
		name         string
		wrote        string
		readBack     string
		verified     bool
		withFirst    bool
		wantContains []string
		wantAbsent   []string
	}{
		{"一致带首行", "第一行中文\nsecond", "第一行中文\nsecond", true, true,
			[]string{"回读一致", "首行：第一行中文"}, nil},
		{"一致不带首行", "a\nb", "a\nb", true, false,
			[]string{"回读一致"}, []string{"首行"}},
		{"不一致", "abc", "abd", true, true,
			[]string{"⚠ 回读与写入不一致", "写 3 读 3 字节"}, []string{"回读一致"}},
		{"长度不一致", "abcdef", "ab", true, false,
			[]string{"写 6 读 2 字节"}, nil},
		{"回读多出BOM", "x", "\xef\xbb\xbfx", true, true,
			[]string{"回读与写入不一致", "回读带 UTF-8 BOM"}, nil},
		{"未校验", "x", "", false, true,
			[]string{"（未回读校验）"}, []string{"回读一致", "⚠"}},
	}
	for _, c := range cases {
		note := readBackNote([]byte(c.wrote), []byte(c.readBack), c.verified, c.withFirst)
		for _, want := range c.wantContains {
			if !strings.Contains(note, want) {
				t.Fatalf("%s: 注记应含 %q，实得 %q", c.name, want, note)
			}
		}
		for _, absent := range c.wantAbsent {
			if strings.Contains(note, absent) {
				t.Fatalf("%s: 注记不应含 %q，实得 %q", c.name, absent, note)
			}
		}
	}
}

func TestFirstLineSnippet(t *testing.T) {
	if got := firstLineSnippet([]byte("  你好 world  \r\n第二行"), 60); got != "你好 world" {
		t.Fatalf("CRLF 首行应归一去空白: %q", got)
	}
	long := strings.Repeat("字", 80)
	got := firstLineSnippet([]byte(long), 60)
	if want := strings.Repeat("字", 60) + "…"; got != want {
		t.Fatalf("超长应截 60 字符加省略号: len=%d", len([]rune(got)))
	}
	if got := firstLineSnippet(nil, 60); got != "（空文件）" {
		t.Fatalf("空文件占位: %q", got)
	}
}

// TestWriteReadBackNote 集成断言：write 成功文案带"回读一致，首行"
// （沿用 TestWriteJxAITmpObservationExemption 的 toolContext 直调手法）。
func TestWriteReadBackNote(t *testing.T) {
	a := newCapTestApp(t, "data-readback-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "write"}

	// 新文件不走 .jxai/tmp（系统目录不做快照、也就没有回读注记可断言）
	target := filepath.Join(ws, "回读验证.txt")
	out, err := runWriteFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"中文首行内容\nsecond line"}`, target)))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(out, "回读一致") || !strings.Contains(out, "首行：中文首行内容") {
		t.Fatalf("成功文案应带回读注记: %q", out)
	}
	// 盘上确实是对的字节（本地写即所得，writeFileAtomic 直写 UTF-8）
	data, rerr := os.ReadFile(target)
	if rerr != nil || !bytes.HasPrefix(data, []byte("中文首行内容")) {
		t.Fatalf("盘上内容异常: %v", rerr)
	}
}
