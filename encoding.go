package main

// 文本编码探测与判定（全项目唯一真源）。
//
// 为什么需要它：PowerShell 5.1 的 Get-Content/Select-String 默认按 ANSI(936)
// 解文件，UTF-8 无 BOM 的文本必然乱码；而"UTF-8 字节被 GBK 解码"得到的是
// 合法 UTF-8 字符串（閿€鍞…），输出侧的解码兜底看不出来。所以必须在**读之前**
// 按真实编码去解：命令里出现的文件先探测、read/edit 读文件也探测。
//
// 判定顺序固定且互斥（无歧义）：BOM → 严格 UTF-8 → GBK/GB18030 结构 →
// UTF-16 启发 → 二进制/未知。GBK 结构校验自己写而不用"解码是否报错"——
// x/text 的 GBK 解码器对非法字节静默丢字、不返回错误（实测 0xFF 0xFF → 空串），
// 拿它当探针会把任意二进制都认成 GBK。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
)

// TextEncoding 是探测出来的文件编码。零值即 UTF-8（无 BOM），
// 让"默认 UTF-8"成为结构的默认行为。
type TextEncoding int

const (
	EncUTF8    TextEncoding = iota // UTF-8 无 BOM（默认）
	EncUTF8BOM                     // UTF-8 带 BOM
	EncGBK                         // GBK/GB2312 双字节（含 ASCII）
	EncGB18030                     // 出现 GB18030 四字节序列
	EncUTF16LE                     // UTF-16 小端（带或不带 BOM）
	EncUTF16BE                     // UTF-16 大端（带或不带 BOM）
	EncBinary                      // 二进制（含 NUL 且不像 UTF-16 文本）
	EncUnknown                     // 既非合法 UTF-8 也非合法 GB 族：按 GB18030 宽松解，调用方注记
)

func (e TextEncoding) String() string {
	switch e {
	case EncUTF8:
		return "UTF-8"
	case EncUTF8BOM:
		return "UTF-8(BOM)"
	case EncGBK:
		return "GBK"
	case EncGB18030:
		return "GB18030"
	case EncUTF16LE:
		return "UTF-16LE"
	case EncUTF16BE:
		return "UTF-16BE"
	case EncBinary:
		return "二进制"
	default:
		return "未知编码"
	}
}

// needsPreserveBytes 表示 write 整体覆盖时会改变编码，需要提示用户/模型。
func (e TextEncoding) needsPreserveBytes() bool {
	return e == EncGBK || e == EncGB18030 || e == EncUTF16LE || e == EncUTF16BE || e == EncUTF8BOM
}

var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

// detectTextEncoding 探测给定字节块的编码（调用方负责只喂文件头部）。
// 顺序：BOM → 含 NUL（UTF-16 或二进制）→ 严格 UTF-8 → GB 族结构 → 未知。
// 含 NUL 必须排在 utf8.Valid 之前：NUL 在 UTF-8 里是合法字符，但正常文本
// 文件不会带 NUL，带了就只能是 UTF-16 或二进制。
func detectTextEncoding(b []byte) TextEncoding {
	switch {
	case len(b) == 0:
		return EncUTF8
	case bytes.HasPrefix(b, bomUTF8):
		return EncUTF8BOM
	case bytes.HasPrefix(b, bomUTF16LE):
		return EncUTF16LE
	case bytes.HasPrefix(b, bomUTF16BE):
		return EncUTF16BE
	case bytes.IndexByte(b, 0) >= 0:
		if e := utf16Guess(b); e != EncUnknown {
			return e
		}
		return EncBinary
	case utf8.Valid(b):
		return EncUTF8
	}
	if ok, hasFour := gbkStructure(b); ok {
		if hasFour {
			return EncGB18030
		}
		return EncGBK
	}
	return EncUnknown
}

// gbkStructure 校验字节能否完整切分成 GBK 双字节 / GB18030 四字节序列。
// ASCII 区（0x00-0x7F）两族共用；0x80/0xFF 在 GBK 中未定义，直接判否。
func gbkStructure(b []byte) (ok bool, hasFour bool) {
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		if c == 0x80 || c == 0xFF {
			return false, false
		}
		if i+3 < len(b) && b[i+1] >= 0x30 && b[i+1] <= 0x39 {
			if b[i+2] < 0x81 || b[i+2] > 0xFE || b[i+3] < 0x30 || b[i+3] > 0x39 {
				return false, false
			}
			hasFour = true
			i += 4
			continue
		}
		if i+1 >= len(b) {
			return false, false
		}
		t := b[i+1]
		if t < 0x40 || t == 0x7F || t > 0xFE {
			return false, false
		}
		i += 2
	}
	return true, hasFour
}

