package main

// ADB 主机协议客户端（纯 Go，无外部依赖）：无线 ADB 能力的引擎侧实现。
//
// 用途：手机上的引擎作为 ADB 主机连到 adbd——典型目标是本机无线调试端口
//（开发者选项→无线调试，shell uid 权限：screencap/input/pm 全可用）或局域网
// 内其他安卓设备。认证走经典 RSA：CNXN → AUTH(TOKEN) → 签名 →（被拒时）
// 发公钥触发设备上的授权弹窗（用户点允许后永久记住）。密钥对持久化在
// dataDir/adbkey（PEM PKCS#8），公钥用 ADB 的 ssh 线格式 base64。
//
// 协议要点（API 35 adbd 实测）：①报文字段一律小端；②CNXN 校验和字段按现代
// adb 惯例置 0（adbd 见 0 跳过校验）；③v1 shell: 与 exec: 服务已被移除，
// 全部走 shell,v2,raw:（帧 = [id:1][len:u32le][data]，id1=stdout/2=stderr/
// 3=exit；raw 模式无 PTY，二进制安全）。

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	adbMaxData     = 262144
	adbConnectTout = 8 * time.Second
	adbAuthWait    = 60 * time.Second // 发公钥后等用户在设备上点「允许」的窗口
)

// adbMsg 是一条 ADB 报文（24 字节头 + payload）。
type adbMsg struct {
	Cmd  string
	Arg0 uint32
	Arg1 uint32
	Data []byte
}

func adbChecksum(b []byte) uint32 { return crc32.ChecksumIEEE(b) }

func adbPack(m adbMsg) []byte {
	// ADB 报文字段一律小端（与 AOSP 实现一致；CNXN 的校验和字段按现代 adb
	// 惯例置 0——adbd 见 0 跳过校验，见非零才验，真 adb 也发 0）
	h := make([]byte, 24)
	copy(h, m.Cmd)
	binary.LittleEndian.PutUint32(h[4:8], m.Arg0)
	binary.LittleEndian.PutUint32(h[8:12], m.Arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(m.Data)))
	if m.Cmd == "CNXN" {
		binary.LittleEndian.PutUint32(h[16:20], 0)
	} else {
		binary.LittleEndian.PutUint32(h[16:20], adbChecksum(m.Data))
	}
	binary.LittleEndian.PutUint32(h[20:24], binary.LittleEndian.Uint32(h[0:4])^0xFFFFFFFF)
	return append(h, m.Data...)
}

func adbReadMsg(r io.Reader) (adbMsg, error) {
	h := make([]byte, 24)
	if _, err := io.ReadFull(r, h); err != nil {
		return adbMsg{}, err
	}
	m := adbMsg{
		Cmd:  string(h[0:4]),
		Arg0: binary.LittleEndian.Uint32(h[4:8]),
		Arg1: binary.LittleEndian.Uint32(h[8:12]),
		Data: make([]byte, binary.LittleEndian.Uint32(h[12:16])),
	}
	if len(m.Data) > 1<<24 {
		return adbMsg{}, errors.New("adb 报文超长")
	}
	if _, err := io.ReadFull(r, m.Data); err != nil {
		return adbMsg{}, err
	}
	return m, nil
}

// adbKeys 持久化的 RSA 密钥对（懒生成）。
type adbKeys struct {
	Priv *rsa.PrivateKey
	Pub  string // ADB 公钥串（base64 + " name\n"）
}

var (
	adbKeysOnce sync.Once
	adbKeysVal  *adbKeys
	adbKeysErr  error
)

func adbLoadKeys(keyPath string) (*adbKeys, error) {
	if b, err := os.ReadFile(keyPath); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, errors.New("adbkey PEM 解码失败")
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("adbkey 解析失败: %w", err)
		}
		priv, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("adbkey 不是 RSA 密钥")
		}
		return &adbKeys{Priv: priv, Pub: adbPublicKey(priv, keyPath)}, nil
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll(filepath.Dir(keyPath), 0700)
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	return &adbKeys{Priv: priv, Pub: adbPublicKey(priv, keyPath)}, nil
}

// adbPublicKey 生成 ADB 认识的公钥串：ssh 线格式（string "ssh-rsa" +
// mpint e + mpint n）整体 base64，后跟 " name\n"。mpint 是大端字节，
// 首字节高位为 1 时须补 0x00（n 恒如此，e=65537 不用）。
func adbPublicKey(k *rsa.PrivateKey, keyPath string) string {
	var w bytes.Buffer
	putStr := func(s string) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(s)))
		w.Write(l[:])
		w.WriteString(s)
	}
	putInt := func(v *big.Int) {
		b := v.Bytes()
		if len(b) > 0 && b[0]&0x80 != 0 {
			b = append([]byte{0}, b...)
		}
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(b)))
		w.Write(l[:])
		w.Write(b)
	}
	putStr("ssh-rsa")
	putInt(big.NewInt(int64(k.PublicKey.E)))
	putInt(k.PublicKey.N)
	return base64.StdEncoding.EncodeToString(w.Bytes()) + " jx-assistant\n"
}

