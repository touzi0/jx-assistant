package main

// 编码探测 / 写回语义 / 只读判定 / 乱码检测的测试。
//
// 纯函数用例零环境敏感；工具级用例只碰临时文件（不起子进程、不碰真实沙箱）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("构造 GBK 样本失败: %v", err)
	}
	return b
}

func TestDetectTextEncoding(t *testing.T) {
	utf16le := []byte{0xFF, 0xFE, 'a', 0x00, 'b', 0x00}
	cases := []struct {
		name string
		b    []byte
		want TextEncoding
	}{
		{"空文件按 UTF-8", nil, EncUTF8},
		{"纯 ASCII", []byte("hello\n"), EncUTF8},
		{"UTF-8 中文", []byte("中文\n第二行"), EncUTF8},
		{"UTF-8 带 BOM", append([]byte{0xEF, 0xBB, 0xBF}, []byte("中文")...), EncUTF8BOM},
		{"GBK 中文", gbkBytes(t, "中文测试\n"), EncGBK},
		{"UTF-16LE 带 BOM", utf16le, EncUTF16LE},
		{"二进制含 NUL", []byte{0x00, 0x01, 0x02, 0x03}, EncBinary},
		{"非法字节", []byte{0xFF, 0xFF, 0xFE, 0x01}, EncUnknown},
	}
	for _, c := range cases {
		if got := detectTextEncoding(c.b); got != c.want {
			t.Fatalf("%s: 期望 %v，实得 %v", c.name, c.want, got)
		}
	}
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	for _, s := range []string{"第一行中文\n第二行 English 混排：；！", "只有 ASCII"} {
		raw := gbkBytes(t, s)
		text, enc := decodeText(raw)
		if enc != EncGBK {
			t.Fatalf("应判为 GBK，实得 %v", enc)
		}
		if text != s {
			t.Fatalf("解码不一致: %q != %q", text, s)
		}
		back, err := encodeText(text, enc)
		if err != nil {
			t.Fatalf("GBK 编回失败: %v", err)
		}
		if string(back) != string(raw) {
			t.Fatalf("编回字节与原文不一致（写回会改动原文件）")
		}
	}
}

// 关键语义：GBK 编不出的字符必须报错，绝不静默替换成 '?'——那是有损丢字。
func TestEncodeGBKRejectsEmoji(t *testing.T) {
	if _, err := encodeText("中文😀", EncGBK); err == nil {
		t.Fatal("emoji 写 GBK 应报错")
	}
	if _, err := encodeText("中文😀", EncUTF8); err != nil {
		t.Fatalf("UTF-8 不该拒绝 emoji: %v", err)
	}
}

func TestLooksMojibake(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"正常中文：销售订单与采购订单的执行情况", false},
		{"混排 English 与数字 123，标点：；！", false},
		{"閿€鍞崟鍒楄〃", true},                  // 典型 UTF-8→GBK 误码
		{"hello \uFFFD world", true},        // 有损解码的替换符
		{"这里有一个字：銆", false},              // 单个特征字不误伤
		{"銆锛", true},                        // 两个特征字才判乱码
		{"中文 English 混排，偶尔出现鏄字", false},
	}
	for _, c := range cases {
		if got := looksMojibake(c.s); got != c.want {
			t.Fatalf("%q: 期望 %v，实得 %v", c.s, c.want, got)
		}
	}
}

func TestExtractPathCandidates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gbkFile := filepath.Join(dir, "旧脚本.bat")
	if err := os.WriteFile(gbkFile, gbkBytes(t, "echo 中文\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`Get-Content "a.txt"`,
		`Select-String -Pattern 'x' -Path 'a.txt'`,
		`type a.txt`,
		`Get-Content ` + a,
		"Get-Content '旧脚本.bat'",
	} {
		got := extractPathCandidates(cmd, dir)
		if len(got) == 0 {
			t.Fatalf("%q: 应抓到至少一个候选路径", cmd)
		}
		for _, p := range got {
			if !filepath.IsAbs(p) {
				t.Fatalf("%q: 候选应是绝对路径，实得 %q", cmd, p)
			}
		}
	}
	// 不存在的文件、通配符、纯参数都不该被抓
	if got := extractPathCandidates(`Get-Content -Raw 不存在.txt *.log`, dir); len(got) != 0 {
		t.Fatalf("应抓不到候选，实得 %v", got)
	}
}