// utf16Guess 用「高位字节多为 0」的统计判定 UTF-16 字节序（仅无 BOM 时需要）。
func utf16Guess(b []byte) TextEncoding {
	limit := len(b)
	if limit > 4096 {
		limit = 4096
	}
	pairs := limit / 2
	if pairs < 2 {
		return EncUnknown
	}
	var even, odd int
	for i := 0; i+1 < limit; i += 2 {
		if b[i] == 0 {
			even++
		}
		if b[i+1] == 0 {
			odd++
		}
	}
	switch {
	case odd*4 > pairs*3: // 小端：偶数位是低字节、奇数位是高字节
		return EncUTF16LE
	case even*4 > pairs*3:
		return EncUTF16BE
	}
	return EncUnknown
}

// decodeText 按探测结果把字节解成 UTF-8 文本，返回文本与编码。
// 只解一次、不做二次转换：这是 read 与 edit 的共同入口。
func decodeText(b []byte) (string, TextEncoding) {
	enc := detectTextEncoding(b)
	switch enc {
	case EncUTF8BOM:
		return string(b[len(bomUTF8):]), enc
	case EncUTF16LE, EncUTF16BE:
		return decodeUTF16(b, enc), enc
	case EncGBK:
		if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return string(b), EncUnknown
	case EncGB18030:
		if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return string(b), EncUnknown
	case EncUnknown:
		// 未知编码按 GB18030 宽松解（中文环境最可能的来源），失败再兜底替换
		if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return strings.ToValidUTF8(string(b), "\uFFFD"), enc
	default: // EncUTF8 / EncBinary
		return string(b), enc
	}
}

// decodeUTF16 解 UTF-16（带 BOM 或不带）。
func decodeUTF16(b []byte, enc TextEncoding) string {
	if enc == EncUTF16LE && bytes.HasPrefix(b, bomUTF16LE) {
		b = b[2:]
	}
	if enc == EncUTF16BE && bytes.HasPrefix(b, bomUTF16BE) {
		b = b[2:]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if enc == EncUTF16LE {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		} else {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
	}
	return string(utf16.Decode(u))
}

// encodeText 按目标编码把文本编回字节（edit 写回用）。不可表示字符直接报错，
// 绝不静默替换成 '?'——替换一次就永久丢字，比报错难查得多。
func encodeText(s string, enc TextEncoding) ([]byte, error) {
	switch enc {
	case EncUTF8BOM:
		return append(append([]byte{}, bomUTF8...), []byte(s)...), nil
	case EncUTF16LE:
		out := append([]byte{}, bomUTF16LE...)
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u), byte(u>>8))
		}
		return out, nil
	case EncUTF16BE:
		out := append([]byte{}, bomUTF16BE...)
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u>>8), byte(u))
		}
		return out, nil
	case EncGBK:
		b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		if err != nil {
			return nil, errors.New("内容含 GBK 无法表示的字符（如 emoji），已中止写入：请改用 write 以 UTF-8 保存，或先去掉该字符")
		}
		return b, nil
	case EncGB18030:
		b, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(s))
		if err != nil {
			return nil, errors.New("内容含 GB18030 无法表示的字符，已中止写入：请改用 write 以 UTF-8 保存")
		}
		return b, nil
	default:
		// UTF-8 / Unknown：按 UTF-8 落盘（Unknown 已被解码为 UTF-8 文本）
		return []byte(s), nil
	}
}