// adbConn 是一条已认证的 adbd 连接（进程级单例，按地址重连）。
type adbConn struct {
	mu      sync.Mutex
	addr    string
	conn    net.Conn
	info    string // 对端 CNXN 的 system identity
	lastErr error
}

var adbPrimary adbConn

// adbStateText 供环境快照/自检描述当前连接状态。
func adbStateText() string {
	adbPrimary.mu.Lock()
	defer adbPrimary.mu.Unlock()
	if adbPrimary.conn == nil {
		if adbPrimary.lastErr != nil {
			return "未连接（上次错误：" + adbPrimary.lastErr.Error() + "）"
		}
		return "未连接"
	}
	return "已连接 " + adbPrimary.addr + "（" + adbPrimary.info + "）"
}

// adbConnect 建立到 addr（host:port）的认证连接；成功后替换全局单例。
func adbConnect(keyPath, addr string) (string, error) {
	adbKeysOnce.Do(func() { adbKeysVal, adbKeysErr = adbLoadKeys(keyPath) })
	if adbKeysErr != nil {
		return "", adbKeysErr
	}
	conn, err := net.DialTimeout("tcp", addr, adbConnectTout)
	if err != nil {
		adbPrimary.mu.Lock()
		adbPrimary.lastErr = err
		adbPrimary.mu.Unlock()
		return "", fmt.Errorf("连接 %s 失败: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(adbAuthWait))
	if info, err := adbHandshake(conn, adbKeysVal); err != nil {
		conn.Close()
		adbPrimary.mu.Lock()
		adbPrimary.lastErr = err
		adbPrimary.mu.Unlock()
		return "", err
	} else {
		adbPrimary.mu.Lock()
		if adbPrimary.conn != nil {
			adbPrimary.conn.Close()
		}
		adbPrimary.conn, adbPrimary.addr, adbPrimary.info, adbPrimary.lastErr = conn, addr, info, nil
		adbPrimary.mu.Unlock()
		return info, nil
	}
}

// adbHandshake 完成 CNXN/AUTH 认证，返回对端 identity。
func adbHandshake(conn net.Conn, keys *adbKeys) (string, error) {
	id := []byte("host::features=shell_v2,cmd,stat_v2,ls_v2,fixed_push_mkdir,apex,abb,fixed_push_symlink_timestamp,abb_exec,remount_shell,track_app,sendrecv_v2,sendrecv_v2_brotli,sendrecv_v2_lz4,sendrecv_v2_zstd,sendrecv_v2_dry_run_send,devicetracker_proto_format,devraw,app_info,server_status,track_mdns")
	if _, err := conn.Write(adbPack(adbMsg{Cmd: "CNXN", Arg0: 0x01000001, Arg1: adbMaxData, Data: id})); err != nil {
		return "", err
	}
	for {
		m, err := adbReadMsg(conn)
		if err != nil {
			return "", err
		}
		switch m.Cmd {
		case "CNXN":
			_ = conn.SetDeadline(time.Time{})
			return strings.TrimSpace(string(m.Data)), nil
		case "AUTH":
			if m.Arg0 != 1 || len(m.Data) != 20 {
				return "", fmt.Errorf("意外的 AUTH 类型 %d", m.Arg0)
			}
			// 先发签名；多数已授权设备直接过。未授权设备会再回 TOKEN，
			// 此时发公钥触发设备上的「允许 USB 调试」弹窗。
			sig, err := rsa.SignPKCS1v15(rand.Reader, keys.Priv, crypto.SHA1, m.Data)
			if err != nil {
				return "", err
			}
			if _, err := conn.Write(adbPack(adbMsg{Cmd: "AUTH", Arg0: 2, Data: sig})); err != nil {
				return "", err
			}
			m2, err := adbReadMsg(conn)
			if err != nil {
				return "", err
			}
			if m2.Cmd == "CNXN" {
				_ = conn.SetDeadline(time.Time{})
				return strings.TrimSpace(string(m2.Data)), nil
			}
			if m2.Cmd == "AUTH" && m2.Arg0 == 1 {
				if _, err := conn.Write(adbPack(adbMsg{Cmd: "AUTH", Arg0: 3, Data: []byte(keys.Pub)})); err != nil {
					return "", err
				}
				continue // 等用户在设备上点「允许」，随后 CNXN（或断开重来）
			}
			return "", fmt.Errorf("认证流程意外报文 %s/%d", m2.Cmd, m2.Arg0)
		default:
			return "", fmt.Errorf("握手意外报文 %s", m.Cmd)
		}
	}
}

// adbStream 打开一个本地流并等待对端 OKAY；被拒（CLSE）时报错。
func adbStream(conn net.Conn, service string) error {
	if _, err := conn.Write(adbPack(adbMsg{Cmd: "OPEN", Arg0: 1, Arg1: 0, Data: append([]byte(service), 0)})); err != nil {
		return err
	}
	m, err := adbReadMsg(conn)
	if err != nil {
		return err
	}
	if m.Cmd == "CLSE" {
		return errors.New("adbd 拒绝了该服务")
	}
	if m.Cmd != "OKAY" {
		return fmt.Errorf("OPEN 应答 %s", m.Cmd)
	}
	return nil
}

// adbCollectV2 收流直到 CLSE：逐块应答 OKAY。payload 按 shell v2 raw 帧解析
//（[id:1][len:u32le][data]，一个 WRTE 可含多帧）：id1=stdout、id2=stderr、
// id3=exit。stdout 进 out；binary=true 时只收 stdout（screencap 等裸流用），
// 否则 stderr 也并入 out 尾部。返回退出码（无则 -1）。
func adbCollectV2(conn net.Conn, w io.Writer, rawOnly bool) (int, error) {
	exit := -1
	var errBuf bytes.Buffer
	for {
		m, err := adbReadMsg(conn)
		if err != nil {
			return exit, err
		}
		switch m.Cmd {
		case "WRTE":
			if _, err := conn.Write(adbPack(adbMsg{Cmd: "OKAY", Arg0: 1, Arg1: 0})); err != nil {
				return exit, err
			}
			for len(m.Data) > 0 {
				id := m.Data[0]
				if len(m.Data) < 5 {
					break // 残帧：下一块续（v2 帧可跨 WRTE 边界——按协议不会，
					// 但容错处理免得死循环）
				}
				n := int(binary.LittleEndian.Uint32(m.Data[1:5]))
				if n < 0 || 5+n > len(m.Data) {
					break
				}
				payload := m.Data[5 : 5+n]
				m.Data = m.Data[5+n:]
				switch id {
				case 1:
					if _, err := w.Write(payload); err != nil {
						return exit, err
					}
				case 2:
					errBuf.Write(payload)
				case 3:
					if len(payload) > 0 {
						exit = int(payload[0])
					}
				}
			}
		case "CLSE":
			if _, err := conn.Write(adbPack(adbMsg{Cmd: "CLSE", Arg0: 1, Arg1: 0})); err != nil {
				return exit, err
			}
			if !rawOnly && errBuf.Len() > 0 {
				_, _ = w.Write(errBuf.Bytes())
			}
			return exit, nil
		default:
			return exit, fmt.Errorf("流上意外报文 %s", m.Cmd)
		}
	}
}

// adbShellWithConn 在给定连接上跑命令（shell v2 raw：新版 adbd 已移除 v1
// shell: 与 exec: 服务；v2 raw 同样二进制安全），返回合并输出与退出码。
func adbShellWithConn(conn net.Conn, command string, timeout time.Duration) (string, int, error) {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})
	if err := adbStream(conn, "shell,v2,raw:"+command); err != nil {
		return "", -1, err
	}
	var buf bytes.Buffer
	exit, err := adbCollectV2(conn, &buf, false)
	if err != nil {
		return "", exit, err
	}
	out := buf.String()
	note := ""
	if len(out) > 128*1024 {
		out = clipUTF8Tail(out, 128*1024) // 按字节切会劈开多字节字符（走同一套边界对齐）
		note = "\n[输出过长已截断]"
	}
	if exit > 0 {
		note += fmt.Sprintf("\n[退出码 %d]", exit)
	}
	return out + note, exit, nil
}

