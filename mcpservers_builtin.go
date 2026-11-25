package main

// mcpservers_builtin.go —— 内置 MCP 连接器。
//
// 与「内置技能」同一套心智：随包分发、开箱即用，用户不必手填启动命令；
// 删掉也不会被重新塞回来（一次性迁移释放 + 账本记账，见 migrations.go）。
//
// 二者都受「操作电脑」能力门控，软硬双保险：
//   - 软性：能力没开时 PromptBlock 不把连接器列进环境快照，模型压根不知道有它；
//   - 硬性：mcp_read_connector / mcp_call_tool 走 requireCapability 拦截并弹审批卡。
// 两道都要——软性省掉模型白撞墙的轮次，硬性保证撞了也进不去。

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// builtinMCPDef 一条内置连接器定义。
type builtinMCPDef struct {
	ID          string // 稳定 id：落进 source=builtin:<id>，也是分块更新的白名单键
	Name        string // 注册表里的服务器名（模型只在连接器清单里看到它）
	Description string // 进环境快照的一句话描述
	RequiresCap string // 需要的会话能力（空 = 不门控）
	// resolve 在运行时解析启动参数：安装目录各机不同，不能写死。
	// ok=false 表示本机没有这份内置组件——跳过不注册，不造"点了就报错"的空壳。
	resolve func(a *App) (command string, args []string, env map[string]string, ok bool)
}

// builtinMCPOrder 内置连接器清单（顺序即设置页展示顺序）。
// 新增内置连接器时三处同步：这里、打包脚本（内置组件目录）、分块更新的 mcp 块白名单。
var builtinMCPOrder = []string{"cua", "windows-mcp"}

func builtinMCPDefs() []builtinMCPDef {
	return []builtinMCPDef{
		{
			ID:   "cua",
			Name: "cua",
			Description: "Cua 桌面驱动（跨平台）：截屏、点击、输入、滚动、窗口与无障碍树。动作用的是窗口内坐标或" +
				"元素 token，动作前先读一次窗口快照；读取超过 1 秒就放弃语义定位、走坐标兜底（慢机器只是慢，不会卡死）",
			RequiresCap: capComputer,
			resolve:     resolveCuaDriver,
		},
		{
			ID:   "windows-mcp",
			Name: "windows-mcp",
			Description: "Windows 桌面自动化（UIAutomation，工具面最全）：点击（左/右/中、悬停、双击）、输入、快捷键、" +
				"滚动（含水平）、拖拽、等待条件、启动应用、剪贴板、文件与进程",
			RequiresCap: capComputer,
			resolve:     resolveWindowsMCP,
		},
	}
}

// builtinComponentsDir 内置组件目录（Cua 二进制、Windows-MCP 的 Python 依赖都放这）。
// 解析顺序与 SkillsDir 一致：可执行文件旁 > 当前工作目录（开发态）。
//
// 刻意不塞进 技能/_runtime：那个目录会被分块更新（runtime 块）整目录替换，
// 外部二进制和第三方依赖会被一起冲掉。
// builtinComponentsPath 返回「内置组件」目录的绝对路径（不检查是否存在）：
// 更新块要靠它铺新版本——首次安装时这个目录可能还不存在。
// 优先 exe 同级（安装位置），其次当前工作目录（开发态直接 go run 时）。
func builtinComponentsPath() string {
	if exe, err := os.Executable(); err == nil {
		c := filepath.Join(filepath.Dir(exe), "内置组件")
		if dirExists(c) {
			return c
		}
		if wd, err2 := os.Getwd(); err2 == nil {
			if c2 := filepath.Join(wd, "内置组件"); dirExists(c2) {
				return c2
			}
		}
		base := strings.ToLower(filepath.Dir(exe))
		if strings.Contains(base, "go-build") || strings.Contains(base, "\\temp") {
			return "" // go test / 临时产物目录不算安装位置
		}
		return c
	}
	return ""
}

func builtinComponentsDir() string {
	c := builtinComponentsPath()
	if c == "" || !dirExists(c) {
		return ""
	}
	return c
}