func TestIsReadOnlyCommand(t *testing.T) {
	readOnly := []string{
		`Get-Content a.txt`,
		`Select-String -Pattern 中文 -Path a.txt`,
		`git log --oneline -5`,
		`git status; git diff`,
		`dir; Get-ChildItem`,
		`type 旧脚本.bat`,
		`Get-Content a.txt | Select-Object -First 10`,
		`Get-Content a.txt 2>&1`, // 句柄复制不是写文件
	}
	for _, c := range readOnly {
		if !isReadOnlyCommand(c) {
			t.Fatalf("%q 应判为只读", c)
		}
	}
	notReadOnly := []string{
		`Set-Content b.txt "x"`,
		`Get-Content a.txt; Set-Content b.txt x`,
		`Get-Content a.txt > b.txt`,
		`git commit -m "x"`,
		`git push origin main`,
		`Remove-Item a.txt`,
		`go build ./...`,
		`npm install`,
		`Start-Process notepad`,
		`echo hi >> log.txt`,
	}
	for _, c := range notReadOnly {
		if isReadOnlyCommand(c) {
			t.Fatalf("%q 不该判为只读（重跑会执行两遍）", c)
		}
	}
}

func TestPs51PrologueModes(t *testing.T) {
	utf8 := ps51Prologue("")
	for _, want := range []string{
		"$OutputEncoding", "[Console]::OutputEncoding",
		"$PSDefaultParameterValues['Get-Content:Encoding']='utf8'",
		"$PSDefaultParameterValues['Select-String:Encoding']='utf8'",
		"$PSDefaultParameterValues['Import-Csv:Encoding']='utf8'",
	} {
		if !strings.Contains(utf8, want) {
			t.Fatalf("UTF-8 前置块应含 %q:\n%s", want, utf8)
		}
	}
	gbk := ps51Prologue(encodingModeGBK)
	for _, want := range []string{
		"GetEncoding(936)",
		"$PSDefaultParameterValues['Get-Content:Encoding']='Default'",
		"$PSDefaultParameterValues['Select-String:Encoding']='Default'",
	} {
		if !strings.Contains(gbk, want) {
			t.Fatalf("GBK 前置块应含 %q:\n%s", want, gbk)
		}
	}
	if strings.Contains(utf8, "Out-File:Encoding") || strings.Contains(gbk, "Out-File:Encoding") {
		t.Fatal("写面默认编码不该注入（5.1 的 -Encoding utf8 会写 BOM，写坏 .bat/.cmd）")
	}
}

