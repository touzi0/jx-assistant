package main

// 右栏「文件」视图的编辑模式（用户手工改文件，不必再开记事本）：
//
//	GET  /api/sessions/{id}/file?path=   取文件**当前完整内容** + 版本标识
//	POST /api/sessions/{id}/file         按版本标识校验后写回
//
// 为什么不复用 /file-version：那条口径是"看历史版本对比"，正文按 256KB 截断
// （snapshotVersionMaxText），把截断内容回写等于毁掉大文件；编辑器要的是当前
// 全文（上限 2MB）加一次写前冲突校验，所以单开一对接口。
//
// 为什么用 mux.HandleFunc 而不进 apiRoute：这是用户在界面上的手势（与
// open-path / delivery/open、md 渲染页同一类），写文件的能力必须留在人手——
// 进了 call_api 目录，长期助理（总控刻意不配写工具）就能绕道拿到写入口。
//
// 写前一律先落快照（复用 snapshot.go 的 snapshotBase / recordCheckpoint），
// 记成 tool_name=file_edit、seq=0：只进该文件的「历史版本」（可按版本回退），
// 不锚定任何对话回合，因此不会被「按轮撤回 / 全部撤回」顺带带走。

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	// fileEditMaxBytes 右栏编辑器能打开 / 保存的单文件上限（文本，2MB）。
	fileEditMaxBytes = 2 << 20
	// uiEditTool 右栏手工编辑落的快照工具名（历史版本列表里显示 file_edit）。
	uiEditTool = "file_edit"
)

// handleFileContent 取一个工作区文件的当前完整内容：编辑器打开时调。
// 返回 text / size / mtime / hash / encoding / lineEndings——hash 与 mtime 是
// 保存时的冲突校验基线，行尾风格用于回写时还原（textarea 只有 LF）。
func (a *App) handleFileContent(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if raw == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, rel, ok := panelEditTarget(w, tctx, raw)
	if !ok {
		return
	}
	data, code, msg := panelEditBytes(tctx, abs)
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	enc := detectTextEncoding(data)
	if enc == EncBinary {
		writeErr(w, 400, "这是二进制文件（"+humanSize(int64(len(data)))+"），右栏编辑器只能打开文本文件——用顶栏「打开」交给系统程序")
		return
	}
	if enc == EncUnknown {
		writeErr(w, 400, "这个文件的编码识别不了（既不是 UTF-8，也不是 GBK / UTF-16 文本），为免写坏不在这里编辑——用顶栏「打开」交给系统程序")
		return
	}
	text, enc2 := decodeText(data)
	if enc2 == EncUnknown { // 解不动（GB 族解压失败）/ 已被替换符污染：同样不放进编辑器
		writeErr(w, 400, "这个文件的编码识别不了（"+enc.String()+" 解码失败），为免写坏不在这里编辑——用顶栏「打开」交给系统程序")
		return
	}
	writeJSON(w, 200, map[string]any{
		"path": rel, "text": text, "size": len(data),
		"mtime": panelMtime(tctx, abs), "hash": hashBytes(data),
		"encoding": enc.String(), "lineEndings": dominantLineEndings(data),
		"maxBytes": fileEditMaxBytes,
	})
}