// resolveCuaDriver 解析 Cua 驱动的 stdio MCP 入口。
//
// 注意不传 timeout_ms：驱动默认 1000ms 读窗口，读不完就降级到坐标——这是
// 2026-11-03 与用户确认过的取舍（快机器上语义定位有用，慢机器上多等 1 秒也无妨）。
// 后人不许为了"读全一点"把这个值调大。
func resolveCuaDriver(a *App) (string, []string, map[string]string, bool) {
	dir := builtinComponentsDir()
	if dir == "" {
		return "", nil, nil, false
	}
	// 可执行文件名按平台取：Windows 是 cua-driver.exe，macOS/Linux 是 cua-driver
	// （同一份 Rust 工程的多平台构建，见 crates/platform-macos；mac 包由
	// 打包/打包Mac版.py 从上游 release 取 darwin-universal 二进制放进来）。
	exe := filepath.Join(dir, "cua-driver", cuaDriverBinName())
	if !fileExists(exe) {
		return "", nil, nil, false
	}
	// macOS 要「辅助功能 + 屏幕录制」两项系统授权（首次点击/截屏时由系统弹窗
	// 申请，用户拒绝则驱动只能读到窗口列表、动不了）。这里不代劳，交给系统弹窗。
	return exe, []string{"mcp"}, nil, true
}

// resolveWindowsMCP 解析 Windows-MCP 的 stdio MCP 入口。
//
// 依赖装在 内置组件/wmcppkg（PYTHONPATH 指过去），解释器复用技能区的便携运行时——
// numpy/PIL/cryptography/pywin32 这些大件运行时里已有，不重复占体积。
// 遥测默认关闭：这是面向企业内网的产品，默认外发数据不能接受。
func resolveWindowsMCP(a *App) (string, []string, map[string]string, bool) {
	dir := builtinComponentsDir()
	if dir == "" {
		return "", nil, nil, false
	}
	pkg := filepath.Join(dir, "wmcppkg")
	if !dirExists(pkg) {
		return "", nil, nil, false
	}
	boot := filepath.Join(dir, "windows-mcp-boot.py")
	if !fileExists(boot) {
		return "", nil, nil, false
	}
	skills, err := a.SkillsDir()
	if err != nil {
		return "", nil, nil, false
	}
	py := filepath.Join(skills, "_runtime", "python", "python.exe")
	if !fileExists(py) {
		return "", nil, nil, false
	}
	env := map[string]string{
		"PYTHONUTF8":           "1",
		"ANONYMIZED_TELEMETRY": "false",
		// UIA 连接/事务超时：单个卡死的应用只失败它自己，别拖垮整轮调用
		"WINDOWS_MCP_UIA_CONNECTION_TIMEOUT_MS":  "2000",
		"WINDOWS_MCP_UIA_TRANSACTION_TIMEOUT_MS": "5000",
		// 已知会让无障碍树遍历卡死的编辑器：直接不进它们的树
		"WINDOWS_MCP_EXCLUDE_PROCESSES": "Code.exe,Cursor.exe",
	}
	// 走垫片而不是 -m windows_mcp：便携 Python 的 ._pth 让解释器 isolated，
	// PYTHONPATH 不生效，必须由垫片把 wmcppkg 插进 sys.path。
	return py, []string{boot, "serve"}, env, true
}

// materializeBuiltinMCP 释放内置连接器（由一次性迁移调用，跑过一次就记账）。
//
// 只落盘不建连接：启动期零 MCP 进程是既有约定（见 InitMCPServers 注释），
// 连接按需在首次调用时懒启动。
func (a *App) materializeBuiltinMCP() error {
	if a.MCPServers == nil {
		return nil
	}
	for _, d := range builtinMCPDefs() {
		if a.MCPServers.HasName(d.Name) {
			continue // 已存在（含用户改过/关过）：不覆盖用户的决定
		}
		cmd, args, env, ok := d.resolve(a)
		if !ok {
			log.Printf("[MCP] 内置连接器 %s 未随包安装，跳过注册", d.ID)
			continue
		}
		e := mcpServerEntry{
			Name:        d.Name,
			Description: d.Description,
			Enabled:     true,
			Source:      "builtin:" + d.ID,
			Command:     cmd,
			Args:        args,
			Env:         env,
			RequiresCap: d.RequiresCap,
		}
		if err := a.MCPServers.addQuiet(e); err != nil {
			log.Printf("[MCP] 内置连接器 %s 注册失败: %v", d.ID, err)
			continue
		}
		log.Printf("[MCP] 已释放内置连接器 %s（%s）", d.Name, d.ID)
	}
	return nil
}

// fileExists 小工具：内置组件解析用（目录工具的 dirExists 在 skills.go）。
func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// cuaDriverBinName Cua 驱动可执行文件名：Windows 带 .exe，macOS/Linux 不带。
func cuaDriverBinName() string {
	if runtime.GOOS == "windows" {
		return "cua-driver.exe"
	}
	return "cua-driver"
}
