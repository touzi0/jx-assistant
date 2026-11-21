package main

// 文件快照子系统（对齐 ZCode 的 workspace checkpoint 机制）：
//
//   - 采集：write / edit 写入时自动记录改动前内容（snapshotWrite）；
//     run_command、外部程序、Office 文档运行库等无法追踪的修改，由模型按
//     系统提示词的约定先调 snapshot_file 手动记录（见 tools.go 的工具定义）。
//   - 回退：编辑历史消息时带 workspaceMode=rewind（server.go 的 handleEdit），
//     或在文件改动面板上按轮撤销（handleFileRewind）。两者共用本文件的
//     planRewind / applyRewind：安全预检（fail-closed）→ 补偿 journal →
//     全部成功才动对话记录（文件与对话要么都退、要么都不退）。
//   - 保留策略：snapshotJanitor 周期清理——按天（SnapshotDays）超龄删除、
//     按量（SnapshotMaxMB）从最旧开始驱逐，保证库不无限膨胀。
//
// 路径口径：快照统一存"工作区内相对路径"（正斜杠），工作区改名/搬家后回退
// 仍能按相对路径重新解析；写到工作区之外的文件没有相对形态，存原样绝对路径。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// snapshotMaxFileBytes 自动快照的单文件上限：超过则跳过该文件的快照（回退时
// 它按未追踪处理）。防单个大文件把库撑爆；总量另由自动池预算与按天清理约束。
const snapshotMaxFileBytes = 32 << 20

// snapshotManualMaxFileBytes 手动快照（snapshot_file）的单文件上限。手动档
// 的用途恰恰是 Office 文档等"大而重要"的文件，上限放得很宽；清理主力是
// 按天过期（SnapshotDays），手动池默认不限量。
const snapshotManualMaxFileBytes = 256 << 20

// manualSnapshotTool 标记"模型手动打的快照"：这类快照之后文件会按预期被
// 不可追踪的方式修改，回退时不做哈希预检（信任快照时刻的内容）。
const manualSnapshotTool = "snapshot_file"

