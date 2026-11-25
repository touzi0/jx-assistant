package main

// 演示记录器：操作录制器（界面名）的材料后端，归用户直控——输入区的录制按钮/
// 胶囊经 HTTP 端点（/api/demo-record/*，见 server.go）起录/停录，模型不持有录制
// 工具；停录端点把【演示记录】通知随响应返回，前端把材料作为一张卡挂进输入框
// 上方的附件区，随用户下一条消息发给模型；模型 read INDEX.md 定位关键步骤、
// read_image 看截图，即可还原用户做了什么。
// 起录后全局鼠标/键盘钩子在每次点击、回车/Tab、窗口切换时自动截屏（画面没变
// 则跳过），全程记事件日志与麦克风音频；停录收录成材料目录（截图序列 +
// events.jsonl + audio.wav + 讲解.srt + INDEX.md 对照表）。
// **停录不等语音识别**：停录只做停钩子、停采集、写 audio.wav（秒级返回）；
// 随录随转还没识别完的段由识别 worker 在后台继续，写完 讲解.srt 再重建
// INDEX.md 并广播 demo_record_transcribed（recorderASRDone 钩子）。首次用语音
// 时本地模型要加载，识别可能滞后几十秒——首次停录曾因此卡 8 秒。
// 字幕没出来/不全时的补转不进客户端（没有模型工具）：通知与提示词只提醒模型
//「语音组件里有本地识别脚本，可自己调 Python 对 audio.wav 重新识别」。
// 实现按平台分文件（recorder_windows.go 真实现，recorder_other.go 空桩），
// 与 computer_* 组件同一组织方式；VAD 分段、SRT、INDEX 等纯逻辑在本文件跨平台。

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 转写接线位 ----------

// recorderASR 把一段 16kHz/单声道/16bit WAV 交给语音组件的本地识别 worker
// （voiceasr.go 在 NewApp 注册；语音组件停用时调用返回错误＝该段字幕丢弃，
// 不影响录制本体）。演示记录随录随转：VAD 按停顿切段（recorder_windows.go），
// 每段经这里识别成文字，逐段落 讲解.srt——时间轴与事件/截图同源。测试环境
// 不注册（nil）＝纯录音、无字幕，行为与接组件前完全一致。
var recorderASR func(wav []byte) (string, error)

// recorderASRDone 停录后后台识别收尾的回调（server.go 在 NewApp 注册）：识别
// worker 把停录时在途的段全部转完（讲解.srt 写完）后调用，参数是该次记录的
// 统计与最终字幕段数——回调里重建 INDEX.md 并广播 demo_record_transcribed，
// 前端据此把输入框上方材料卡的「语音转写中」换成字幕段数。测试环境不注册。
var recorderASRDone func(stats *recorderStats, cues int)

// ---------- 材料 目录名约定 ----------

const (
	recorderBaseDir   = "演示记录"         // 工作区下的记录根目录
	recorderEventsFn  = "events.jsonl" // 事件日志（每行一条 JSON）
	recorderAudioFn   = "audio.wav"    // 麦克风录音（16kHz 单声道）
	recorderSrtFn     = "讲解.srt"       // 语音转写字幕（转写服务接入后产出）
	recorderIndexFn   = "INDEX.md"     // 材料对照表（stop 时生成）
	recorderMaxShots  = 400            // 单次记录截图上限（防磁盘失控）
	recorderDirPrefix = "record-"      // 记录目录前缀，后接时间戳
)

// recorderStats 是一次记录收尾时的统计（INDEX 与工具返回共用）。
type recorderStats struct {
	Dir     string
	MS      int
	Events  int
	Shots   int
	AudioOK bool
	// ASRPending：停录时随录随转还有段没识别完（识别在后台继续，完成后经
	// recorderASRDone 回调收尾）；Cues 是停录那一刻已写出的字幕段数。
	ASRPending bool
	Cues       int
	asrDone    chan struct{} // 音频会话的识别 worker 退出信号（nil = 无音频会话）
}