// probeTextFile 读文件头部（64KB）探测编码；文件不存在返回 EncUnknown。
func probeTextFile(path string) TextEncoding {
	f, err := os.Open(path)
	if err != nil {
		return EncUnknown
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	return detectTextEncoding(buf[:n])
}

const mojibakeSample = "銆锛鈥鍜閿鑴娴鏂绋缂鍐鑻鏈宸璁鏄鍚瀹鎴缁鍒鏃鍙锟"

// looksMojibake 判断文本是否已经是"乱码态"（两种来源）：
//  1. 含 U+FFFD：某次有损解码已经把字节换成替换符，**不可逆**，必须重读；
//  2. 出现 ≥2 个 UTF-8→GBK 误码的高频字：正常中文文本里这些字几乎不出现，
//     阈值 2 是为了放过偶发的单字（如引文、人名用字）。
func looksMojibake(s string) bool {
	if strings.ContainsRune(s, utf8.RuneError) {
		return true
	}
	hits := 0
	for _, r := range mojibakeSample {
		if strings.ContainsRune(s, r) {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

var (
	reWinPath     = regexp.MustCompile(`(?i)\b[A-Z]:[\\/][^"'|<>;&\r\n]*`)
	reQuoted      = regexp.MustCompile(`"([^"\r\n]+)"|'([^'\r\n]+)'`)
	reBarePath    = regexp.MustCompile(`(?i)[^\s"'|<>;&]+\.(?:txt|md|sql|java|kt|go|ts|vue|js|cjs|mjs|json|ya?ml|xml|csv|bat|cmd|ps1|py|log|ini|conf|properties|html|css|scss|toml|gradle|kts|sh)`)
	reWriteRedir  = regexp.MustCompile(`[^0-9&]>`)
	reCmdSplitter = regexp.MustCompile(`[;|\r\n]+|&&|\|\|`)
	maxProbeNum   = 8 // 一条命令里最多探测几个文件：够覆盖常见读法，又不让每条命令多花时间
)

// extractPathCandidates 从命令串里捞出可能被读取的文件路径（去重、只留真实存在的
// 普通文件）。只认"命令里字面出现"的路径——程序内部自己读的文件探测不到，那条路
// 由输出乱码检测 + 重跑兜底。
func extractPathCandidates(command, cwd string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	var raw []string
	raw = append(raw, reWinPath.FindAllString(command, -1)...)
	for _, m := range reQuoted.FindAllStringSubmatch(command, -1) {
		if m[1] != "" {
			raw = append(raw, m[1])
		} else if m[2] != "" {
			raw = append(raw, m[2])
		}
	}
	raw = append(raw, reBarePath.FindAllString(command, -1)...)
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(strings.Trim(p, `"';,`))
		if p == "" || strings.ContainsAny(p, "*?<>|") || strings.HasPrefix(p, "-") {
			continue
		}
		// 去掉尾随的 PowerShell 参数/标点残留（如 file.txt, 或 file.txt)）
		p = strings.TrimRight(p, "):,;")
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxProbeNum {
			break
		}
	}
	return out
}

// 只读命令白名单：命令被「; | && || 换行」拆段后，每段的第一个动词都必须在这里。
var readOnlyVerbs = map[string]bool{
	"get-content": true, "gc": true, "cat": true, "type": true, "more": true,
	"head": true, "tail": true, "select-string": true, "sls": true, "findstr": true,
	"grep": true, "rg": true, "dir": true, "ls": true, "get-childitem": true, "gci": true,
	"test-path": true, "get-item": true, "gi": true, "resolve-path": true,
	"measure-object": true, "sort-object": true, "select-object": true, "where-object": true,
	"format-table": true, "format-list": true, "out-string": true, "out-host": true,
	"write-output": true, "write-host": true, "echo": true, "cd": true, "set-location": true,
	"pwd": true, "get-location": true, "whoami": true, "hostname": true, "date": true,
	"git": true, "go": true, "npm": true, "node": true, "java": true, "python": true, "py": true,
	"mvn": true, "gradle": true, "which": true, "where": true, "ver": true, "uname": true,
}

// 写/副作用特征：出现即判定"非只读"，绝不自动重跑（重跑等于执行两次）。
var writeHints = []string{
	"out-file", "set-content", "add-content", "clear-content", "new-item", "remove-item",
	"copy-item", "move-item", "rename-item", "set-itemproperty", "start-process", "stop-process",
	"taskkill", "kill ", "rm ", "rmdir", "del ", "erase ", "move ", "mv ", "copy ", "cp ",
	"mkdir", "md ", "tee ", "chmod", "chown", "touch", "install", "uninstall", "update",
	"commit", "push", "pull", "fetch", "merge", "rebase", "reset", "checkout", "clean",
	"build", "package", "publish", "deploy", "start ", "restart", "stop ", "enable",
	"disable", "import-csv", "export-csv", "convertto-json ", "set-", "invoke-webrequest",
	"invoke-restmethod", "curl ", "wget ", "scp ", "ssh ", "docker ", "systemctl",
}

// isReadOnlyCommand 判定命令是否可安全重跑一次（只读、无写副作用）。
// 保守原则：拿不准就返回 false——宁可少重跑一次，也不能让命令执行两遍。
func isReadOnlyCommand(command string) bool {
	low := strings.ToLower(command)
	for _, w := range writeHints {
		if strings.Contains(low, w) {
			return false
		}
	}
	// 重定向写文件（含 >>），排除 2>&1 这类句柄复制
	if reWriteRedir.MatchString(command) {
		return false
	}
	// 拆段：每段第一个动词必须在只读白名单里
	seen := false
	for _, seg := range reCmdSplitter.Split(command, -1) {
		seg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(seg), "&"))
		if seg == "" {
			continue
		}
		// 跳过 $env:X='y' / $v=... 之类的赋值段
		if strings.HasPrefix(seg, "$") && strings.Contains(seg, "=") {
			continue
		}
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		verb := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
		verb = strings.TrimSuffix(verb, ".exe")
		if !readOnlyVerbs[verb] {
			return false
		}
		seen = true
	}
	return seen
}

// 文件级"读面编码"：告诉 PS 5.1 该按 UTF-8 还是按 ANSI(936) 读文件。
const (
	encodingModeUTF8 = "utf8"
	encodingModeGBK  = "gbk"
)

// detectEncodingMode 决定一条 PS 5.1 命令该用哪种读面编码：
//   - 命令里出现的文本文件全是 GB 族 → gbk（含 .bat/.cmd 这类老文件）
//   - 全是 UTF-8 族 / 没有可探测文件 → utf8（默认）
//   - 混在一起 → utf8（多数资产是 UTF-8），由输出乱码检测 + 重跑兜底
//
// 返回的 hint 只在混合/未知时给出（正常情况为空，不制造噪音）。
func detectEncodingMode(cwd, command, shell string) (mode string, hint string) {
	if !isPS51Shell(shell) {
		return encodingModeUTF8, ""
	}
	paths := extractPathCandidates(command, cwd)
	if len(paths) == 0 {
		return encodingModeUTF8, ""
	}
	var gbFiles, utf8Files, otherFiles []string
	for _, p := range paths {
		switch probeTextFile(p) {
		case EncGBK, EncGB18030:
			gbFiles = append(gbFiles, p)
		case EncUTF8, EncUTF8BOM, EncUTF16LE, EncUTF16BE:
			utf8Files = append(utf8Files, p)
		default:
			otherFiles = append(otherFiles, p)
		}
	}
	switch {
	case len(gbFiles) > 0 && len(utf8Files) == 0:
		return encodingModeGBK, ""
	case len(gbFiles) > 0 && len(utf8Files) > 0:
		return encodingModeUTF8, "命令同时涉及 UTF-8 与 GBK 文件（" + filepath.Base(gbFiles[0]) +
			" 是 " + probeTextFile(gbFiles[0]).String() + "）：本条按 UTF-8 读；读该文件请加 -Encoding Default 或改用 read 工具"
	case len(otherFiles) > 0 && len(utf8Files) == 0:
		return encodingModeUTF8, "命令里的文件编码无法确定（" + filepath.Base(otherFiles[0]) +
			"）：按 UTF-8 读取，遇到乱码会自动换编码重跑一次"
	}
	return encodingModeUTF8, ""
}

// isPS51Shell 与三处注入点的判定口径保持一致：空 shell 与 powershell 走 PS 5.1。
func isPS51Shell(shell string) bool {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "", "powershell":
		return true
	}
	return false
}

