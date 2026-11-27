package main

// 中继通道的「同一设备号只让一个实例注册」守卫 + 重连节奏常量。
//
// 背景：config.json 里的 remoteAccess.myId 是**按安装**生成的，同一 data 目录下的
// 多个实例（桌面版 + 控制台版 + 测试实例）会拿到同一个设备号。网关按设备号路由，
// 同一个号只保留一条通道——于是各实例互相把对方踢下线，谁都不停地「注册成功→被断
// →重连→再注册」。这正是真机上看到的每秒 3-6 次重注册（日志累计 2 万多次）的主因，
// 也是手机入口反复闪断的原因。
//
// 做法：中继归属落一个小文件（data/relay.owner，内容是持有者的 pid）。启动/改配置
// 时如果发现持有者还活着，就不重复注册，只记一行日志；进程退出或关闭开关时释放。

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// relayRegLoggedAt 上次记「已注册」日志的时刻（unix ms），用于日志节流。
var relayRegLoggedAt atomic.Int64

// relayCooldownUntil 验密失败熔断的重连冷却截止（unix ms，跨连接生效：hubCtx
// 每次重连新建，冷却必须留在包级）。见 remoteaccess.go 的 noteAuthFail。
var relayCooldownUntil atomic.Int64

// 重连节奏：稳定链路断了就快速重连；「注册成功后很快被断」要按退避递增——
// 那正是被网关踢（设备号冲突）或网关异常的形态，立刻重连只会变成热循环。
const (
	relayRetryMin      = 5 * time.Second
	relayRetryMax      = 30 * time.Second
	relayReconnectFast = time.Second      // 稳定链路正常断线后的重连延迟：手机端的 503「中继 offline」空窗就是这一秒（2026-11-27 从 5s 压下来）
	relayStableAfter   = 60 * time.Second // 连接活过这么久再断，视为「正常断线」，快速重连
	relayRegLogEvery   = time.Minute      // 「已注册」日志节流（重连风暴时别刷爆日志）
	relayWriteTimeout  = 20 * time.Second // 单帧写超时：网关停读/链路停滞时写会无限期持锁卡住 ping（见 hubCtx.writeFrame）
)

// 验密失败熔断（2026-11-04 洪泛防护）：/relay 匿名，攻击者知道设备号即可持续
// 灌密文（每条都要家端做一次 AES-GCM 解密尝试）。窗口内连败超阈即主动断开
// hub 连接并冷却——网关侧该设备的挂起中继全部立即失败（503），代价从家端
// 解密 CPU 转嫁给连接层；冷却结束重连恢复。真用户密钥输错远达不到阈值，且
// 成功一次即清零计数。
const (
	relayAuthFailWindow = 30 * time.Second // 失败计数观测窗
	relayAuthFailBurst  = 20               // 窗口内连败熔断阈值
	relayAuthFailCool   = 60 * time.Second // 熔断后的重连冷却
)

// relayLockPath 中继归属文件路径（放 data 目录，与库、配置同处）。
func (a *App) relayLockPath() string {
	return filepath.Join(a.Cfg.DataDir, "relay.owner")
}

// relayOwnedByOther 判断中继是否已由另一个活着的实例持有。
func (a *App) relayOwnedByOther() (int, bool) {
	b, err := os.ReadFile(a.relayLockPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if pid == os.Getpid() {
		return pid, false
	}
	return pid, processAlive(pid)
}

// claimRelay 登记本实例为持有者（覆盖旧内容：走到这里说明没有别的活实例持有）。
func (a *App) claimRelay() {
	_ = os.WriteFile(a.relayLockPath(), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644)
}

// releaseRelay 释放归属——只在自己持有的时候删，别把别人的删掉。
func (a *App) releaseRelay() {
	if pid, other := a.relayOwnedByOther(); !other && pid == os.Getpid() {
		_ = os.Remove(a.relayLockPath())
	}
}