// handleFileSave 保存编辑器里的内容。写前必查冲突（hash 优先、其次 mtime）：
// 磁盘上的内容与用户打开编辑器时读到的不一致就 409 并拒绝——绝不让界面上的
// 旧稿静默盖掉别的程序（或助手自己）刚写进去的新内容。
func (a *App) handleFileSave(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Path        string `json:"path"`
		Content     string `json:"content"`
		Hash        string `json:"hash"`
		MTime       int64  `json:"mtime"`
		LineEndings string `json:"lineEndings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	raw := strings.TrimSpace(body.Path)
	if raw == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	hash := strings.ToLower(strings.TrimSpace(body.Hash))
	if hash == "" && body.MTime == 0 {
		writeErr(w, 400, "需要 hash 或 mtime：保存必须带上打开编辑器时读到的版本标识，否则无法判断文件是否已被其他程序改过")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, rel, ok := panelEditTarget(w, tctx, raw)
	if !ok {
		return
	}
	cur, code, msg := panelEditBytes(tctx, abs)
	if code == 404 {
		writeErr(w, 409, "文件已经被删除或改名了（不是这里改的），没有写回——重开编辑可看到最新状态")
		return
	}
	if code != 0 {
		writeErr(w, code, msg)
		return
	}
	if hash != "" {
		if hashBytes(cur) != hash {
			writeErr(w, 409, "文件已被其他程序修改过（内容与打开编辑时读到的不一致），没有覆盖——点「取消」再重新打开编辑即可看到最新内容")
			return
		}
	} else {
		// 只有 mtime 可用时按修改时间比对。远程工作区拿不到 mtime（SFTP stat 无此
		// 字段），此时只能靠哈希——读侧总会下发 hash，正常不会落到这一支。
		curM := panelMtime(tctx, abs)
		if curM == 0 {
			writeErr(w, 409, "无法校验文件有没有被改过（远程工作区取不到修改时间），没有写回——请重新打开编辑再保存")
			return
		}
		if curM != body.MTime {
			writeErr(w, 409, "文件已被其他程序修改过（修改时间与打开编辑时不一致），没有覆盖——点「取消」再重新打开编辑即可看到最新内容")
			return
		}
	}
	enc := detectTextEncoding(cur)
	if enc == EncBinary || enc == EncUnknown {
		writeErr(w, 400, "磁盘上这个文件已经不是可编辑的文本了（编码识别不了），拒绝按文本覆盖——用顶栏「打开」交给系统程序")
		return
	}
	content := body.Content
	if strings.EqualFold(strings.TrimSpace(body.LineEndings), "crlf") {
		// HTML textarea 的换行一律是 LF：CRLF 文件（.bat/.cmd/.ps1 等）回写前
		// 还原成 CRLF，否则"用右栏编辑器改一下"会悄悄把整个文件的行尾改掉。
		content = strings.ReplaceAll(strings.ReplaceAll(content, "\r\n", "\n"), "\n", "\r\n")
	}
	out, eerr := encodeText(content, enc)
	if eerr != nil {
		writeErr(w, 400, eerr.Error()+"（文件未改动）")
		return
	}
	if len(out) > fileEditMaxBytes {
		writeErr(w, 400, "内容太大（"+humanSize(int64(len(out)))+"），右栏编辑器只支持保存 2MB 以内的文本")
		return
	}
	cpID, werr := tctx.panelWriteFile(abs, out)
	if werr != nil {
		writeErr(w, 500, panelWriteError(werr))
		return
	}
	// 写后回读：以磁盘真实内容为准给新的版本标识，前端据此继续编辑 / 再存一次
	after, rerr := tctx.envReadFile(abs)
	if rerr != nil {
		after = out
	}
	writeJSON(w, 200, map[string]any{
		"ok": "1", "path": rel, "size": len(after),
		"mtime": panelMtime(tctx, abs), "hash": hashBytes(after),
		"encoding": enc.String(), "lineEndings": dominantLineEndings(after),
		"snapshotId": cpID, "versioned": cpID != 0,
	})
}

// panelEditTarget 把前端给的文件路径解析成可编辑的绝对路径，并做工作区边界
// 校验（防目录穿越）。不合格时已经写好错误响应，返回 ok=false。
//
// 路径口径与快照一致（工作区相对路径）；越界、或指向 .jxai 系统目录（快照正文
// / 暂存 / 交付备份都是助手内部数据，改坏了回退能力就没了）一律拒绝。
func panelEditTarget(w http.ResponseWriter, t *toolContext, raw string) (abs, rel string, ok bool) {
	abs, err := t.resolve(raw)
	if err != nil {
		writeErr(w, 400, "路径无法解析："+raw)
		return "", "", false
	}
	if !t.isRemote() {
		// 工作区内的符号链接可能指向外面：按真实位置再校验一次（不存在则跳过，
		// 新建的情形由下面的工作区判定兜住）
		if real, eerr := filepath.EvalSymlinks(abs); eerr == nil {
			abs = real
		}
	}
	rel = t.displayPath(abs)
	if !withinWorkspace(rel) {
		writeErr(w, 400, "只能编辑本会话工作区内的文件："+raw+" 落在工作区之外")
		return "", "", false
	}
	if jxaiHiddenRel(rel) {
		writeErr(w, 400, "这是助手的内部数据（.jxai 系统目录：快照正文 / 暂存 / 交付备份），不能在右栏编辑")
		return "", "", false
	}
	return abs, rel, true
}

// withinWorkspace 判断 displayPath 的结果是否落在工作区内：工作区内是相对
// 斜杠路径；落到外面时 displayPath 返回原样绝对路径（本地 D:/…、远程 /…）。
func withinWorkspace(rel string) bool {
	rel = filepath.ToSlash(strings.TrimSpace(rel))
	if rel == "" || rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	if filepath.IsAbs(rel) || path.IsAbs(rel) {
		return false
	}
	return !(len(rel) > 1 && rel[1] == ':') // Windows 盘符（远程会话跑在 Linux 判据下也成立）
}

// panelEditBytes 读一个待编辑文件的原始字节：存在性 / 是否目录 / 2MB 上限都在
// 这里拦，返回 HTTP 码与中文原因（code=0 表示成功）。
func panelEditBytes(t *toolContext, abs string) (data []byte, code int, msg string) {
	isDir, size, err := t.envStat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 404, "文件不存在（可能已被删除或改名）"
		}
		return nil, 400, "读不到这个文件：" + err.Error()
	}
	if isDir {
		return nil, 400, "这是一个文件夹，不能当文件编辑"
	}
	if size > fileEditMaxBytes {
		return nil, 400, "文件太大（" + humanSize(size) + "），右栏编辑器只支持 2MB 以内的文本文件——用顶栏「打开」交给系统程序"
	}
	b, rerr := t.envReadFile(abs)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return nil, 404, "文件不存在（可能已被删除或改名）"
		}
		return nil, 400, panelReadError(rerr)
	}
	return b, 0, ""
}

// panelReadError 读失败的人话：独占打开（别人正占着）与权限拒绝最常见，其余
// 原样带上系统原因（用户拿着原文也好查）。
func panelReadError(err error) string {
	raw := err.Error()
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(low, "being used by another process") || strings.Contains(raw, "正由另一进程使用"):
		return "读不到这个文件：它正被其他程序独占打开（记事本 / Office 等），关掉那个程序再试"
	case errors.Is(err, fs.ErrPermission) || strings.Contains(low, "access is denied") || strings.Contains(raw, "拒绝访问"):
		return "读不到这个文件：没有读取权限（只读目录或文件权限受限）"
	}
	return "读不到这个文件：" + raw
}

// panelMtime 取本地文件的修改时间（毫秒）；0 = 取不到（远程工作区没有 mtime）。
func panelMtime(t *toolContext, abs string) int64 {
	if t.isRemote() {
		return 0
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return 0
	}
	return fi.ModTime().UnixMilli()
}

// dominantLineEndings 判内容的主导行尾（"crlf" / "lf"），供编辑器回写时还原。
func dominantLineEndings(b []byte) string {
	crlf, lf := 0, 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		if i > 0 && b[i-1] == '\r' {
			crlf++
		} else {
			lf++
		}
	}
	if crlf > lf {
		return "crlf"
	}
	return "lf"
}

// panelWriteFile 是右栏手工编辑的写入口：与模型的 write/edit 走同一套"写前留
// 旧版本"（snapshotBase / recordCheckpoint），但不过权限引擎（用户本人在界面上
// 的操作）且不锚定回合（seq=0）。返回新落快照的 id（0 = 没留快照）。
func (t *toolContext) panelWriteFile(abs string, data []byte) (int64, error) {
	before, existed, track := t.snapshotBase(abs)
	if err := t.envWriteFile(abs, data); err != nil {
		return 0, err
	}
	if !track {
		return 0, nil
	}
	after, rerr := t.envReadFile(abs)
	if rerr != nil {
		after = data // 远程回读失败时按写入内容记账（哈希仍可用于回退校验）
	}
	return t.recordCheckpoint(&Checkpoint{
		SessionID: t.sess.ID, Seq: 0, ToolName: uiEditTool,
		Path: t.displayPath(abs), ExistedBefore: existed, BeforeSize: int64(len(before)),
		AfterHash: hashBytes(after), AfterSize: int64(len(after)), CreatedAt: now(),
	}, before), nil
}

// panelWriteError 把底层写入错误翻成人话：用户看到的是"只读 / 被占用"，不是
// "Access is denied"。这两类在 Windows 上一个来自建临时文件/rename 的权限拒绝，
// 一个来自 FILE_SHARE 冲突，提示词里都把"该怎么办"说清楚。
func panelWriteError(err error) string {
	raw := err.Error()
	low := strings.ToLower(raw)
	switch {
	case errors.Is(err, fs.ErrPermission) || strings.Contains(low, "access is denied") ||
		strings.Contains(raw, "拒绝访问") || strings.Contains(low, "read-only"):
		// Windows 上这两种情形报的是同一个错误码（rename 覆盖需要删除权）：目录/
		// 文件只读，或者文件正被别的程序开着。两种可能都写出来，用户才好动手。
		return "写入被拒绝：目标目录或文件是只读的（只读目录或微信接收目录默认只读，文件也可能带只读属性），也可能是文件正被其他程序占用（记事本 / Office 打开着）。取消只读属性、关掉占用它的程序后再试。"
	case strings.Contains(low, "being used by another process") || strings.Contains(low, "sharing violation") ||
		strings.Contains(raw, "正由另一进程使用") || strings.Contains(raw, "另一个程序"):
		return "文件正被其他程序占用（可能已在记事本 / Office / 编辑器里打开），关掉那个程序再保存。"
	case strings.Contains(low, "no space left") || strings.Contains(raw, "磁盘空间不足"):
		return "磁盘空间不足，没有写入。"
	}
	return "写入失败：" + raw
}