func TestDetectEncodingMode(t *testing.T) {
	dir := t.TempDir()
	utf8File := filepath.Join(dir, "u.sql")
	gbkFile := filepath.Join(dir, "g.bat")
	if err := os.WriteFile(utf8File, []byte("select 中文 from t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gbkFile, gbkBytes(t, "echo 中文\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "u.sql"`, ""); m != encodingModeUTF8 {
		t.Fatalf("UTF-8 文件应走 utf8，实得 %q", m)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "g.bat"`, ""); m != encodingModeGBK {
		t.Fatalf("GBK 文件应走 gbk，实得 %q", m)
	}
	if m, hint := detectEncodingMode(dir, `Get-Content "u.sql" ; Get-Content "g.bat"`, ""); m != encodingModeUTF8 || hint == "" {
		t.Fatalf("混合编码应按 utf8 并给出提示，实得 %q / %q", m, hint)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "u.sql"`, "cmd"); m != encodingModeUTF8 {
		t.Fatalf("cmd 路径不做注入，应返回 utf8，实得 %q", m)
	}
}

// TestPs51PrologueLive 是唯一真跑 PowerShell 的用例：验证新注入块在
// -Command 环境下确实生效（读面默认 UTF-8 / GBK 模式对照）。
// 默认跳过（环境敏感），显式设置 JX_ENC_LIVE=1 才执行。
func TestPs51PrologueLive(t *testing.T) {
	if os.Getenv("JX_ENC_LIVE") != "1" || !isWindowsRuntime {
		t.Skip("设置 JX_ENC_LIVE=1 且在 Windows 上才跑真实 PowerShell 编码用例")
	}
	dir := t.TempDir()
	u8 := filepath.Join(dir, "u8.txt")
	gb := filepath.Join(dir, "g.bat")
	if err := os.WriteFile(u8, []byte("第一行中文（UTF-8 无 BOM）\r\n第二行：销售订单与采购订单\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gb, gbkBytes(t, "@echo off\r\necho 中文批处理：执行情况\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPS := func(mode, cmd string) string {
		t.Helper()
		c := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps51Prologue(mode)+cmd)
		c.Env = appendEnvUnique(os.Environ(), ps51Utf8Env()...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("powershell 执行失败（mode=%s）: %v\n%s", mode, err, decodeCmdOutput(out))
		}
		return decodeCmdOutput(out)
	}
	// 1) 注入块本身生效：读面默认值确实被设上
	if s := runPS("", `$PSDefaultParameterValues['Get-Content:Encoding']`); !strings.Contains(s, "utf8") {
		t.Fatalf("读面默认编码未生效，实际: %q", s)
	}
	// 2) UTF-8 模式读 UTF-8 无 BOM 文件：正确、无乱码
	if s := runPS("", `Get-Content -LiteralPath '`+u8+`'`); !strings.Contains(s, "第二行：销售订单与采购订单") || looksMojibake(s) {
		t.Fatalf("UTF-8 模式读 UTF-8 文件应正确，实际:\n%s", s)
	}
	// 3) GBK 模式读 GBK 文件：正确
	if s := runPS(encodingModeGBK, `Get-Content -LiteralPath '`+gb+`'`); !strings.Contains(s, "中文批处理：执行情况") {
		t.Fatalf("GBK 模式读 GBK 文件应正确，实际:\n%s", s)
	}
	// 4) 对照：UTF-8 模式读 GBK 文件必须解不对（证明第 2/3 步不是"碰巧都对"）
	if s := runPS("", `Get-Content -LiteralPath '`+gb+`'`); strings.Contains(s, "中文批处理：执行情况") {
		t.Fatalf("对照组不应正确解出中文，实际:\n%s", s)
	}
}

// ---------- 工具级：read 解码 + edit 保持原编码 ----------

func TestReadFileDecodesGBK(t *testing.T) {
	a := newCapTestApp(t, "data-enc-read-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "read"}

	target := filepath.Join(ws, "gbk读取验证.txt")
	if err := os.WriteFile(target, gbkBytes(t, "第一行中文\n第二行内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q}`, target)))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "第一行中文") || !strings.Contains(out, "第二行内容") {
		t.Fatalf("GBK 文件应解码为正确中文，实得: %q", out)
	}
	if !strings.Contains(out, "GBK") || !strings.Contains(out, "原编码写回") {
		t.Fatalf("应注明原编码与写回语义，实得: %q", out)
	}
}

func TestEditPreservesGBKEncoding(t *testing.T) {
	a := newCapTestApp(t, "data-enc-edit-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "edit"}

	target := filepath.Join(ws, "gbk编辑验证.bat")
	orig := gbkBytes(t, "@echo off\r\necho 中文第一行\r\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文第一行","new_string":"echo 中文已改过"}`, target)))
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	if !strings.Contains(out, "保持原 GBK 编码写回") {
		t.Fatalf("成功文案应注明保持原编码，实得: %q", out)
	}
	raw, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := detectTextEncoding(raw); got != EncGBK {
		t.Fatalf("写回后应仍是 GBK，实得 %v", got)
	}
	text, _ := decodeText(raw)
	if !strings.Contains(text, "echo 中文已改过") || !strings.Contains(text, "@echo off") {
		t.Fatalf("写回内容不正确: %q", text)
	}
	// 连续第二次编辑：验证幂等，不会因转码叠加而变乱
	if _, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文已改过","new_string":"echo 中文再改"}`, target))); err != nil {
		t.Fatalf("第二次编辑失败: %v", err)
	}
	raw2, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw2); got != EncGBK {
		t.Fatalf("第二次写回后应仍是 GBK，实得 %v", got)
	}
	if text2, _ := decodeText(raw2); !strings.Contains(text2, "echo 中文再改") {
		t.Fatalf("第二次写回内容不正确: %q", text2)
	}
}

