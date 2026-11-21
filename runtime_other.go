//go:build !windows && !linux && !android && !darwin

package main

const (
	isWindowsRuntime = false
	isLinuxRuntime   = false
	isAndroidRuntime = false
	isDarwinRuntime  = false
)