// hashBytes 计算内容的十六进制 sha256（安全预检用）。
func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// displayPath 把解析后的绝对路径折算回"工作区内相对路径"（正斜杠）；
// 不在工作区内则返回原样绝对路径。工作区可能整体改名/搬家，快照里只存
// 相对形态才能在改名后继续回退。
func (t *toolContext) displayPath(abs string) string {
	ws := t.workspace()
	if t.isRemote() {
		ws = path.Clean(ws)
		if strings.HasPrefix(abs, ws+"/") {
			return strings.TrimPrefix(abs, ws+"/")
		}
		return filepath.ToSlash(abs)
	}
	// 配置里的工作区可能是相对路径（基于进程 CWD），先转绝对再求相对
	if wabs, err := filepath.Abs(ws); err == nil {
		ws = wabs
	}
	rel, err := filepath.Rel(ws, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}

// snapshotsEnabled 文件快照总开关（设置页「文件备份」）。配置缺省视为开。
func (a *App) snapshotsEnabled() bool {
	if a.Cfg == nil || a.Cfg.SnapshotEnabled == nil {
		return true
	}
	return *a.Cfg.SnapshotEnabled
}

// jxAIName 工作区内的系统目录名：快照、模型临时文件（.jxai/tmp）都归这里。
const jxAIName = ".jxai"

// snapshotFilePath 一条快照的正文文件位置（工作区相对路径）：
// .jxai/snapshots/<会话id>/<checkpoint id>.bin
func snapshotFilePath(sessionID string, id int64) string {
	return fmt.Sprintf("%s/snapshots/%s/%d.bin", jxAIName, sessionID, id)
}

// writeSnapshotBody 把快照正文写进 .jxai/snapshots/<会话>/<id>.bin。
func (t *toolContext) writeSnapshotBody(id int64, content []byte) error {
	p, err := t.resolve(snapshotFilePath(t.sess.ID, id))
	if err != nil {
		return err
	}
	return t.envWriteFile(p, content)
}

// loadBefore 取一条快照的正文：旧库内联 BLOB 优先，否则读 .jxai 里的落盘文件。
func (t *toolContext) loadBefore(cp *Checkpoint) []byte {
	if cp.Before != nil {
		return cp.Before
	}
	if !cp.ExistedBefore {
		return nil // 改动前不存在：本就没有正文
	}
	p, err := t.resolve(snapshotFilePath(cp.SessionID, cp.ID))
	if err != nil {
		return nil
	}
	b, err := t.envReadFile(p)
	if err != nil {
		return nil
	}
	return b
}

// ---------- 历史版本查看（文件改动面板行内「历史版本」的数据源） ----------

// snapshotVersionMaxText 单个版本正文下发给前端的字节上限（超出截断，
// 前端提示「内容过长，仅显示前 N KB」）。历史版本是"供对比或回退"的阅读
// 材料，不做全量下载——要看整份内容用户会去打开当前文件。
const snapshotVersionMaxText = 256 << 10

// fileVersion 是一条历史版本摘要：某文件的一条快照。existedBefore=false
// 说明"这次改动前文件不存在"（该版本即新建），前端按新建展示。hasBody 表示
// 快照正文（.jxai/snapshots 里的文件）还在；current 表示该版本改动后的内容
// 与文件当前内容一致；manual 表示来自手动 snapshot_file（回退不做哈希预检）。
type fileVersion struct {
	ID            int64  `json:"id"`
	Seq           int64  `json:"seq"`
	ToolName      string `json:"toolName"`
	ExistedBefore bool   `json:"existedBefore"`
	BeforeSize    int64  `json:"beforeSize"`
	AfterSize     int64  `json:"afterSize"`
	CreatedAt     string `json:"createdAt"`
	HasBody       bool   `json:"hasBody"`
	Current       bool   `json:"current"`
	Manual        bool   `json:"manual"`
}

// fileVersionList 列出某文件在本会话的全部历史版本（最旧在前）。当前版本用
// "该版本改动后的内容哈希 == 文件当前内容哈希" 判定，无需前端再哈希一遍。
func (a *App) fileVersionList(t *toolContext, path string) ([]fileVersion, error) {
	cps, err := a.Store.CheckpointsForPath(t.sess.ID, path)
	if err != nil {
		return nil, err
	}
	curHash := ""
	if abs, rerr := t.resolve(path); rerr == nil {
		if b, rerr2 := t.envReadFile(abs); rerr2 == nil {
			curHash = hashBytes(b)
		}
	}
	out := make([]fileVersion, 0, len(cps))
	for _, cp := range cps {
		v := fileVersion{
			ID: cp.ID, Seq: cp.Seq, ToolName: cp.ToolName,
			ExistedBefore: cp.ExistedBefore, BeforeSize: cp.BeforeSize,
			AfterSize: cp.AfterSize, CreatedAt: cp.CreatedAt,
			Manual: cp.ToolName == manualSnapshotTool,
		}
		if cp.ExistedBefore {
			v.HasBody = t.loadBefore(cp) != nil
		}
		if curHash != "" && cp.AfterHash == curHash {
			v.Current = true
		}
		out = append(out, v)
	}
	return out, nil
}

// fileVersionBody 取某个版本正文（改动前内容），带"是否二进制/是否截断"。
// existedBefore=false 的版本没有正文：返回空内容 + existedBefore=false，
// 前端按"这次改动前文件不存在（新建）"展示。
func (t *toolContext) fileVersionBody(cp *Checkpoint) (data []byte, truncated bool, ok bool) {
	if !cp.ExistedBefore {
		return nil, false, true
	}
	b := t.loadBefore(cp)
	if b == nil {
		return nil, false, false
	}
	if len(b) > snapshotVersionMaxText {
		return b[:snapshotVersionMaxText], true, true
	}
	return b, false, true
}

// workspaceFileBody 读会话工作区内某文件当前内容（与 fileVersionBody 同口径：
// 二进制判定与截断由调用方按需处理），供历史版本与"当前内容"对比。
func (t *toolContext) workspaceFileBody(path string) (data []byte, exists bool, truncated bool) {
	abs, err := t.resolve(path)
	if err != nil {
		return nil, false, false
	}
	b, err := t.envReadFile(abs)
	if err != nil {
		return nil, false, false
	}
	if len(b) > snapshotVersionMaxText {
		return b[:snapshotVersionMaxText], true, true
	}
	return b, true, false
}

// appendVersionBody 把正文按 text/binary 填充进响应体：二进制（含 NUL 或
// 非 UTF-8）不下发正文，只给标记与字节数，前端显示"二进制内容，不展示文本"。
func appendVersionBody(m map[string]any, data []byte, truncated bool) {
	if data == nil {
		m["text"] = ""
		m["binary"] = false
		m["size"] = 0
		m["truncated"] = false
		return
	}
	m["size"] = len(data)
	m["truncated"] = truncated
	if isBinaryBytes(data) {
		m["binary"] = true
		m["text"] = ""
		return
	}
	m["binary"] = false
	m["text"] = string(data)
}

// isBinaryBytes 判定内容是否二进制：含 NUL 字节或不是合法 UTF-8 都算。
func isBinaryBytes(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return true
	}
	return !utf8.Valid(b)
}

