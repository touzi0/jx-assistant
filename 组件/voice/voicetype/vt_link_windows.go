//go:build windows

package main

// 与主程序（坚祥数字助理）联动：从主程序那边取**听写快捷键**与**主题色**。
//
// 为什么走文件而不是别的：
//   · 主程序的听写快捷键存在前端 localStorage（组件 voice 的 cx:voice:*），独立
//     进程读不到；主程序侧的 /api/config 只存 accentColor，没有快捷键。
//   · 所以约定一个**专用小文件** voicetype.json（只含热键/颜色，绝无密钥），
//     主程序在快捷键或点缀色变化时写它，输入法每秒看 mtime 热更新。
//   · 组件模式下主程序可以用环境变量 VT_VOICETYPE_JSON 指明它的绝对路径；
//     独立运行则按下面的候选路径探测。
//
// 字段（主程序写）：
//
//	{
//	  "hotkeys": {"main": ["alt"], "sub": ["mouse-side"]},
//	  "holdMs": 300,
//	  "accentColor": "#4d6bfe",   // 主程序「点缀色」
//	  "textColor": "#1f2328",     // 主程序主题文字色（浅色主题 #1f2328 / 深色 #e6e8eb）
//	  "updatedAt": "2026-11-03T21:30:00+08:00"
//	}
//
// 环境变量优先级最高（产品拉起 worker 时直接注入，最可靠）：
//
//	VT_VOICETYPE_JSON  联动文件绝对路径
//	VT_HOTKEY          形如 "alt" / "ctrl+f2" / "mouse-side"
//	VT_SUB_HOTKEY      副热键（同上格式，可空）
//	VT_ACCENT          "#4d6bfe"
//	VT_TEXT_COLOR      "#1f2328"
//	VT_HOLD_MS         长按阈值毫秒

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// vtLinkFile 联动文件名（放在主程序目录，与 config.json 同级）。
const vtLinkFile = "voicetype.json"

// vtLinkCfg 联动配置（只解这几个字段，别的字段一概不碰）。
type vtLinkCfg struct {
	Hotkeys struct {
		Main []string `json:"main"`
		Sub  []string `json:"sub"`
		// Mode 触发方式（tap=点按切换 / hold=按住说话），由主程序的「设置·语音」决定；
		// 空=沿用本目录 hotkeys.json。两侧各有一套会很别扭，所以跟主程序走。
		Mode string `json:"mode"`
	} `json:"hotkeys"`
	HoldMs      int    `json:"holdMs"`
	TapMs       int    `json:"tapMs"` // tap 模式的点按上限（主程序可选下发，缺省 1000）
	AccentColor string `json:"accentColor"`
	TextColor   string `json:"textColor"`
	UpdatedAt   string `json:"updatedAt"`
}

// vtLinkState 当前生效的联动配置 + 来源（日志用）。
type vtLinkState struct {
	Path   string
	Cfg    vtLinkCfg
	Loaded bool
}

var vtLinkCur vtLinkState