// recEvent 是事件日志（events.jsonl）里的一行。T 为相对记录起点的毫秒数
// （与 audio.wav / SRT 的时间轴同源对齐）。VK/Up 是钩子回调投递键盘原始
// 事件的内部字段，worker 聚合改写成 text/key 事件后不会落盘。
type recEvent struct {
	T      int    `json:"t"`
	Type   string `json:"type"` // click / wheel / text / key / window / shot
	Button string `json:"button,omitempty"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	Text   string `json:"text,omitempty"`
	Key    string `json:"key,omitempty"`
	Title  string `json:"title,omitempty"`
	Delta  int    `json:"delta,omitempty"`
	Shot   string `json:"shot,omitempty"`
	VK     uint32 `json:"vk,omitempty"`
	Up     bool   `json:"up,omitempty"`
}

// ---------- 用户侧入口的支撑函数（HTTP 端点见 server.go） ----------

// recorderRecentDirs 列出工作区 演示记录/ 下已收录的记录目录，按目录名倒序
//（名字含时间戳，倒序＝新→旧）取前 n 条。建技能会话时的【演示记录】清单
// 通知用它告诉模型有哪些现成材料可分析——首页「先录后聊」的入口。
func recorderRecentDirs(ws string, n int) []string {
	if ws == "" || n <= 0 {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(ws, recorderBaseDir))
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), recorderDirPrefix) {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	if len(dirs) > n {
		dirs = dirs[:n]
	}
	return dirs
}

// recorderShutdown 进程收尾兜底：还在录就停录（保存 audio.wav）并生成
// INDEX.md，避免中途退出丢掉整段材料。桌面壳退出路径调用；没在录时
// 是空操作。后台识别若还在跑，等一小会儿（recASRShutdownWait）让它把在途
// 的段转完再退——进程一退 worker 就没了，这几秒是字幕的最后机会。
func recorderShutdown() {
	stats, err := recorderStop()
	if err != nil || stats == nil {
		return
	}
	if stats.asrDone != nil {
		select {
		case <-stats.asrDone:
		case <-time.After(recASRShutdownWait):
		}
	}
	_ = buildRecorderIndex(stats)
}

const recASRShutdownWait = 8 * time.Second // 退出时等后台识别的上限

// demoRecordStopNote 停录端点随响应返回的【演示记录】通知正文：前端把材料作为一张
// 卡挂进输入框上方的附件区，发送时注记进用户消息开头的「用户附件：」块（与文件
// 上传注记同一通路，首页没开会话时录的也跟着第一条消息走）。**必须单行**：「用户
// 附件：」块的注记按行计、一行份材料，前端把每行折成一条灰条（只露前 10 字、
// 悬停看全文，与引用行同款）——多行会被拆成好几条灰条。字幕一句按实况分四种：
// 已出 / 后台识别中（停录不等识别）/ 没识别到 / 没音频；需要补转的只提醒模型
//「语音组件里有本地识别脚本，可自己调 Python 对 audio.wav 重新识别」——不给路径、
// 不加工具，模型照环境提示词里的客户端位置自己找（用户定的边界）。
func demoRecordStopNote(stats *recorderStats) string {
	srtNote := demoRecordSrtNote(stats)
	return fmt.Sprintf("【演示记录】用户点停止录制完成了一段操作演示，材料已收录：目录 %s；时长 %s，事件 %d 条，截图 %d 张；音频：%s；字幕：%s。"+
		"下一步：read %s 看时间线对照表，再按编号 read_image 看关键截图（所在模式没有 read_image 就派子对话去看）；"+
		"用户想让你拿这段操作做什么，以他在对话里的说明为准（分析问题、写教程、沉淀技能都有可能）；"+
		"用户当时的口头讲解若没进对话，请他口述关键步骤的意图，不要猜。",
		filepath.ToSlash(stats.Dir),
		(time.Duration(stats.MS) * time.Millisecond).Round(time.Second),
		stats.Events, stats.Shots,
		map[bool]string{true: recorderAudioFn + " 已保存", false: "未录到（无可用麦克风）"}[stats.AudioOK],
		srtNote, filepath.ToSlash(filepath.Join(stats.Dir, recorderIndexFn)))
}

// demoRecordSrtNote 通知里的字幕一句（按材料实况；与 demoRecordStopNote 同一
// 判定供后台识别完成后的广播复用）。补转提示只提「语音组件」：它的识别脚本在
// 组件目录里，模型按环境提示词里的客户端位置自己找。
func demoRecordSrtNote(stats *recorderStats) string {
	if !stats.AudioOK {
		return "无音频可转写（没录到麦克风），讲解以用户在对话里说的为准"
	}
	if stats.ASRPending {
		return "后台识别中（停录不等识别：通常几十秒内写出 " + recorderSrtFn + " 并更新 INDEX.md；若稍后 read 不到或想要完整讲解词，语音组件里有本地识别脚本，可自己调 Python 对 " + recorderAudioFn + " 重新识别）"
	}
	if n := len(recorderReadCues(stats.Dir)); n > 0 {
		return fmt.Sprintf("已生成 %s（%d 段，随录随转）", recorderSrtFn, n)
	}
	if recorderASR == nil {
		return "未生成（语音组件未启用）；需要讲解词可在设置里启用语音组件后，用它的识别脚本对 " + recorderAudioFn + " 重新识别"
	}
	return "未生成（本次没识别到有效语音；需要讲解词可用语音组件的识别脚本对 " + recorderAudioFn + " 重新识别）"
}

// recorderReadCues 读材料目录里已落盘的字幕段（没有/解析失败返回空）。
func recorderReadCues(dir string) []srtCue {
	data, err := os.ReadFile(filepath.Join(dir, recorderSrtFn))
	if err != nil {
		return nil
	}
	return parseSRT(string(data))
}

// demoRecordListNote 建技能会话时的【演示记录】清单通知正文：工作区已有
// 现成演示材料时告诉模型有哪些可分析（dirs 为 recorderRecentDirs 的结果）。
func demoRecordListNote(dirs []string) string {
	names := make([]string, len(dirs))
	for i, d := range dirs {
		names[i] = recorderBaseDir + "/" + d
	}
	return "【演示记录】工作区已有 " + strconv.Itoa(len(dirs)) + " 份演示材料（按时间倒序）：" +
		strings.Join(names, "、") +
		"。每份含 INDEX.md 时间线对照表、编号截图、events.jsonl 事件日志与 audio.wav 录音（讲解字幕 讲解.srt 随录随转；某份没有或读不到字幕时，语音组件里有本地识别脚本，可自己调 Python 对它重新识别）。" +
		"用户要分析某份时先 read 它的 INDEX.md，再按编号 read_image 看关键截图。"
}

// ---------- SRT 解析与 INDEX 生成（跨平台共用） ----------

// srtCue 是一条字幕：毫秒区间 + 文本。
type srtCue struct {
	StartMs int
	EndMs   int
	Text    string
}

// parseSRT 解析标准 SRT 字幕（块间空行分隔，时间行 HH:MM:SS,mmm --> …）。
// 解析宽松：坏块跳过，不整体失败。
func parseSRT(data string) []srtCue {
	var cues []srtCue
	for _, block := range strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}
		// 找时间行（有的块第一行是序号，有的直接是时间）
		ti := 0
		if strings.Contains(lines[0], "-->") {
			ti = 0
		} else if len(lines) > 1 && strings.Contains(lines[1], "-->") {
			ti = 1
		} else {
			continue
		}
		segs := strings.SplitN(lines[ti], "-->", 2)
		if len(segs) != 2 {
			continue
		}
		s, ok1 := parseSrtTime(strings.TrimSpace(segs[0]))
		e, ok2 := parseSrtTime(strings.Split(strings.TrimSpace(segs[1]), " ")[0])
		if !ok1 || !ok2 {
			continue
		}
		text := strings.TrimSpace(strings.Join(lines[ti+1:], " "))
		if text == "" {
			continue
		}
		cues = append(cues, srtCue{StartMs: s, EndMs: e, Text: text})
	}
	return cues
}

func parseSrtTime(s string) (int, bool) {
	// HH:MM:SS,mmm（毫秒分隔符逗号/点都认）
	s = strings.Replace(s, ".", ",", 1)
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	secParts := strings.Split(parts[2], ",")
	sec, err3 := strconv.Atoi(secParts[0])
	ms := 0
	var err4 error
	if len(secParts) > 1 {
		ms, err4 = strconv.Atoi(secParts[1])
		if err4 == nil && len(secParts[1]) == 1 {
			ms *= 100
		} else if err4 == nil && len(secParts[1]) == 2 {
			ms *= 10
		}
	}
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return 0, false
	}
	return ((h*60+m)*60+sec)*1000 + ms, true
}

// writeSRTFile 把字幕段写成标准 SRT（序号/时间码/文本，块间空行）。随录随转
// 用：每出一段整文件重写（段数小、代价可忽略），中途崩溃已落盘的段仍在。
func writeSRTFile(path string, cues []srtCue) error {
	var b strings.Builder
	for i, c := range cues {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", i+1, srtTimecode(c.StartMs), srtTimecode(c.EndMs), strings.TrimSpace(c.Text))
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

// srtTimecode 毫秒 → SRT 时间码 HH:MM:SS,mmm。
func srtTimecode(ms int) string {
	if ms < 0 {
		ms = 0
	}
	h, m, s := ms/3600000, ms/60000%60, ms/1000%60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms%1000)
}

// recPcmToWav 给裸 PCM（16kHz/单声道/16bit，录制器的采集格式）包 RIFF 头，
// 供识别 worker 消费（worker 按头自解析采样率与声道）。
func recPcmToWav(pcm []byte) []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	recLe32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	recLe32(hdr[16:], 16) // fmt 块长
	hdr[20], hdr[22] = 1, 1
	recLe32(hdr[24:], 16000) // 采样率
	recLe32(hdr[28:], 32000) // 字节率
	recLe16(hdr[32:], 2)     // 块对齐
	recLe16(hdr[34:], 16)    // 位深
	copy(hdr[36:40], "data")
	recLe32(hdr[40:], uint32(len(pcm)))
	return append(hdr, pcm...)
}

func recLe16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }

func recLe32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// ---------- VAD 停顿分段（纯 PCM 逻辑：采集循环与文件重放共用） ----------
//
// 一次采集两用：PCM 全量累积（停录写 audio.wav）＋ 喂 VAD 停顿分段。VAD 分段
// 语义与语音组件的听写线（组件/voice/client.js 的 makeLocalRec）逐常量对齐：
// 350ms 起效 / 3s 尾静音定稿 / 18s 强制切段 / 静音垫 1.5s 上限，能量判据
// rms > max(0.008, 底噪×3.5)、底噪自适应（0.98/0.02，上限 0.02）。定稿段经
// recorderASR（voiceasr.go → 语音组件本地模型）识别成文字，逐段落 讲解.srt
//（时间轴与事件/截图同源：都以记录起点为 0）。

const (
	recFrameBytes   = 3200  // 100ms @ 16kHz/16bit/单声道 —— VAD 帧长=缓冲长
	recFrameMs      = 100   // 帧长（毫秒）
	recSpeechMinMs  = 350   // 短于此视为噪声毛刺，丢弃
	recFinalPauseMs = 3000  // 尾随静音满 3 秒才定稿（真停顿，非说话时长）
	recSegMaxMs     = 18000 // 连续不停顿超 18 秒强制定稿
	recPadMaxMs     = 1500  // 段内静音垫上限（间隙+尾部，近似停顿中点切分）
)

// recSegment VAD 定稿的一段：音频（WAV，供识别 worker 按头解析）与首尾毫秒
//（cue 时间在定稿时就定好——识别只回文本）。dropped=true 表示语音不足
//（噪声毛刺/不足 100ms）丢弃，不进识别。
type recSegment struct {
	wav     []byte
	startMs int
	endMs   int
	dropped bool
}

// recVAD 能量 VAD 分段状态机：每帧（100ms）喂 feed，定稿时返回该段。时间轴以
// 第一帧起点为 0：采集循环按设备缓冲一帧一帧喂，文件重放按同样帧长切着喂，
// 两种来源切出来的段一致。状态只归喂它的那一方访问（采集循环 / 文件重放）。
type recVAD struct {
	noiseFloor     float64
	sinceFinal     []byte // 定稿段音频（语音帧 + 限量静音垫）
	sinceFinalMs   int    // 段内语音时长
	finalSilenceMs int    // 尾随静音计时
	padMs          int    // 段内已收录的静音垫
	nowMs          int    // 已喂入的时长（当前帧的帧末时刻）
	lastSpeechMs   int    // 最近一帧语音的帧末时刻（-1 = 还没有语音）
	prevEndMs      int    // 上一段结束（下一段的起点）
}

func newRecVAD() *recVAD { return &recVAD{noiseFloor: 0.004, lastSpeechMs: -1} }

// feed 喂一帧 100ms 的 PCM（recFrameBytes 字节），定稿返回段，否则返回 nil。
func (v *recVAD) feed(data []byte) *recSegment {
	rms := recRMS(data)
	isSpeech := rms > math.Max(0.008, v.noiseFloor*3.5)
	if !isSpeech {
		v.noiseFloor = math.Min(0.02, v.noiseFloor*0.98+rms*0.02)
	}
	if isSpeech {
		v.sinceFinal = append(v.sinceFinal, data...)
		v.sinceFinalMs += recFrameMs
		v.finalSilenceMs = 0
		v.lastSpeechMs = v.nowMs + recFrameMs
	} else if v.sinceFinalMs > 0 {
		v.finalSilenceMs += recFrameMs
		if v.padMs < recPadMaxMs { // 间隙/尾部静音限量入段
			v.sinceFinal = append(v.sinceFinal, data...)
			v.padMs += recFrameMs
		}
	}
	v.nowMs += recFrameMs
	if v.sinceFinalMs >= recSegMaxMs || (v.sinceFinalMs >= recSpeechMinMs && v.finalSilenceMs >= recFinalPauseMs) {
		return v.finalize()
	}
	return nil
}

// finalize 定稿当前段：cue 终点取本段最后一帧语音+300ms（尾静音不计入），起点
// 接上一段结束。停录收尾也调一次（尾段兜底：余下语音没有 3s 静音也照转）；
// 没有待定稿内容时返回 dropped 段。
func (v *recVAD) finalize() *recSegment {
	frames, spoken := v.sinceFinal, v.sinceFinalMs
	endMs := 300
	if v.lastSpeechMs >= 0 {
		endMs = v.lastSpeechMs + 300
	}
	startMs := v.prevEndMs
	v.sinceFinal, v.sinceFinalMs, v.finalSilenceMs, v.padMs = nil, 0, 0, 0
	v.prevEndMs = endMs
	if spoken < recSpeechMinMs || len(frames) < recFrameBytes { // 噪声毛刺/音频不足 100ms：丢弃
		return &recSegment{dropped: true}
	}
	return &recSegment{wav: recPcmToWav(frames), startMs: startMs, endMs: endMs}
}

// recRMS 归一化均方根（int16 LE → [0,1)），VAD 能量判据的输入。
func recRMS(b []byte) float64 {
	n := len(b) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i+1 < len(b); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(b[i:])))
		sum += v * v
	}
	return math.Sqrt(sum/float64(n)) / 32768
}

// buildRecorderIndex 生成 INDEX.md：事件时间线 + 截图编号 + 对应字幕段对照表。
func buildRecorderIndex(stats *recorderStats) error {
	raw, err := os.ReadFile(filepath.Join(stats.Dir, recorderEventsFn))
	if err != nil {
		return err
	}
	var cues []srtCue
	if srtData, err := os.ReadFile(filepath.Join(stats.Dir, recorderSrtFn)); err == nil {
		cues = parseSRT(string(srtData))
	}
	rel, _ := filepath.Rel(filepath.Dir(filepath.Dir(stats.Dir)), stats.Dir)

	var b strings.Builder
	fmt.Fprintf(&b, "# 演示记录对照表\n\n")
	fmt.Fprintf(&b, "- 目录：`%s`（相对工作区）\n", filepath.ToSlash(rel))
	fmt.Fprintf(&b, "- 时长约 %s；事件 %d 条，截图 %d 张\n",
		(time.Duration(stats.MS) * time.Millisecond).Round(time.Second), stats.Events, stats.Shots)
	if stats.AudioOK {
		fmt.Fprintf(&b, "- 音频：`%s`（麦克风全程录音）\n", recorderAudioFn)
	} else {
		fmt.Fprintf(&b, "- 音频：未录到（无可用麦克风）\n")
	}
	if len(cues) > 0 {
		fmt.Fprintf(&b, "- 字幕：`%s`（%d 段，时间与事件轴同源对齐）\n", recorderSrtFn, len(cues))
	} else if stats.ASRPending {
		fmt.Fprintf(&b, "- 字幕：后台识别中（停录不等识别，这份对照表生成时还有段没转完；稍后重读 %s 与本文件，或用语音组件的识别脚本对 %s 重新识别）\n", recorderSrtFn, recorderAudioFn)
	} else {
		fmt.Fprintf(&b, "- 字幕：未生成（没识别到有效语音或语音组件未启用；需要讲解词可用语音组件的识别脚本对 %s 重新识别）；用户在对话里说的话就是讲解词\n", recorderAudioFn)
	}
	b.WriteString("\n说明：时间轴以记录起点为 0（毫秒）。截屏发生在点击、回车/Tab、窗口切换时；" +
		"连续打字聚合成一条 text 事件（中文输入法的上屏结果以截图为准，事件里只有原始按键）。" +
		"看图用 read_image，路径 = 上面的目录 + 截图文件名。\n\n")
	b.WriteString("| 时间 | 事件 | 截图 | 讲解 |\n|---|---|---|---|\n")

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev recEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		shot := ""
		if ev.Shot != "" {
			shot = ev.Shot
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			fmtMS(ev.T), describeRecEvent(&ev), shot, subtitleNear(cues, ev.T))
	}
	return os.WriteFile(filepath.Join(stats.Dir, recorderIndexFn), []byte(b.String()), 0644)
}

// subtitleNear 找覆盖/紧邻事件时刻的字幕文本（前后 3.5 秒窗口），多段去重拼接。
func subtitleNear(cues []srtCue, tMs int) string {
	var picked []string
	seen := map[string]bool{}
	for _, c := range cues {
		if tMs >= c.StartMs-3500 && tMs <= c.EndMs+3500 {
			txt := strings.ReplaceAll(c.Text, "|", "／")
			if !seen[txt] {
				seen[txt] = true
				picked = append(picked, txt)
			}
		}
		if len(picked) >= 3 {
			break
		}
	}
	return strings.Join(picked, " / ")
}

func describeRecEvent(ev *recEvent) string {
	switch ev.Type {
	case "click":
		return fmt.Sprintf("点击 (%d, %d) %s键", ev.X, ev.Y, ev.Button)
	case "wheel":
		return fmt.Sprintf("滚轮 %s %d 格", map[bool]string{true: "向上", false: "向下"}[ev.Delta > 0], absInt(ev.Delta))
	case "text":
		return "输入 " + strconv.Quote(ev.Text)
	case "key":
		return "按键 " + ev.Key
	case "window":
		return "切换窗口 → " + strings.ReplaceAll(ev.Title, "|", "／")
	case "shot":
		return "截屏（画面变化）"
	}
	return ev.Type
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func fmtMS(ms int) string {
	d := time.Duration(ms) * time.Millisecond
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d", m, s)
}

// sortRecEvents 供测试/内部使用的事件排序（按时间稳定排序）。
func sortRecEvents(evs []recEvent) {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].T < evs[j].T })
}
