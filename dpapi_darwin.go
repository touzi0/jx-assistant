//go:build darwin

package main

// macOS 的敏感字段静态加密（dpapi_windows.go 的对等物）：主密钥存登录
// 钥匙串（用户级、静态加密，绑定当前用户——密文配置被拷到别的机器/账户
// 也解不开，与 DPAPI 同一意图），敏感值用主密钥 AES-256-GCM 加密，格式
// 沿用 dpapi:v1:<base64>（config.go 的 protectSecret/unprotectSecret 通用）。
//
// 钥匙串经系统 security CLI 读写（无第三方依赖；本用户创建的 generic
// password 无 ACL 限制，读取不弹窗）。钥匙串不可用（无登录会话等）时
// 返回错误，protectSecret 按 fail-open 设计保持明文可读写、不丢配置。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const keychainService = "jx-assistant"
const keychainAccount = "master-key"

// keychainMasterKey 取（缺失则生成并写入）登录钥匙串里的 32 字节主密钥。
func keychainMasterKey() ([]byte, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-w", "-s", keychainService, "-a", keychainAccount).Output()
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(out)))
		if derr == nil && len(key) == 32 {
			return key, nil
		}
		// 钥匙串里的值不合法：按损坏处理，重建（-U 覆盖）
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := exec.Command("security", "add-generic-password", "-U",
		"-s", keychainService, "-a", keychainAccount,
		"-w", hex.EncodeToString(key)).Run(); err != nil {
		return nil, fmt.Errorf("写入钥匙串失败: %w", err)
	}
	return key, nil
}

func dpapiProtect(plain []byte) ([]byte, error) {
	key, err := keychainMasterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func dpapiUnprotect(blob []byte) ([]byte, error) {
	key, err := keychainMasterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(blob) < gcm.NonceSize() {
		return nil, errors.New("密文太短")
	}
	return gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
}