// alternateEncodingMode 是重跑时切换到的另一种模式。
func alternateEncodingMode(mode string) string {
	if mode == encodingModeGBK {
		return encodingModeUTF8
	}
	return encodingModeGBK
}

// ---------- 输出侧：控制台字节解码 + 截断的字符边界 ----------
//
// 这一节是 2026-11-21 三方编码对比后的修复，两个由来：
//
//  1. **截断按字节切 → 整段乱码（H1）**：缓冲超限时从头部丢字节，剩下的字节
//     可能以某个 UTF-8 字符的**续字节**（0x80-0xBF）起始。utf8.Valid 因此失败，
//     整段落进 GB18030 分支——而 0x80-0xBF 恰在 GBK 第二字节的合法区间，结构
//     校验还会通过，于是**整段输出**被解成乱码。尾部按字节截断（解码前）同理。
//     解码后按字节切字符串则是另一种形态：只切断末尾一个字，但会产出非法 UTF-8。
//     → alignHeadToCharBoundary（丢头部时对齐）、trimIncompleteUTF8Tail（解码前
//     截尾时对齐）、clipUTF8Head / clipUTF8Tail（解码后切字符串时对齐）。
//
//  2. 解码侧原来只有"UTF-8 / GB 族"两档：非中文 Windows（日/韩/俄/西欧）的控制台
//     输出会静默解成错字。→ consoleCodePageDecoder 按**系统 ANSI 代码页**（GetACP，
//     只查一次并缓存）选解码器；中文环境（936）行为与以前完全一致（GBK 走
//     GB18030，超集）。另留一个显式开关 JX_OUTPUT_ENCODING 作确定性出口。
//
// 这一层**不重跑命令**：字节都在手里，候选解码只是同一份字节多解一次。

