//go:build windows

package main

// 【临时缓解——排查「静默闪退」用，Go 官方修复后整文件删除】
//
// 现象（2026-10-16 定位，安装机 data\desktop.log 累计 22 次同签名闪退）：
// 进程毫无征兆消失（托盘图标一并消失），Windows 事件日志 / WER 无任何记录
// （Go runtime.throw 走 exit(2) 干净退出，不产生崩溃报告）；日志里只有
// copystack 崩溃堆栈，throw 点在 runtime.adjustpointers（栈拷贝时的指针
// 调整），触发路径是 UI goroutine 上的 Windows 消息回调链：
//   mainWndSubclassProc（desktop_windows.go）
//     → CallWindowProcW → go-webview2 wndproc → WebView2 COM 回调
// 回调链活动期间发生栈扩容拷贝（morestack→copystack）即中招。这与
// 「跑长任务不死、闲着/轻负载反而死」「开机刚启动、别的软件陆续打开时死」
// 完全吻合：主 goroutine 栈的历次增长集中在启动早期的消息风暴期，长过了
// 就安全；崩溃与否与工具调用无关。
//
// 已知 Go 运行时缺陷（1.25 / 1.26 / master 均未修复，非本项目代码错误）：
//   https://gist.github.com/workturnedplay/9f0420d5230553fd422f446a60f2a6e4
//   https://github.com/golang/go/issues/69926
// （装机的坚祥数字助理GO.exe 为 go1.26.5 构建，仓库当前 go1.27.1，均受影响。）
//
// 缓解思路：让承载消息循环的 goroutine 整体跑在「哨兵递归帧」里——启动时
// 先递归 ~2MB 深，再把循环体放在栈底运行。效果：
//   1) 该 goroutine 栈占用恒高 → GC 的 shrinkstack 永不收缩；
//   2) 回调链实际深度（几十 KB 量级）远小于预增长余量 → morestack 永不触发。
// 该 goroutine 上从此不再发生 copystack，上述 bug 无从引爆。
// 代价：这条 goroutine 常驻 ~2MB 栈、GC 栈扫描略增，可忽略。
// 备注：GODEBUG=gcshrinkstackoff=1 也能防收缩，但 //go:debug 白名单不收、
// 环境变量注入需要改启动拓扑（自中转进程），哨兵帧纯代码、双击首启即生效。
//
// 已知未覆盖（残留风险，历史上无崩溃样本）：tool_computer_windows.go 的
// EnumWindows 回调跑在工具工作 goroutine 上（浅层、不嵌套），如日志中再现
// 同签名堆栈且栈顶在 enumWndCb，再为工具执行路径加同款护栏。
//
// 删除条件：Go 官方修复上述回调栈拷贝缺陷（跟进 golang/go issue），或桌面
// 壳换掉 WebView2 之后本护栏无回调可保。

import "log"

// guardedStackDepth 递归帧数。实测（go1.27.1，64 位）：编译器只给每帧保留
// ~64B（未用到的数组元素不占帧），16384 层 ≈ 1MB 固定栈、一次性递归耗时
// ~2ms。回调链实际深度为几十 KB 量级，余量数十倍；内存代价 1MB/进程。
const guardedStackDepth = 16384

// guardedStackSink 让递归帧里的数组被真实引用，防止编译器裁帧。
var guardedStackSink uintptr

// withGuardedStack 在预增长的深栈内同步运行 fn。fn 返回前全部递归帧保持
// 存活（栈占用恒高），fn 内启动的一切（含 Windows 消息回调）都在护栏内。
func withGuardedStack(fn func()) {
	guardedStackDescend(guardedStackDepth, fn)
}

// withGuardedStackResult 带返回值版本（runDesktop 需要回传 error）。
func withGuardedStackResult[T any](fn func() T) T {
	var result T
	guardedStackDescend(guardedStackDepth, func() { result = fn() })
	return result
}

//go:noinline
func guardedStackDescend(depth int, fn func()) {
	if depth <= 0 {
		log.Printf("[栈护栏] 就位：消息循环及全部窗口回调改跑在预增长深栈上（临时缓解 Go 回调栈缺陷，详见 stackguard_windows.go 注释）")
		fn()
		return
	}
	var pad [64]uintptr
	pad[0] = uintptr(depth)
	guardedStackSink ^= pad[0]
	guardedStackDescend(depth-1, fn)
}
