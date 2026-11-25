package main

// update_builtin.go —— 「内置组件」分块静默更新（Cua 驱动 / Windows-MCP 依赖）。
//
// 与 runtime / skills 两块同一纪律：下载校验 → 解压到临时 → 结构校验 →
// 目录级替换（先改名再铺新，失败回滚）。
//
// 有一处特殊：cua-driver 可能是活进程（MCP 连接器按需拉起后会常驻），
// 目录被占用时 rename 会失败。这时直接让路、下一轮更新再来——不杀用户
// 正在用的进程，也不因为一次更新把操作电脑弄瘫。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// builtinComponentsMarker 是内置组件目录根下的版本标记文件名。
const builtinComponentsMarker = ".version"

// applyBuiltinBlock 应用「内置组件」块。
func (a *App) applyBuiltinBlock(b *updateBlock, rel releaseInfo, sourceURL string) {
	if b == nil || strings.TrimSpace(b.Version) == "" || strings.TrimSpace(b.File) == "" {
		return
	}
	dir := builtinComponentsPath()
	if dir == "" {
		log.Printf("[更新] 内置组件块跳过（本机没有可用的安装位置）")
		return
	}
	marker := filepath.Join(dir, builtinComponentsMarker)
	if readMark(marker) == strings.TrimSpace(b.Version) {
		log.Printf("[更新] 内置组件已是最新 %s，跳过", b.Version)
		return
	}
	url := findReleaseAssetURL(rel.Assets, b.File, sourceURL)
	if url == "" {
		log.Printf("[更新] 内置组件块在附件里找不到 %s，跳过", b.File)
		return
	}
	zipPath, cleanup, err := downloadUpdateFile(url, b.SHA256)
	if err != nil {
		log.Printf("[更新] 内置组件包下载失败（本地未改动）: %v", err)
		setUpdateError("内置组件更新失败: " + err.Error())
		return
	}
	defer cleanup()

	tmp, err := os.MkdirTemp("", "jxbi-")
	if err != nil {
		log.Printf("[更新] 内置组件临时目录创建失败: %v", err)
		return
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		log.Printf("[更新] 内置组件包解压失败（本地未改动）: %v", err)
		setUpdateError("内置组件包解压失败: " + err.Error())
		return
	}

	// 结构校验：包里必须能找到 cua-driver（Windows 是 cua-driver.exe，mac 是
	// cua-driver）。发布端可以把目录再套一层，所以用 findRootWith 找根而不是
	// 假定 zip 根就是目录根。
	root, err := findRootWith(tmp, filepath.Join("cua-driver", cuaDriverBinName()))
	if err != nil {
		log.Printf("[更新] 内置组件包结构不对（需含 cua-driver\\%s），已拒绝: %v", cuaDriverBinName(), err)
		setUpdateError("内置组件包结构不对: " + err.Error())
		return
	}

	// 目录级替换：旧目录先改名（Windows 上还能借它判断"是否被占用"）
	old := filepath.Join(filepath.Dir(dir), fmt.Sprintf("%s-old-%d", filepath.Base(dir), time.Now().Unix()))
	if err := os.Rename(dir, old); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[更新] 内置组件目录被占用（多半 cua-driver 还在跑），本轮让路: %v", err)
			setUpdateError("内置组件正在使用中，稍后自动重试")
			return
		}
		old = "" // 本地还没有：直接新建
	}
	if err := copyDir(root, dir); err != nil {
		_ = os.RemoveAll(dir)
		if old != "" {
			_ = os.Rename(old, dir)
		}
		log.Printf("[更新] 内置组件铺开失败（已回滚）: %v", err)
		setUpdateError("内置组件铺开失败: " + err.Error())
		return
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	_ = writeMark(marker, b.Version)
	st := a.loadUpdateBlockState()
	st.Builtin = b.Version
	_ = a.saveUpdateBlockState(st)
	log.Printf("[更新] 内置组件已静默更新到 %s（重启后内置连接器按新版本启动）", b.Version)
	a.broadcast(Event{Type: "update_ready", Title: "内置组件 " + b.Version})
}