// deleteSnapshotFiles 删除快照正文文件（行已删或将删；文件不存在视为成功）。
func (t *toolContext) deleteSnapshotFiles(ids []int64) {
	for _, id := range ids {
		p, err := t.resolve(snapshotFilePath(t.sess.ID, id))
		if err != nil {
			continue
		}
		if err := t.envDeleteFile(p); err != nil {
			log.Printf("[快照] 会话 %s 清理正文文件 %d 失败: %v", t.sess.ID, id, err)
		}
	}
}

// copySnapshotFiles 分支复制快照时同步复制正文文件（pairs = 源id → 新id）。
// 源正文可能是旧库内联 BLOB（读到再写）或 .jxai 落盘文件，loadBefore 统一处理。
func (a *App) copySnapshotFiles(from, to *SessionMeta, pairs [][2]int64) {
	if len(pairs) == 0 {
		return
	}
	ftx := &toolContext{app: a, sess: from}
	ttx := &toolContext{app: a, sess: to}
	for _, pr := range pairs {
		src, err := ftx.resolve(snapshotFilePath(from.ID, pr[0]))
		if err != nil {
			continue
		}
		body, rerr := ftx.envReadFile(src)
		if rerr != nil {
			// 源正文可能是内联 BLOB：按 checkpoint 元数据再取一次
			cps, _ := a.Store.CheckpointsAfter(from.ID, 0)
			for _, cp := range cps {
				if cp.ID == pr[0] {
					body = ftx.loadBefore(cp)
					break
				}
			}
		}
		if body == nil {
			continue
		}
		if err := ttx.writeSnapshotBody(pr[1], body); err != nil {
			log.Printf("[快照] 分支复制正文 %d→%d 失败: %v", pr[0], pr[1], err)
		}
	}
}

// ensureJxAI 确保工作区里有 .jxai/tmp（模型临时文件的约定去处，见系统提示词）。
// 选定工作目录后尽快调用一次；模型用 write 写 .jxai/tmp 也会自动建目录，
// 这里只是让目录从一开始就存在、可被发现。
func (t *toolContext) ensureJxAI() {
	p, err := t.resolve(jxAIName + "/tmp")
	if err != nil {
		return
	}
	if err := t.envMkdirAll(p); err != nil {
		log.Printf("[.jxai] 会话 %s 建临时目录失败: %v", t.sess.ID, err)
	}
}

