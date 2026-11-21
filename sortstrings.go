package main

import "strings"

// sortStrings 原定义在 sandbox_windows.go，但 update_runtime.go（跨平台）也在用，
// 挪到平台无关文件：不区分大小写的插入排序（Windows 环境块、技能目录名共用的稳定排序）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.ToLower(s[j]) < strings.ToLower(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
