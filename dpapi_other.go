//go:build !windows && !darwin

package main

// 非 Windows/macOS 平台暂无系统凭据保护：加密直通（返回原值），保持配置
// 可读写。密文迁移只在 Windows（DPAPI）与 macOS（钥匙串，dpapi_darwin.go）
// 上发生；Linux 侧敏感字段仍是明文落盘（如实文档）。

func dpapiProtect(plain []byte) ([]byte, error) { return plain, nil }

func dpapiUnprotect(cipher []byte) ([]byte, error) { return cipher, nil }