// snapshotWrite 带快照的文件写入（write / edit 共用）：先读旧内容，
// 写入成功后回读算哈希，落一条 checkpoint 并把正文写进 .jxai/snapshots/。
// 旧文件读不了、超过单文件上限、正文属于 .jxai 系统子目录或备份开关关闭时跳过
// 快照（写入照常；.jxai/uploads 里的用户上传文件不在其列，照常追踪）。快照
// 失败只记日志，不让正常写入报错。
//
// 回读结果随返回值交给调用方拼进工具成功文案（readBackNote）：verified
// 表示"真的从盘上/SFTP 读回来了"；快照关闭（track=false）或远程回读失败
// 降级时为 false，此时 readBack 不可作数。
func (t *toolContext) snapshotWrite(abs string, grant *Grant, data []byte) (readBack []byte, verified bool, err error) {
	before, existed, track := t.snapshotBase(abs)
	if err := t.envWriteFileWithGrant(abs, data, grant); err != nil {
		return nil, false, err
	}
	if !track {
		return nil, false, nil
	}
	after, rerr := t.envReadFile(abs)
	if rerr != nil {
		after = data // 远程回读失败时退回写入内容（本地写即所得）；verified=false 如实相告
	} else {
		readBack, verified = after, true
	}
	t.recordCheckpoint(&Checkpoint{
		SessionID: t.sess.ID, Seq: t.seq, ToolCallID: t.callID, ToolName: t.toolName,
		Path: t.displayPath(abs), ExistedBefore: existed, BeforeSize: int64(len(before)),
		AfterHash: hashBytes(after), AfterSize: int64(len(after)), CreatedAt: now(),
	}, before)
	return readBack, verified, nil
}

// snapshotBase 采集一次写入前的基线与"是否值得追踪"：返回旧内容、改动前是否
// 存在、是否落快照。旧文件读不了、超单文件上限、备份开关关闭、正文属于 .jxai
// 系统子目录时 track=false（写入照常，只是不留基线）。
//
// 抽出来是因为两条写入路径都要它：模型的 write/edit（snapshotWrite）与用户在
// 右栏「文件」视图里的手工编辑（fileedit.go 的 panelWriteFile）——后者的写入
// 不过权限引擎、也不锚定对话回合，但"写前留旧版本"的口径必须与前者一致。
func (t *toolContext) snapshotBase(abs string) (before []byte, existed, track bool) {
	existed = true
	track = t.app.snapshotsEnabled()
	if isDir, size, err := t.envStat(abs); err != nil {
		if os.IsNotExist(err) {
			existed = false
		} else {
			track = false // 状态不明：不建快照，避免留下错误基线
		}
	} else if isDir || size > snapshotMaxFileBytes {
		track = false
	} else if b, rerr := t.envReadFile(abs); rerr != nil {
		track = false
	} else {
		before = b
	}
	if track && jxaiHiddenRel(t.displayPath(abs)) {
		track = false // .jxai 系统子目录（快照正文/暂存/交付备份）不做快照；
		// uploads 里的用户上传文件照常追踪——文件改动行要能看到对它的编辑
	}
	return before, existed, track
}

// recordCheckpoint 落一条快照并把它改动前的正文写进 .jxai/snapshots/。返回快照
// id（快照没写成返回 0）。快照只是"能回退"的保障，失败只记日志，不影响已经
// 成功的写入。
func (t *toolContext) recordCheckpoint(cp *Checkpoint, before []byte) int64 {
	id, aerr := t.app.Store.AppendCheckpoint(cp)
	if aerr != nil {
		log.Printf("[快照] 会话 %s 写 %s 的快照落库失败: %v", t.sess.ID, cp.Path, aerr)
		return 0
	}
	if cp.ExistedBefore {
		if werr := t.writeSnapshotBody(id, before); werr != nil {
			// 正文写不进去就把元数据行一并撤掉，避免留下"有快照却没正文"的坑
			log.Printf("[快照] 会话 %s 写 %s 的快照正文失败: %v", t.sess.ID, cp.Path, werr)
			_ = t.app.Store.DeleteCheckpoints(t.sess.ID, []int64{id})
			return 0
		}
	}
	return id
}

// rewindPlan 是一个文件的回退计划：恢复到 before 状态，或删除（改动前不存在）。
type rewindPlan struct {
	Path          string `json:"path"`
	Action        string `json:"action"`           // restore=恢复旧内容 delete=撤回新建
	Reason        string `json:"reason,omitempty"` // unsafe 时的人类可读原因
	Manual        bool   `json:"manual"`           // 基线来自手动快照（不做哈希预检）
	ExistedBefore bool   `json:"existedBefore"`
	Before        []byte
	IDs           []int64 `json:"-"` // 这条计划消耗掉的快照行 id（回退成功后精确清理）
}