// vtLinkCandidatePaths 联动文件的探测顺序。
//
// 1) 环境变量指定（组件模式：主程序自己知道文件在哪）
// 2) 输入法自己目录（手写/调试最方便）
// 3) 开发工作区根（voicetype → voice → 组件 → 工作区根，三级向上；
//    没有这条，双击启动的输入法读不到主程序设置——叉掉的热键会在重启后复活。
//    目录改名前的老布局 ../坚祥数字助理GO 也保留一条）
// 4) 开发工作区里的老主程序目录（../坚祥数字助理GO，改名前的工作区布局）
// 5) 安装版主程序目录（与 config.json 同级）
func vtLinkCandidatePaths() []string {
	out := []string{}
	if p := strings.TrimSpace(os.Getenv("VT_VOICETYPE_JSON")); p != "" {
		out = append(out, p)
	}
	out = append(out, filepath.Join(vtDir, vtLinkFile))
	out = append(out, filepath.Join(vtDir, "..", "..", "..", vtLinkFile)) // 工作区根：voicetype→voice→组件→软著版数字助理
	out = append(out, filepath.Join(vtDir, "..", "坚祥数字助理GO", vtLinkFile))
	out = append(out, `D:\123\AI数字助理\`+vtLinkFile)   // 安装版（2026-11-19 起新装机目录名跟发行名）
	out = append(out, `D:\123\坚祥数字助理GO\`+vtLinkFile) // 老装机（AppId 不变沿用旧目录，改名前装的）
	return out
}

// vtLoadLink 读联动配置（取第一个存在的候选文件）。
func vtLoadLink() vtLinkState {
	st := vtLinkState{}
	for _, p := range vtLinkCandidatePaths() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg vtLinkCfg
		if err := json.Unmarshal(raw, &cfg); err != nil {
			vtLog("联动文件解析失败（忽略）：%s %v", p, err)
			continue
		}
		st = vtLinkState{Path: p, Cfg: cfg, Loaded: true}
		break
	}
	// 环境变量覆盖（逐字段）
	if v := strings.TrimSpace(os.Getenv("VT_HOTKEY")); v != "" {
		st.Cfg.Hotkeys.Main = vtSplitHotkey(v)
		st.Loaded = true
		if st.Path == "" {
			st.Path = "环境变量 VT_HOTKEY"
		}
	}
	if v := strings.TrimSpace(os.Getenv("VT_SUB_HOTKEY")); v != "" {
		st.Cfg.Hotkeys.Sub = vtSplitHotkey(v)
	}
	if v := strings.TrimSpace(os.Getenv("VT_ACCENT")); v != "" {
		st.Cfg.AccentColor = v
	}
	if v := strings.TrimSpace(os.Getenv("VT_TEXT_COLOR")); v != "" {
		st.Cfg.TextColor = v
	}
	if v := strings.TrimSpace(os.Getenv("VT_HOLD_MS")); v != "" {
		var ms int
		if _, err := fmt.Sscanf(v, "%d", &ms); err == nil && ms > 0 {
			st.Cfg.HoldMs = ms
		}
	}
	vtLinkCur = st
	return st
}

// vtSplitHotkey "ctrl+f2" → ["ctrl","f2"]；"alt" → ["alt"]。
func vtSplitHotkey(s string) []string {
	out := []string{}
	for _, part := range strings.Split(s, "+") {
		if p := strings.TrimSpace(strings.ToLower(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 主题色 ----------

var (
	vtThemeAccent string
	vtThemeText   string
)

// vtEffectiveTheme 生效的主题：(点缀色, 文字色)。
//
// 点缀色优先用主程序给的（accentColor），文字色优先用主程序的主题文字色；
// 缺省时按点缀色的亮度自动挑一个对比度够的（深底白字、浅底深字），
// 保证任何点缀色下胶囊上的字都看得清。
func vtEffectiveTheme() (accent, text string) {
	cfg := vtLinkCur.Cfg
	accent = strings.TrimSpace(cfg.AccentColor)
	if !vtIsHexColor(accent) {
		accent = "#4d6bfe" // 主程序默认点缀色
	}
	text = strings.TrimSpace(cfg.TextColor)
	if !vtIsHexColor(text) {
		text = vtContrastText(accent)
	}
	return strings.ToLower(accent), strings.ToLower(text)
}

func vtIsHexColor(s string) bool {
	if len(s) != 7 && len(s) != 4 {
		return false
	}
	if s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// vtContrastText 深底给白字、浅底给近黑字（WCAG 相对亮度的粗分档）。
func vtContrastText(hex string) string {
	r, g, b := vtHexRGB(hex)
	lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b) // 0..255
	if lum >= 168 {
		return "#1f2328" // 主程序浅色主题的文字色
	}
	return "#ffffff"
}

func vtHexRGB(hex string) (int, int, int) {
	s := strings.TrimPrefix(hex, "#")
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	var r, g, b int
	fmt.Sscanf(s, "%02x%02x%02x", &r, &g, &b)
	return r, g, b
}

// vtApplyTheme 把主题色推给悬浮窗页面（页面没就绪时静默跳过，就绪后会再推一次）。
func vtApplyTheme(force bool) {
	accent, text := vtEffectiveTheme()
	if !force && accent == vtThemeAccent && text == vtThemeText {
		return
	}
	vtThemeAccent, vtThemeText = accent, text
	vtEval(fmt.Sprintf(`vt.theme(%q, %q)`, accent, text))
}

// vtThemeDesc 日志用的一句话。
func vtThemeDesc() string {
	accent, text := vtEffectiveTheme()
	return fmt.Sprintf("点缀色=%s 文字色=%s（来源：%s）", accent, text, vtSourceDesc())
}

func vtSourceDesc() string {
	if !vtLinkCur.Loaded {
		return "内置默认"
	}
	p := vtLinkCur.Path
	if p == "" {
		p = "联动配置"
	}
	return p
}