// adbShellText 对全局连接执行文本命令。
func adbShellText(command string, timeout time.Duration) (string, error) {
	adbPrimary.mu.Lock()
	conn, addr := adbPrimary.conn, adbPrimary.addr
	adbPrimary.mu.Unlock()
	if conn == nil {
		return "", fmt.Errorf("ADB 未连接（先用 adb_connect，当前状态：%s）", adbStateText())
	}
	out, _, err := adbShellWithConn(conn, command, timeout)
	if err != nil {
		// 连接坏了就清掉，下次调用提示重连
		adbPrimary.mu.Lock()
		if adbPrimary.conn == conn {
			adbPrimary.conn = nil
			adbPrimary.lastErr = err
		}
		adbPrimary.mu.Unlock()
		return "", fmt.Errorf("在 %s 上执行失败: %w", addr, err)
	}
	return out, nil
}

// adbExecRaw 在全局连接上跑命令并取回裸二进制（同样走 shell v2 raw——
// raw 模式无 PTY，不做 CRLF 改写，PNG 等二进制流安全）。
func adbExecRaw(command string, timeout time.Duration) ([]byte, error) {
	adbPrimary.mu.Lock()
	conn := adbPrimary.conn
	adbPrimary.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("ADB 未连接（先用 adb_connect，当前状态：%s）", adbStateText())
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})
	if err := adbStream(conn, "shell,v2,raw:"+command); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := adbCollectV2(conn, &buf, true); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