func TestEditGBKRejectsUnencodable(t *testing.T) {
	a := newCapTestApp(t, "data-enc-edit-reject-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "edit"}

	target := filepath.Join(ws, "gbk不可写验证.bat")
	orig := gbkBytes(t, "@echo off\r\necho 中文\r\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文","new_string":"echo 😀"}`, target)))
	if err == nil {
		t.Fatal("emoji 写入 GBK 文件应报错")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != string(orig) {
		t.Fatal("报错时文件不应被改动")
	}
}

func TestWriteWarnsWhenOverwritingGBK(t *testing.T) {
	a := newCapTestApp(t, "data-enc-write-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "write"}

	target := filepath.Join(ws, "覆盖提示验证.bat")
	if err := os.WriteFile(target, gbkBytes(t, "echo 旧\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runReadFile(&toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "read"},
		json.RawMessage(fmt.Sprintf(`{"path":%q}`, target))); err != nil {
		t.Fatalf("先读观察失败: %v", err)
	}
	out, err := runWriteFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"echo new\n"}`, target)))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(out, "原为 GBK 编码") || !strings.Contains(out, "UTF-8") {
		t.Fatalf("覆盖 GBK 文件应提示编码变化，实得: %q", out)
	}
	raw, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw); got != EncUTF8 {
		t.Fatalf("write 后应为 UTF-8，实得 %v", got)
	}
}

// ---------- H1：截断的字符边界（2026-11-21 修复的回归测试）----------
//
// 背景：缓冲超限时从头部按字节丢、或解码前按字节切尾，都可能落在多字节字符中间。
// utf8.Valid 因此失败 → 整段被当成 GBK 解 → **整段乱码**（0x80-0xBF 恰在 GBK 第二
// 字节的合法区间，结构校验还会通过，所以不会报错、只会烂）。

func TestAlignHeadToCharBoundary(t *testing.T) {
	// UTF-8：丢掉 1 字节后落在"中"的续字节上 → 必须再多丢 2 字节，让剩余是合法 UTF-8
	utf8Buf := []byte("中文中文中文")
	got := alignHeadToCharBoundary(utf8Buf, 1)
	if got == 1 {
		t.Fatalf("落在续字节上应当对齐，实得 %d", got)
	}
	if !utf8Valid(utf8Buf[got:]) {
		t.Fatalf("对齐后应为合法 UTF-8，实得起点 %d", got)
	}
	if got > 1+maxRuneBytes {
		t.Fatalf("对齐不该多丢超过 %d 字节，实得多丢 %d", maxRuneBytes, got-1)
	}

	// 已经是字符边界：原样返回（不能白丢字节）
	if got := alignHeadToCharBoundary(utf8Buf, 3); got != 3 {
		t.Fatalf("已是边界应原样返回，实得 %d", got)
	}

	// GBK 流：跳续字节也成不了合法 UTF-8 → 必须原样返回，绝不破坏既有行为
	gbk := gbkBytes(t, "中文测试中文测试中文测试")
	if got := alignHeadToCharBoundary(gbk, 3); got != 3 {
		t.Fatalf("GBK 流不该被动过，实得 %d", got)
	}
}

func TestTrimIncompleteUTF8Tail(t *testing.T) {
	full := []byte("中文测试")
	for cut := 1; cut <= 3; cut++ { // 1~3 字节的残缺尾巴都要能收口
		partial := full[:len(full)-cut]
		got := trimIncompleteUTF8Tail(partial)
		if !utf8Valid(got) {
			t.Fatalf("切掉 %d 字节后应收口成合法 UTF-8", cut)
		}
		if len(partial)-len(got) > maxRuneBytes {
			t.Fatalf("收口不该多丢超过 %d 字节，实得 %d", maxRuneBytes, len(partial)-len(got))
		}
	}
	if got := trimIncompleteUTF8Tail(full); len(got) != len(full) {
		t.Fatalf("本来就合法，不该被动：%d → %d", len(full), len(got))
	}
	// GBK 流：本来就是"非法 UTF-8"，但怎么切尾都成不了合法 UTF-8 → 必须原样保留
	gbk := gbkBytes(t, "中文测试中文测试")
	if got := trimIncompleteUTF8Tail(gbk); string(got) != string(gbk) {
		t.Fatal("GBK 流不该被切尾（切了反而制造新乱码）")
	}
}

func TestClipUTF8HeadTail(t *testing.T) {
	s := "中文中文中文" // 每字 3 字节，共 18
	for n := 1; n < len(s); n++ {
		if got := clipUTF8Tail(s, n); !utf8Valid([]byte(got)) || len(got) > n {
			t.Fatalf("clipUTF8Tail(%d) 不合格: %q", n, got)
		}
		if got := clipUTF8Head(s, n); !utf8Valid([]byte(got)) || len(got) > n {
			t.Fatalf("clipUTF8Head(%d) 不合格: %q", n, got)
		}
	}
	if got := clipUTF8Head(s, len(s)); got != s {
		t.Fatalf("不超过上限应原样返回，实得 %q", got)
	}
}

