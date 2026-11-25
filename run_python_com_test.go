package main

// run_python 隐藏参数 com（Office/WPS COM 自动化宿主直跑通道）的针对性测试：
//   ① schema 隐身守护——工具定义序列化后不得出现 "com"（该参数刻意不进
//      schema，唯一告知点是 office 技能 com 分册，见 run_python.go 通道注释）；
//   ② 只读档拒绝——越权在装配运行时之前先拒，不依赖联网装配；
//   ③ 真机通道验证——workspace-write 会话带 com=true：宿主直跑下 pywin32
//      Dispatch 系统自带 COM（WScript.Shell）成功，且脚本能写工作区外
//      探针文件（沙箱内必被拒，参照 TestRunPythonSandboxBlocksOutsideWrite）
//      ——两证齐全才证明走的确实是 full 策略宿主路径，而非沙箱。
//   ④ flexBool 解析矩阵——布尔与字符串 "true" 双形态等价（XML 参数形态的
//      宿主字符串化）、null/空视为未带、不识别值报错；
//   ⑤ 字符串形态链路验证——com="true" 与布尔同效触发 com 分支（借只读
//      档拒绝做探针，秒回不依赖联网）。
// ③ 只在宿主令牌环境跑：go test 进程若本身已在命令沙箱（受限令牌）里，
// com=true 子进程继承的还是受限令牌，「宿主直跑」无从验证——探针法检测
// 后跳过并说明出路。运行时/pywin32 未装配的环境同样自动跳过。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPythonComHiddenFromSchema(t *testing.T) { // ①
	raw, err := json.Marshal(runPythonTool().Spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"com"`) {
		t.Fatalf("com 是隐藏参数，不得出现在工具 schema 里（唯一告知点是 office 技能 com 分册）:\n%.400s", raw)
	}
}

func TestRunPythonComReadOnlyRejected(t *testing.T) { // ②
	a := newRunPyTestApp(t)
	sess := &SessionMeta{ID: "testpycom01abcdef", Workspace: a.Cfg.Workspace, Permission: permReadOnly}
	_, err := runPyCall(t, a, sess, map[string]any{"code": "print(1)", "com": true})
	if err == nil || !strings.Contains(err.Error(), "全电脑只读") {
		t.Fatalf("只读档下 com 通道应被拒绝, got: %v", err)
	}
}

// skipIfRestrictedToken 当前 go test 进程是否已在受限令牌（命令沙箱）下：
// 向用户目录写一个探针，被拒即受限。com=true 的「宿主直跑」只在宿主令牌
// 环境下可验证（真实产品里助手进程是宿主令牌，只有子命令被套沙箱）。
func skipIfRestrictedToken(t *testing.T) {
	t.Helper()
	probe := filepath.Join(os.Getenv("USERPROFILE"), "__jx_com_env_probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Skipf("go test 进程在受限令牌下（%v），com 宿主直跑验证跳过——请在沙箱外环境重跑本用例", err)
	}
	os.Remove(probe)
}

func TestRunPythonComHostRunDispatch(t *testing.T) { // ③
	if !isWindowsRuntime {
		t.Skip("COM 宿主直跑通道仅 Windows 有意义")
	}
	skipIfRestrictedToken(t)
	a := newRunPyTestApp(t)
	skipIfNoPyRuntime(t, a)
	sess := &SessionMeta{ID: "testpycom02abcdef", Workspace: a.Cfg.Workspace} // 缺省 = workspace-write

	// 幂等装配 pywin32（office com 分册依赖）；装配失败（无网）时返回值带
	// 失败文案，按无环境跳过
	guide := officeGuide(t, a, sess, "com")
	if strings.Contains(guide, "装配失败") {
		t.Skip("pywin32 装配需联网，本环境不可用，跳过真机用例")
	}
	if !strings.Contains(guide, "com 分册要点") {
		t.Fatalf("com 分册指南未生效:\n%.300s", guide)
	}

	// workspace-write 会话 + com=true：Dispatch 系统自带 COM + 写工作区外探针
	code := strings.Join([]string{
		"import os, win32com.client",
		"sh = win32com.client.Dispatch('WScript.Shell')",
		"probe = os.path.join(os.path.expanduser('~'), '__jx_com_probe.txt')",
		"open(probe, 'w').write('x')",
		"os.remove(probe)",
		"print('com-ok:' + sh.ExpandEnvironmentStrings('%USERNAME%'))",
	}, "\n")
	out, err := runPyCall(t, a, sess, map[string]any{"code": code, "name": "com_probe", "com": true, "timeout_sec": 120})
	if err != nil || !strings.Contains(out, "com-ok:") {
		t.Fatalf("com=true 宿主直跑 COM 失败: %v\n%s", err, out)
	}
}

// ④ flexBool 解析矩阵：布尔与字符串双形态等价、null/空串视为未带、
// 不识别的值报错（不静默当 false）。XML 参数形态（Claude 协议）的工具
// 调用里 com 到引擎已是 "true" 字符串——宽容解析是 com 通道在该形态下
// 可用的前提（2026-11-18 排查：直解 bool 报 cannot unmarshal string）。
func TestRunPythonComFlexBoolParse(t *testing.T) {
	cases := []struct {
		in   string
		want bool
		ok   bool
	}{
		{`true`, true, true},
		{`"true"`, true, true},
		{`"TRUE"`, true, true},
		{`1`, true, true},
		{`"1"`, true, true},
		{`false`, false, true},
		{`"false"`, false, true},
		{`""`, false, true},
		{`null`, false, true},
		{`"yes"`, false, false},
	}
	for _, c := range cases {
		var b flexBool
		err := json.Unmarshal([]byte(c.in), &b)
		if !c.ok {
			if err == nil {
				t.Errorf("flexBool(%s) 应报错却解析成功", c.in)
			}
			continue
		}
		if err != nil || bool(b) != c.want {
			t.Errorf("flexBool(%s) = %v (err=%v)，期望 %v", c.in, b, err, c.want)
		}
	}
}

// ⑤ 字符串形态的链路验证：com="true"（字符串）应与布尔 true 同效走进
// com 分支——只读档拒绝发生在装配运行时之前，秒回且不依赖联网，正好
// 当「解析结果 = true」的链路级探针。
func TestRunPythonComStringTrueChannel(t *testing.T) {
	a := newRunPyTestApp(t)
	sess := &SessionMeta{ID: "testpycom03abcdef", Workspace: a.Cfg.Workspace, Permission: permReadOnly}
	_, err := runPyCall(t, a, sess, map[string]any{"code": "print(1)", "com": "true"})
	if err == nil || !strings.Contains(err.Error(), "全电脑只读") {
		t.Fatalf(`com="true" 字符串形态应与布尔同效触发只读档拒绝, got: %v`, err)
	}
}
