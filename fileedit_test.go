package main

// 右栏「文件」编辑器（fileedit.go）的回归测试：GET 取全文 / POST 保存的契约与
// 护栏——工作区边界（防穿越）、只收文本（二进制 / 编码识别不了都拒绝）、2MB
// 上限、写前快照（历史版本可回退）、版本标识冲突保护（409 且不覆盖）、
// 行尾还原（CRLF 文件不被悄悄改成 LF）。
//
// 这里锁的是"用户在手改文件"这条链路的底线：宁可报错，也不能静默改坏别人的
// 文件或丢掉回退能力。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// panelTestServer 起一个只挂这两条路由的 mux（用真实路径参数匹配，不惊动整站注册）。
func panelTestServer(a *App) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions/{id}/file", a.handleFileContent)
	mux.HandleFunc("POST /api/sessions/{id}/file", a.handleFileSave)
	return mux
}

func panelGet(t *testing.T, h http.Handler, sid, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/sessions/"+sid+"/file?path="+urlEscape(path), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func panelPost(t *testing.T, h http.Handler, sid string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/sessions/"+sid+"/file", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func urlEscape(s string) string { return strings.NewReplacer(" ", "%20", "\\", "%5C").Replace(s) }

func panelTestApp(t *testing.T) (*App, *SessionMeta, string, http.Handler) {
	t.Helper()
	a := newCapTestApp(t, "data-panel-edit-test")
	ws := roTestWS(t)
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-11-16 21:00:00"}
	mustCreateSession(t, a, sess)
	return a, sess, ws, panelTestServer(a)
}

// 正常路径：取全文 → 保存 → 磁盘变了、快照留下来了（历史版本可回退到旧内容）
func TestPanelEditSaveLeavesSnapshot(t *testing.T) {
	a, sess, ws, h := panelTestApp(t)
	target := filepath.Join(ws, "demo.txt")
	if err := os.WriteFile(target, []byte("第一行\n第二行\n"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}

	code, got := panelGet(t, h, sess.ID, "demo.txt")
	if code != 200 || got["text"] != "第一行\n第二行\n" || got["hash"] == "" || got["lineEndings"] != "lf" {
		t.Fatalf("取全文应 200 且带全文与版本标识，得到 %d %v", code, got)
	}
	code, saved := panelPost(t, h, sess.ID, map[string]any{
		"path": "demo.txt", "content": "第一行\n第二行（改）\n", "hash": got["hash"], "lineEndings": "lf",
	})
	if code != 200 || saved["versioned"] != true {
		t.Fatalf("保存应 200 且落快照，得到 %d %v", code, saved)
	}
	if b, _ := os.ReadFile(target); string(b) != "第一行\n第二行（改）\n" {
		t.Fatalf("磁盘内容未更新: %q", string(b))
	}

	// 写前快照：tool_name=file_edit、seq=0（不锚定回合，不被按轮撤回带走）、
	// 正文是旧内容——正是「历史版本」能回退的依据
	cps, err := a.Store.CheckpointsForPath(sess.ID, "demo.txt")
	if err != nil || len(cps) != 1 {
		t.Fatalf("应落一条快照，得到 %d 条 err=%v", len(cps), err)
	}
	cp := cps[0]
	if cp.ToolName != uiEditTool || cp.Seq != 0 || !cp.ExistedBefore || cp.BeforeSize != int64(len("第一行\n第二行\n")) {
		t.Fatalf("快照元数据不对: %+v", cp)
	}
	tc := &toolContext{app: a, sess: sess}
	if body := tc.loadBefore(cp); string(body) != "第一行\n第二行\n" {
		t.Fatalf("快照正文应是旧内容，得到 %q", string(body))
	}
	// 既有回退链路对这个快照照常有效（checkpointId 口径）
	if safe, unsafe, _ := a.planRewindFor(tc, []*Checkpoint{cp}); len(unsafe) != 0 || len(safe) != 1 {
		t.Fatalf("回退计划应可用，得到 safe=%d unsafe=%d", len(safe), len(unsafe))
	}
}

// 冲突保护：磁盘内容与打开编辑时读到的不一致 → 409 且不覆盖
func TestPanelEditConflictDoesNotOverwrite(t *testing.T) {
	_, sess, ws, h := panelTestApp(t)
	target := filepath.Join(ws, "c.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	_, got := panelGet(t, h, sess.ID, "c.txt")
	// 外部程序改掉文件
	if err := os.WriteFile(target, []byte("别人改的\n"), 0o644); err != nil {
		t.Fatalf("外部修改失败: %v", err)
	}
	code, out := panelPost(t, h, sess.ID, map[string]any{
		"path": "c.txt", "content": "编辑器里的旧稿\n", "hash": got["hash"], "lineEndings": "lf",
	})
	if code != 409 {
		t.Fatalf("内容变了应 409，得到 %d %v", code, out)
	}
	wantAll(t, out["error"].(string), "冲突提示", "已被其他程序修改")
	if b, _ := os.ReadFile(target); string(b) != "别人改的\n" {
		t.Fatalf("409 时绝不能覆盖，磁盘现在是 %q", string(b))
	}
	// 缺版本标识：直接拒，避免"裸写"绕开冲突保护
	code, out = panelPost(t, h, sess.ID, map[string]any{"path": "c.txt", "content": "x\n"})
	if code != 400 {
		t.Fatalf("缺 hash/mtime 应 400，得到 %d %v", code, out)
	}
}

// 护栏：穿越 / 工作区外 / 二进制 / 超大 / .jxai 内部数据 / 文件夹
func TestPanelEditGuards(t *testing.T) {
	_, sess, ws, h := panelTestApp(t)
	writeFile := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(ws, name), b, 0o644); err != nil {
			t.Fatalf("预置 %s 失败: %v", name, err)
		}
	}
	writeFile("bin.dat", []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02})
	writeFile("big.txt", []byte(strings.Repeat("a", fileEditMaxBytes+1)))
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(ws, jxAIName, "snapshots"), 0o755); err != nil {
		t.Fatalf("建 .jxai 失败: %v", err)
	}
	writeFile(filepath.Join(jxAIName, "snapshots", "x.bin"), []byte("hi"))

	cases := []struct {
		path string
		want string
	}{
		{"../../etc/passwd", "工作区"},
		{"../outside.txt", "工作区"},
		{"bin.dat", "二进制"},
		{"big.txt", "太大"},
		{"sub", "文件夹"},
		{jxAIName + "/snapshots/x.bin", ".jxai"},
	}
	for _, c := range cases {
		code, out := panelGet(t, h, sess.ID, c.path)
		if code != 400 {
			t.Fatalf("%s 应被拒（400），得到 %d %v", c.path, code, out)
		}
		wantAll(t, out["error"].(string), "GET "+c.path, c.want)
	}
	// 保存侧同样挡穿越（外面的文件一个字都不能动）
	code, out := panelPost(t, h, sess.ID, map[string]any{
		"path": "../outside.txt", "content": "x", "hash": strings.Repeat("0", 64),
	})
	if code != 400 {
		t.Fatalf("保存穿越应 400，得到 %d %v", code, out)
	}
	// 二进制保存也拒（即便哈希对得上）
	code, out = panelPost(t, h, sess.ID, map[string]any{
		"path": "bin.dat", "content": "文本", "hash": hashBytes([]byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02}),
	})
	if code != 400 {
		t.Fatalf("二进制保存应 400，得到 %d %v", code, out)
	}
}

