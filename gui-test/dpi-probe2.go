//go:build windows

package main

// 子树 dump + 面板尺寸时序探针（v3）：
//   dump 模式：go run gui-test/dpi-probe2.go dump
//   监控模式：go run gui-test/dpi-probe2.go watch <hwnd> <秒>
// watch 模式 1ms 轮询目标 HWND 物理矩形并打印每次变化时刻。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	u32           = windows.NewLazySystemDLL("user32.dll")
	pEnumWindows  = u32.NewProc("EnumWindows")
	pEnumChild    = u32.NewProc("EnumChildWindows")
	pGetWndRect   = u32.NewProc("GetWindowRect")
	pIsVisible    = u32.NewProc("IsWindowVisible")
	pGetClassName = u32.NewProc("GetClassNameW")
	pGetWndText   = u32.NewProc("GetWindowTextW")
	pGetParent    = u32.NewProc("GetParent")
)

type rect struct{ L, T, R, B int32 }

func cls(h uintptr) string {
	b := make([]uint16, 64)
	pGetClassName.Call(h, uintptr(unsafe.Pointer(&b[0])), 64)
	return windows.UTF16ToString(b)
}

func txt(h uintptr) string {
	b := make([]uint16, 128)
	pGetWndText.Call(h, uintptr(unsafe.Pointer(&b[0])), 128)
	return windows.UTF16ToString(b)
}

func rectOf(h uintptr) rect {
	var r rect
	pGetWndRect.Call(h, uintptr(unsafe.Pointer(&r)))
	return r
}

func main() {
	shcore := windows.NewLazySystemDLL("shcore.dll")
	if p := shcore.NewProc("SetProcessDpiAwareness"); p.Find() == nil {
		_, _, _ = p.Call(2) // per-monitor aware
	} else {
		p2 := u32.NewProc("SetProcessDPIAware")
		_, _, _ = p2.Call()
	}

	if len(os.Args) >= 2 && os.Args[1] == "dump" {
		dump()
		return
	}
	if len(os.Args) >= 4 && os.Args[1] == "watch" {
		h, _ := strconv.ParseUint(os.Args[2], 10, 64)
		secs, _ := strconv.ParseFloat(os.Args[3], 64)
		watch(uintptr(h), secs)
		return
	}
	fmt.Println("用法: dump | watch <hwnd> <秒>")
}

func findMain() uintptr {
	title := "AI数字助理"
	var mainWnd uintptr
	cb := syscall.NewCallback(func(h, l uintptr) uintptr {
		if vis, _, _ := pIsVisible.Call(h); vis == 0 {
			return 1
		}
		if strings.Contains(txt(h), title) && mainWnd == 0 {
			mainWnd = h
			return 0
		}
		return 1
	})
	_, _, _ = pEnumWindows.Call(cb, 0)
	return mainWnd
}

func dump() {
	mainWnd := findMain()
	fmt.Printf("main=%d %q\n", mainWnd, txt(mainWnd))
	var walk func(h uintptr, depth int)
	walk = func(h uintptr, depth int) {
		ccb := syscall.NewCallback(func(ch, l uintptr) uintptr {
			r := rectOf(ch)
			vis, _, _ := pIsVisible.Call(ch)
			fmt.Printf("%*s|- hwnd=%d cls=%q title=%q rect=(%d,%d)-(%d,%d) %dx%d vis=%d\n",
				depth*2, "", ch, cls(ch), txt(ch), r.L, r.T, r.R, r.B, r.R-r.L, r.B-r.T, vis)
			walk(ch, depth+1)
			return 1
		})
		_, _, _ = pEnumChild.Call(h, ccb, 0)
	}
	walk(mainWnd, 0)
}

func watch(h uintptr, secs float64) {
	fmt.Printf("watching %d for %.1fs...\n", h, secs)
	last := ""
	stop := time.Now().Add(time.Duration(secs * float64(time.Second)))
	for time.Now().Before(stop) {
		r := rectOf(h)
		s := fmt.Sprintf("%dx%d@(%d,%d)", r.R-r.L, r.B-r.T, r.L, r.T)
		if s != last {
			last = s
			fmt.Println(time.Now().Format("15:04:05.000") + "  " + s)
		}
		time.Sleep(time.Millisecond)
	}
}