// maxRuneBytes UTF-8 单字符最长 4 字节，边界对齐最多回退/跳过 3 字节。
const maxRuneBytes = 3

// alignHeadToCharBoundary 返回"从头部丢字节之后"应当用的新起点：若 from 处落在
// UTF-8 续字节上（半字符），最多再往后丢 3 字节，直到剩余字节能作为合法 UTF-8 解出。
// 关键点：**只在"多丢几字节就能让整段成为合法 UTF-8"时才动手**——GBK 流的字节同样
// 会被 utf8.Valid 判否，这时函数原样返回，绝不破坏既有行为。
func alignHeadToCharBoundary(b []byte, from int) int {
	if from <= 0 {
		return 0
	}
	if from >= len(b) || utf8.Valid(b[from:]) {
		return from
	}
	for i := 1; i <= maxRuneBytes && from+i < len(b); i++ {
		if b[from+i-1]&0xC0 != 0x80 { // 只跳续字节
			break
		}
		if utf8.Valid(b[from+i:]) {
			return from + i
		}
	}
	return from
}

// trimIncompleteUTF8Tail 在解码前把尾部"被切断的字符"去掉（最多 3 字节）。
// 同样只在"去掉这几字节后整段成为合法 UTF-8"时才动手，否则原样返回——
// 否则会把 GBK 流尾巴削掉，反而制造新的乱码。
func trimIncompleteUTF8Tail(b []byte) []byte {
	if len(b) == 0 || utf8.Valid(b) {
		return b
	}
	for i := 1; i <= maxRuneBytes && i < len(b); i++ {
		if utf8.Valid(b[:len(b)-i]) {
			return b[:len(b)-i]
		}
	}
	return b
}

// clipUTF8Tail 取 s 的前 n 字节，且不切断最后一个字符（解码后截断用）。
func clipUTF8Tail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// clipUTF8Head 取 s 的末尾 n 字节，且不从一个字符的中间开始（解码后截断用）。
func clipUTF8Head(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// outputEncodingOverride 读显式覆盖开关 JX_OUTPUT_ENCODING（只在显式设置时生效，
// 认不出取值就当没设——绝不因为一个拼错的变量名改变默认行为）。
func outputEncodingOverride() encoding.Encoding {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("JX_OUTPUT_ENCODING"))) {
	case "utf8", "utf-8":
		return encoding.Nop
	case "gbk", "gb2312", "cp936", "936":
		return simplifiedchinese.GBK
	case "gb18030", "54936":
		return simplifiedchinese.GB18030
	case "big5", "cp950", "950":
		return traditionalchinese.Big5
	case "cp932", "shiftjis", "shift-jis", "sjis", "932":
		return japanese.ShiftJIS
	case "cp949", "euckr", "949":
		return korean.EUCKR
	case "cp866", "866":
		return charmap.CodePage866
	case "cp1252", "windows1252", "1252":
		return charmap.Windows1252
	case "cp1251", "1251":
		return charmap.Windows1251
	case "cp437", "437":
		return charmap.CodePage437
	}
	return nil
}