// H1 的端到端回归：后台作业缓冲超限时丢头部，输出仍须解成正确中文。
func TestJobBufferHeadTruncationKeepsText(t *testing.T) {
	var b lockedBuffer
	// 丢弃量必须落在字符中间（drop % 3 != 0），旧实现才会落在续字节上
	payload := strings.Repeat("中", (jobBufMax/3)+1)
	drop := len(payload) - jobBufMax
	if drop <= 0 || drop%3 == 0 {
		t.Fatalf("样本构造不合格：len=%d drop=%d", len(payload), drop)
	}
	// 机制（确定性）：错位起点不是合法 UTF-8 —— 这正是旧实现让整段落进 GB18030
	// 分支的原因；往前对齐 1 字节后就合法了。
	if utf8.Valid([]byte(payload[drop:])) || !utf8.Valid([]byte(payload[drop+1:])) {
		t.Fatalf("样本应满足「错位起点非法、对齐起点合法」（drop=%d）", drop)
	}
	if _, err := b.Write([]byte(payload)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	out := b.String()
	if !utf8Valid([]byte(out)) || strings.Contains(out, "\uFFFD") {
		t.Fatalf("截断后的输出必须是合法 UTF-8 且无替换符（旧实现会整段变乱码）:\n%q", out)
	}
	if strings.Contains(out, "涓") || strings.Contains(out, "锛") { // UTF-8 被当 GBK 解的典型误码字
		t.Fatalf("出现 UTF-8→GBK 误码字，说明整段被当 GBK 解了:\n%q", out)
	}
	if !strings.Contains(out, "中") {
		t.Fatalf("应保留中文内容，实得:\n%q", out)
	}
}

// ---------- 第 2 条：输出解码的代码页映射与显式覆盖 ----------

func TestConsoleCodePageDecoder(t *testing.T) {
	for _, cp := range []uint32{936, 54936, 950, 932, 949, 866, 1252, 437} {
		if consoleCodePageDecoder(cp) == nil {
			t.Fatalf("代码页 %d 应当有解码器", cp)
		}
	}
	if consoleCodePageDecoder(0) != nil || consoleCodePageDecoder(99999) != nil {
		t.Fatal("未收录的代码页应返回 nil（走 GB18030 兜底）")
	}
}

func TestOutputEncodingOverride(t *testing.T) {
	t.Setenv("JX_OUTPUT_ENCODING", "")
	if outputEncodingOverride() != nil {
		t.Fatal("未设置时不应有覆盖")
	}
	t.Setenv("JX_OUTPUT_ENCODING", "随便写错的值")
	if outputEncodingOverride() != nil {
		t.Fatal("认不出的取值应按未设置处理（不能因为变量名拼错就改变默认行为）")
	}
	t.Setenv("JX_OUTPUT_ENCODING", "cp932")
	if outputEncodingOverride() == nil {
		t.Fatal("cp932 应当被识别")
	}
}

func TestDecodeConsoleBytesOverrideAndDefault(t *testing.T) {
	// 默认（未设置覆盖）：GBK 流仍按中文解出——锁住"中文环境行为不变"
	t.Setenv("JX_OUTPUT_ENCODING", "")
	gbk := gbkBytes(t, "中文输出")
	if got := string(decodeConsoleBytes(gbk)); !strings.Contains(got, "中文输出") {
		t.Fatalf("GBK 流默认应解出中文，实得 %q", got)
	}
	// 本来就是 UTF-8：不动
	utf8In := []byte("中文输出")
	if got := string(decodeConsoleBytes(utf8In)); got != string(utf8In) {
		t.Fatalf("UTF-8 流应当原样返回，实得 %q", got)
	}
	// 显式覆盖成 cp932：日文流按日文解（这是第 2 条的新能力）
	sjis := []byte{0x93, 0xFA, 0x96, 0x7B, 0x8C, 0xEA} // "日本語" 的 Shift_JIS
	t.Setenv("JX_OUTPUT_ENCODING", "cp932")
	if got := string(decodeConsoleBytes(sjis)); !strings.Contains(got, "日本語") {
		t.Fatalf("覆盖为 cp932 后应解出日文，实得 %q", got)
	}
	// 覆盖值解不了这个流时，必须回落到常规路径而不是报错/给空
	t.Setenv("JX_OUTPUT_ENCODING", "cp437")
	if got := string(decodeConsoleBytes(gbk)); got == "" {
		t.Fatal("覆盖值解不了时应回落常规路径，不能返回空")
	}
}

func utf8Valid(b []byte) bool { return utf8.Valid(b) }