// planRewind 为一组快照生成回退计划。seqs 为空表示"seq 之后全部"（编辑重发），
// 非空表示按轮撤销（只回退这些 seq 内的改动）。同一文件多次改动时：基线取
// 最早的快照（改动前状态），预检对最晚的快照（最后一次记录的修改之后文件
// 不应再被外部改过）。返回 safe（可执行）与 unsafe（拒绝执行，带原因）。
// pathFilter 非空时只回退该文件（文件改动面板行内「撤回」用），其余文件的
// 快照原样留在库里，供面板上的「全部撤回」或后续操作使用。
func (a *App) planRewind(t *toolContext, sessionID string, seq int64, seqs []int64, pathFilter string) (safe, unsafe []*rewindPlan, consumed []int64, err error) {
	var cps []*Checkpoint
	if len(seqs) > 0 {
		cps, err = a.Store.CheckpointsIn(sessionID, seqs)
	} else {
		cps, err = a.Store.CheckpointsAfter(sessionID, seq)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if pathFilter != "" {
		kept := cps[:0:0]
		for _, cp := range cps {
			if cp.Path == pathFilter {
				kept = append(kept, cp)
			}
		}
		cps = kept
	}
	safe, unsafe, consumed = a.planRewindFor(t, cps)
	return safe, unsafe, consumed, nil
}

// planRewindFor 是回退计划的实现主体：按文件归并给定的快照集（顺序 = 落库顺序），
// 每个文件产出一条计划。consumed 是这批计划消耗掉的快照行 id——"全部撤回"与
// "单文件撤回"都只清理真正用掉的那些，未被处理的文件快照（如按行撤回后剩下的
// 其他文件）原样留在库里，仍能再次回退。
func (a *App) planRewindFor(t *toolContext, cps []*Checkpoint) (safe, unsafe []*rewindPlan, consumed []int64) {
	type fileState struct {
		first, last *Checkpoint
		ids         []int64
	}
	order := []string{}
	byPath := map[string]*fileState{}
	for _, cp := range cps {
		st, ok := byPath[cp.Path]
		if !ok {
			st = &fileState{first: cp}
			byPath[cp.Path] = st
			order = append(order, cp.Path)
		}
		st.last = cp
		st.ids = append(st.ids, cp.ID)
	}
	for _, p := range order {
		st := byPath[p]
		plan := &rewindPlan{
			Path: p, ExistedBefore: st.first.ExistedBefore, IDs: st.ids,
			Manual: st.last.ToolName == manualSnapshotTool,
		}
		if plan.ExistedBefore {
			plan.Action = "restore"
			plan.Before = t.loadBefore(st.first)
			if plan.Before == nil {
				plan.Reason = "快照正文缺失（.jxai/snapshots 里的文件不在了），拒绝盲恢复"
				unsafe = append(unsafe, plan)
				continue
			}
		} else {
			plan.Action = "delete"
		}
		abs, rerr := t.resolve(p)
		if rerr != nil {
			plan.Reason = "路径无法解析"
			unsafe = append(unsafe, plan)
			continue
		}
		cur, cerr := t.envReadFile(abs)
		switch {
		case cerr != nil && os.IsNotExist(cerr):
			if !plan.ExistedBefore {
				// 要撤回的新建文件已经不在了：目标状态已达成，跳过（快照照样清掉）
				consumed = append(consumed, st.ids...)
				continue
			}
			if plan.Manual {
				// 手动快照的文件：改后的删除可能正是不可追踪修改的一部分，照常恢复
				safe = append(safe, plan)
				continue
			}
			plan.Reason = "文件已不存在（可能被移动或删除）"
			unsafe = append(unsafe, plan)
			continue
		case cerr != nil && !plan.Manual:
			plan.Reason = "当前内容无法读取：" + cerr.Error()
			unsafe = append(unsafe, plan)
			continue
		}
		if cerr == nil && !plan.Manual && hashBytes(cur) != st.last.AfterHash {
			plan.Reason = "文件在最后一次记录的修改之后又被改过（外部修改），拒绝盲恢复"
			unsafe = append(unsafe, plan)
			continue
		}
		safe = append(safe, plan)
		consumed = append(consumed, st.ids...)
	}
	return safe, unsafe, consumed
}

// applyRewind 执行回退计划，带补偿 journal：每个文件动手前先记下当前状态，
// 中途失败按逆序恢复，不留半截状态。journal 补偿也失败时返回不可恢复错误
// （如实上抛，由调用方写日志与提示）。
func (a *App) applyRewind(t *toolContext, plans []*rewindPlan) error {
	type journalEntry struct {
		abs      string
		existed  bool
		content  []byte
		readFail bool
	}
	journal := []journalEntry{}
	rollback := func() {
		for i := len(journal) - 1; i >= 0; i-- {
			e := journal[i]
			if e.readFail {
				continue // 动手前就读不到的文件，补偿无从谈起
			}
			if e.existed {
				_ = t.envWriteFile(e.abs, e.content)
			} else {
				_ = t.envDeleteFile(e.abs)
			}
		}
	}
	for _, p := range plans {
		abs, err := t.resolve(p.Path)
		if err != nil {
			rollback()
			return fmt.Errorf("回退 %s 失败：路径无法解析", p.Path)
		}
		cur, curErr := t.envReadFile(abs)
		entry := journalEntry{abs: abs, readFail: curErr != nil}
		if curErr == nil {
			entry.existed, entry.content = true, cur
			journal = append(journal, entry)
		} else if os.IsNotExist(curErr) {
			entry.existed = false
			journal = append(journal, entry)
		} else {
			journal = append(journal, entry)
		}
		if p.Action == "delete" {
			if err := t.envDeleteFile(abs); err != nil {
				rollback()
				return fmt.Errorf("撤回新建文件 %s 失败: %w", p.Path, err)
			}
			continue
		}
		if err := t.envWriteFile(abs, p.Before); err != nil {
			rollback()
			return fmt.Errorf("恢复 %s 失败: %w", p.Path, err)
		}
	}
	return nil
}

// snapshotJanitor 快照保留策略的周期执行：启动后先跑一轮，之后每天一轮。
// 主力是按天清理（SnapshotDays=0/-1 表示永不过期）；限量驱逐是兜底——自动池
// SnapshotMaxMB 默认 32MB，手动池 SnapshotManualMaxMB 默认 -1 不限量（确有
// 磁盘压力再配 500/1024 级别）。两者都按文件原子清理（见 store.go 注释），
// 且同步删除 .jxai/snapshots/ 里的正文文件。备份开关只停新采集，不停清理——
// 关掉之后存量快照仍按天数退场，不留孤儿文件。
func (a *App) snapshotJanitor() {
	run := func() {
		days := a.Cfg.SnapshotDays
		sessions, err := a.Store.List()
		if err != nil {
			return
		}
		for _, m := range sessions {
			tctx := &toolContext{app: a, sess: m}
			if days > 0 {
				cutoff := time.Now().AddDate(0, 0, -days).Format("2006-01-02T15:04:05")
				if ids, err := a.Store.PruneCheckpointsOlderThan(m.ID, cutoff); err != nil {
					log.Printf("[快照] 会话 %s 超龄清理失败: %v", m.ID, err)
				} else if len(ids) > 0 {
					tctx.deleteSnapshotFiles(ids)
					log.Printf("[快照] 会话 %s 清理 %d 条超龄快照（>%d 天）", m.ID, len(ids), days)
				}
			}
			ids, err := a.Store.EvictCheckpointsOver(m.ID,
				int64(a.Cfg.SnapshotMaxMB)<<20, int64(a.Cfg.SnapshotManualMaxMB)<<20)
			if err != nil {
				log.Printf("[快照] 会话 %s 限量驱逐失败: %v", m.ID, err)
			} else if len(ids) > 0 {
				tctx.deleteSnapshotFiles(ids)
				log.Printf("[快照] 会话 %s 超量，已按文件驱逐最旧快照 %d 条", m.ID, len(ids))
			}
		}
	}
	time.Sleep(time.Minute)
	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for range t.C {
		run()
	}
}