// consoleCodePageDecoder 把 Windows 代码页映射到解码器；未收录返回 nil。
// 只收录"控制台输出真会碰上"的几档，其余走 GB18030 兜底（与历史行为一致）。
func consoleCodePageDecoder(cp uint32) encoding.Encoding {
	switch cp {
	case 936, 54936:
		// 中文代码页直接用 GB18030：它是 GBK 的超集，与修复前的中文环境行为**逐字节一致**
		// （GBK 解不出的四字节序列它也能解），所以中文机器上的默认路径没有变化。
		return simplifiedchinese.GB18030
	case 950:
		return traditionalchinese.Big5
	case 932:
		return japanese.ShiftJIS
	case 949:
		return korean.EUCKR
	case 866:
		return charmap.CodePage866
	case 874:
		return charmap.Windows874
	case 1250:
		return charmap.Windows1250
	case 1251:
		return charmap.Windows1251
	case 1252:
		return charmap.Windows1252
	case 1253:
		return charmap.Windows1253
	case 1254:
		return charmap.Windows1254
	case 1255:
		return charmap.Windows1255
	case 1256:
		return charmap.Windows1256
	case 1257:
		return charmap.Windows1257
	case 1258:
		return charmap.Windows1258
	case 437:
		return charmap.CodePage437
	case 850:
		return charmap.CodePage850
	}
	return nil
}

// decodeConsoleBytes 是控制台输出字节 → 合法 UTF-8 文本的唯一入口。
//
// 顺序（每一步都不改变"本来就是 UTF-8"与"中文环境"的既有行为）：
//  1. 严格 UTF-8 合法 → 直接返回（绝大多数情况，零成本）；
//  2. 显式覆盖 JX_OUTPUT_ENCODING → 用它解（**非中文代码页的确定性出口**，见下）；
//  3. GB18030（中文环境最可能的来源，与修复前逐字节一致）；
//  4. 仅当 3 的结果**确有损**（含替换符，说明 GB18030 根本表示不了这些字节）、
//     且本机 ANSI 代码页能解出无损结果时，才改用本机代码页的解码器。
//
// 为什么不无条件按本机代码页解（2026-11-21 实测后的刻意保守）：代码页之间**无法
// 互判**——GB18030 解 Shift_JIS 日文会得到"擔杮岅偙傫偵偪偼"这种看着正常的中文串
// （既不报错也没有替换符），反过来 cp932 解 GBK 中文会得到半角片假名+替换符。
// 也就是说"自动换表"必然在某一侧静默解错：在中文环境以外，它会把本来能解对的中文
// 输出解成当地文字的乱码。我们的用户绝大多数是中文 Windows（ACP=936，直接走 3），
// 因此这里只保留"能证明主路径有损才换表"这一条；要强制按某个代码页解，用 2 的开关：
//   JX_OUTPUT_ENCODING = gbk | gb18030 | utf8 | big5 | cp932 | cp949 | cp866 | cp1252 | cp437 ...
func decodeConsoleBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	if utf8.Valid(b) {
		return b
	}
	if enc := outputEncodingOverride(); enc != nil {
		if out, err := encNewDecoderBytes(enc, b); err == nil {
			return out
		}
		// 覆盖值解不了就继续往下走常规路径，不给用户一个更差的默认行为
	}
	primary := decodeGB18030Lossy(b)
	if !strings.ContainsRune(string(primary), utf8.RuneError) {
		return primary
	}
	if cp := systemANSICodePage(); cp != 0 && cp != 936 && cp != 54936 {
		if dec := consoleCodePageDecoder(cp); dec != nil {
			if alt, err := encNewDecoderBytes(dec, b); err == nil && !strings.ContainsRune(string(alt), utf8.RuneError) {
				return alt
			}
		}
	}
	return primary
}

// encNewDecoderBytes 是"用某编码解一份字节"的统一封装。
func encNewDecoderBytes(enc encoding.Encoding, b []byte) ([]byte, error) {
	if enc == nil || enc == encoding.Nop {
		return []byte(strings.ToValidUTF8(string(b), "\uFFFD")), nil
	}
	return enc.NewDecoder().Bytes(b)
}

// decodeGB18030Lossy 是历史兜底：GB18030 → 替换符。
func decodeGB18030Lossy(b []byte) []byte {
	if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
		return s
	}
	return []byte(strings.ToValidUTF8(string(b), "\uFFFD"))
}
