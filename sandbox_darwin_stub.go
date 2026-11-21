//go:build !darwin

package main

// 非 macOS 平台的编译占位：运行时不会走到（spawnConfinedLocal 按
// isDarwinRuntime 分派），但 Windows/Linux/安卓构建仍需要符号存在。

import (
	"context"
	"fmt"
	"io"
)

func spawnConfinedDarwin(ctx context.Context, r sandboxRun) (string, error) {
	return "", fmt.Errorf("sandbox unavailable: macOS 沙箱后端不可用")
}

func startConfinedDarwinAsync(ctx context.Context, r sandboxRun, w io.Writer) (func() bool, error) {
	return nil, fmt.Errorf("sandbox unavailable: macOS 沙箱后端不可用")
}

// darwinDenialSigs 占位（非 darwin 平台不参与拒绝方言选择）。
var darwinDenialSigs []string