// CRLF 文件（.bat/.cmd/.ps1）：编辑器只送 LF 回来，写回要还原成 CRLF
func TestPanelEditKeepsCRLF(t *testing.T) {
	_, sess, ws, h := panelTestApp(t)
	target := filepath.Join(ws, "run.cmd")
	if err := os.WriteFile(target, []byte("echo 一\r\necho 二\r\n"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	_, got := panelGet(t, h, sess.ID, "run.cmd")
	if got["lineEndings"] != "crlf" {
		t.Fatalf("应识别为 CRLF，得到 %v", got["lineEndings"])
	}
	code, out := panelPost(t, h, sess.ID, map[string]any{
		"path": "run.cmd", "content": "echo 一\necho 二\necho 三\n", "hash": got["hash"], "lineEndings": "crlf",
	})
	if code != 200 {
		t.Fatalf("保存应 200，得到 %d %v", code, out)
	}
	if b, _ := os.ReadFile(target); string(b) != "echo 一\r\necho 二\r\necho 三\r\n" {
		t.Fatalf("写回应保持 CRLF，得到 %q", string(b))
	}
}

// 工具函数：工作区判定 / 主导行尾 / 失败原因翻译
func TestPanelEditHelpers(t *testing.T) {
	in := []string{"a.txt", "web/app-chat.js", ".jxai/tmp/x", "."}
	for _, p := range in {
		if !withinWorkspace(p) {
			t.Fatalf("%q 应在工作区内", p)
		}
	}
	out := []string{"../x", "..", "D:/x/y", "/etc/passwd"}
	if isWindowsRuntime {
		out = append(out, "..\\..\\x") // Windows：反斜杠是分隔符，构成穿越
	}
	for _, p := range out {
		if withinWorkspace(p) {
			t.Fatalf("%q 应判为工作区外", p)
		}
	}
	if dominantLineEndings([]byte("a\r\nb\r\nc\n")) != "crlf" || dominantLineEndings([]byte("a\nb\n")) != "lf" {
		t.Fatalf("主导行尾判定不对")
	}
	if msg := panelWriteError(os.ErrPermission); !strings.Contains(msg, "只读") || !strings.Contains(msg, "微信接收目录") {
		t.Fatalf("权限错误应给只读提示（含微信接收目录），得到 %q", msg)
	}
	if msg := panelWriteError(errSharingViolation{}); !strings.Contains(msg, "占用") {
		t.Fatalf("占用错误应给被占用提示，得到 %q", msg)
	}
}

// errSharingViolation 模拟 Windows 的共享冲突错误文本
type errSharingViolation struct{}

func (errSharingViolation) Error() string {
	return "rename x: The process cannot access the file because it is being used by another process."
}
