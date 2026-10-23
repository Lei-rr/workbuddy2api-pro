// crypto.go 凭证静态加密（v2.4.0 #7）：AES-256-GCM 文件级加密 + 目录迁移工具核心。
//
// 密钥：进程级 32 字节主密钥，来源环境变量 WB2A_MASTER_KEY（base64 std/url 或 64 位
// hex；空 = 未配置）。`security.encrypt_auths=true` 时 main 启动必须提供密钥否则
// fail-fast；命令行工具（cmd/signin、cmd/trial 等）不显式装配，Parse 遇到密文时会
// 惰性尝试从环境变量加载密钥，保证"同一套 auths 目录给多进程读"仍可用。
//
// 文件格式（整文件包装，明文旧文件自动兼容；文件结构与文件名不变）：
//
//	{"encrypted":true,"version":1,"nonce":"<base64 nonce>","data":"<base64 ciphertext>"}
//
// data 是「原明文 auth JSON」的 AES-256-GCM 密文（随机 12 字节 nonce，无 AAD）。
// 解密失败只报"密钥不正确/文件损坏"之类，绝不把密文内容写进错误/日志。
//
// 迁移：EncryptDir/DecryptDir 逐文件原子替换并保留一份备份（<name>.plain.bak /
// <name>.enc.bak），供 cmd/authcrypt 调用；手工命令见该命令 -h。
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// EnvMasterKey 主密钥环境变量名（与 config 侧注释/README 一致）。
	EnvMasterKey = "WB2A_MASTER_KEY"
	// masterKeyBytes AES-256 主密钥长度。
	masterKeyBytes = 32
	// encryptedFileVersion 当前加密文件格式版本（后续轮换格式时递增）。
	encryptedFileVersion = 1
	// cryptoPlanVersion 预留：密文 version 字段上界校验。（当前只有 1）
)

// 错误哨兵：调用方可用 errors.Is 区分"缺密钥"与"解密失败"。
var (
	// ErrMasterKeyMissing 未配置 WB2A_MASTER_KEY（或 SetMasterKey 未调用）。
	ErrMasterKeyMissing = errors.New("未配置主密钥 WB2A_MASTER_KEY（需 32 字节 base64 或 64 位 hex）")
	// ErrDecryptFailed 密文解密失败：主密钥不正确，或文件被篡改/损坏。
	ErrDecryptFailed = errors.New("auth 解密失败（主密钥不正确或密文被篡改/损坏）")
	// ErrEncryptionUnavailable 加密已启用但主密钥不可用（拒绝明文降级写回）。
	ErrEncryptionUnavailable = errors.New("auths 加密已启用但主密钥不可用，拒绝写回")
)

// 进程级加密状态：主密钥 + 是否启用加密（main 装配 / authcrypt 工具设置）。
var (
	cryptoMu          sync.RWMutex
	masterKey         []byte
	encryptionEnabled bool
)

// SetMasterKey 设置进程级主密钥。key 必须恰好 32 字节；非法长度返回 error 且不改
// 当前状态（防御：避免把 16 字节 AES-128 密钥误当 AES-256 用）。调用方自行持有 key
// 副本的销毁责任（本函数内部拷贝，调用方可立即清零自己的切片）。
func SetMasterKey(key []byte) error {
	if len(key) != masterKeyBytes {
		return fmt.Errorf("主密钥长度=%d 字节，必须为 %d 字节（AES-256）", len(key), masterKeyBytes)
	}
	cp := make([]byte, masterKeyBytes)
	copy(cp, key)
	cryptoMu.Lock()
	masterKey = cp
	cryptoMu.Unlock()
	return nil
}

// EnableEncryption 开启/关闭"写回时加密"开关（读路径不受影响：明文/密文始终都能读）。
// 开启后 SaveAtomic 写密文；关闭后 SaveAtomic 写回明文（配合 authcrypt -decrypt 回迁）。
func EnableEncryption(enabled bool) {
	cryptoMu.Lock()
	encryptionEnabled = enabled
	cryptoMu.Unlock()
}

// EncryptionEnabled 报告"写回时加密"开关是否开启（不代表主密钥一定可用）。
func EncryptionEnabled() bool {
	cryptoMu.RLock()
	defer cryptoMu.RUnlock()
	return encryptionEnabled
}

// getMasterKey 返回当前主密钥副本（未配置返回 nil）。
func getMasterKey() []byte {
	cryptoMu.RLock()
	defer cryptoMu.RUnlock()
	if len(masterKey) == 0 {
		return nil
	}
	cp := make([]byte, len(masterKey))
	copy(cp, masterKey)
	return cp
}

// MasterKeyFingerprint 返回主密钥 SHA-256 前 8 字节 hex（启动日志展示用；不可逆，
// 便于确认"多实例/多工具用的是同一把密钥"）。未配置返回空串。
func MasterKeyFingerprint() string {
	key := getMasterKey()
	if len(key) == 0 {
		return ""
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// LoadMasterKeyFromEnv 从 WB2A_MASTER_KEY 读取主密钥。
//
// 返回 (nil, nil) 表示"环境变量未设置"（调用方据此判断是否需要 fail-fast）；
// 设置了但无法解码 / 长度不是 32 字节 → 返回明确 error。
// 支持：base64 StdEncoding / RawStdEncoding / URLEncoding / RawURLEncoding，
// 以及 64 位 hex（推荐 base64：`openssl rand -base64 32`）。
func LoadMasterKeyFromEnv() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(EnvMasterKey))
	if raw == "" {
		return nil, nil
	}
	return ParseMasterKey(raw)
}

// ParseMasterKey 解析主密钥字符串（base64 或 hex），校验必须为 32 字节。
func ParseMasterKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrMasterKeyMissing
	}
	// 64 个字符先按 hex 尝试（64 位 hex）；否则按 base64 家族逐个尝试。
	if len(raw) == 2*masterKeyBytes {
		if b, err := hex.DecodeString(raw); err == nil {
			return validateMasterKey(b)
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(raw); err == nil {
			return validateMasterKey(b)
		}
	}
	return nil, fmt.Errorf("%s 无法解码（支持 base64 或 64 位 hex，解码后必须为 %d 字节）", EnvMasterKey, masterKeyBytes)
}

// validateMasterKey 校验解码后的密钥长度并返回副本。
func validateMasterKey(b []byte) ([]byte, error) {
	if len(b) != masterKeyBytes {
		return nil, fmt.Errorf("%s 解码后为 %d 字节，必须为 %d 字节（AES-256）", EnvMasterKey, len(b), masterKeyBytes)
	}
	cp := make([]byte, masterKeyBytes)
	copy(cp, b)
	return cp, nil
}

// activeKey 返回当前可用主密钥：优先进程内已设置；否则惰性尝试 WB2A_MASTER_KEY
// （成功后缓存，便于 cmd/signin 等未显式装配的进程读取加密 auths）。
func activeKey() ([]byte, error) {
	if key := getMasterKey(); len(key) > 0 {
		return key, nil
	}
	key, err := LoadMasterKeyFromEnv()
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		return nil, ErrMasterKeyMissing
	}
	if err := SetMasterKey(key); err != nil {
		return nil, err
	}
	return key, nil
}

// encryptedFile 加密文件包装（明文 auth JSON 的整文件密文）。
type encryptedFile struct {
	Encrypted bool   `json:"encrypted"`
	Version   int    `json:"version"`
	Nonce     string `json:"nonce"` // base64 std，12 字节 GCM nonce
	Data      string `json:"data"`  // base64 std，GCM 密文（含 tag）
}

// IsEncryptedFile 判断原始文件内容是否为加密包装（只看顶层 "encrypted":true）。
// 明文 auth JSON 不含该键，恒 false；非法 JSON 也返回 false（交给 Parse 报原错误）。
func IsEncryptedFile(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	var probe struct {
		Encrypted bool `json:"encrypted"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.Encrypted
}

// Encrypt 用当前主密钥加密一段明文，返回加密文件包装 JSON（随机 nonce）。
// 主密钥不可用 → 返回 error（ErrMasterKeyMissing / 解码错误）。
func Encrypt(plaintext []byte) ([]byte, error) {
	key, err := activeKey()
	if err != nil {
		return nil, err
	}
	return encryptWithKey(key, plaintext)
}

// Decrypt 解密加密文件包装，返回原始明文。密钥不可用/不正确/文件损坏 → error，
// 错误信息不含密文内容。
func Decrypt(encrypted []byte) ([]byte, error) {
	key, err := activeKey()
	if err != nil {
		return nil, err
	}
	return decryptWithKey(key, encrypted)
}

// encryptWithKey 纯函数（显式 key，便于测试）：AES-256-GCM 加密并包装成文件 JSON。
func encryptWithKey(key, plaintext []byte) ([]byte, error) {
	if len(key) != masterKeyBytes {
		return nil, fmt.Errorf("加密密钥长度=%d，必须为 %d", len(key), masterKeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成 nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)
	doc := encryptedFile{
		Encrypted: true,
		Version:   encryptedFileVersion,
		Nonce:     base64.StdEncoding.EncodeToString(nonce),
		Data:      base64.StdEncoding.EncodeToString(ciphertext),
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化加密文件: %w", err)
	}
	return raw, nil
}

// decryptWithKey 纯函数（显式 key，便于测试）：解包装 + AES-256-GCM 解密。
//
// 错误刻意不包含密文/密钥内容，只区分"格式/版本不对"与"解密失败（密钥错/损坏）"。
func decryptWithKey(key, encrypted []byte) ([]byte, error) {
	if len(key) != masterKeyBytes {
		return nil, fmt.Errorf("解密密钥长度=%d，必须为 %d", len(key), masterKeyBytes)
	}
	var doc encryptedFile
	if err := json.Unmarshal(encrypted, &doc); err != nil {
		return nil, fmt.Errorf("加密文件格式错误: %w", err)
	}
	if !doc.Encrypted {
		return nil, errors.New("不是加密文件（缺少 encrypted=true）")
	}
	if doc.Version != encryptedFileVersion {
		return nil, fmt.Errorf("不支持的加密文件版本 %d（当前支持 %d）", doc.Version, encryptedFileVersion)
	}
	nonce, err := base64.StdEncoding.DecodeString(doc.Nonce)
	if err != nil {
		return nil, fmt.Errorf("加密文件 nonce 损坏: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(doc.Data)
	if err != nil {
		return nil, fmt.Errorf("加密文件 data 损坏: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plaintext, nil
}

// ParseFile 读取并解析单个 auth 文件；错误带上文件路径（加密文件解密失败时明确
// 指出是哪个文件，便于 authcrypt 迁移与人工排查），不泄露密文内容。
func ParseFile(path string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 auth 文件 %s 失败: %w", path, err)
	}
	a, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("auth 文件 %s: %w", path, err)
	}
	a.FilePath = path
	return a, nil
}

// writeFileAtomic 与 SaveAtomic 同口径的 0600 原子写（crypto 迁移工具复用）。
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// CryptSummary 目录迁移统计（cmd/authcrypt 打印 + 退出码依据）。
type CryptSummary struct {
	Scanned   int  // 扫描到的 workbuddy*.json 文件数
	Encrypted int  // 本次加密的文件数（EncryptDir）
	Decrypted int  // 本次解密的文件数（DecryptDir）
	Skipped   int  // 已是目标形态（已加密/已明文）或不含 auth 的文件
	Failed    int  // 读取/校验/写盘失败的文件数（详情见 WARN 日志）
	DryRun    bool // 仅演练：不写盘、不生成备份
}

// EncryptDir 把目录下的明文 auth 文件批量加密（cmd/authcrypt -encrypt 的核心）。
//   - 已加密文件跳过；无法解析的明文文件跳过并计 Failed（不加密垃圾数据）；
//   - 每个文件先写备份 <name>.plain.bak（原明文 0600），再原子替换为密文；
//   - dryRun=true 时只统计不写盘（不生成备份）。
//
// 返回统计与"致命错误"（目录/密钥问题）；单文件失败只计数不中断迁移。
func EncryptDir(dir string, dryRun bool) (CryptSummary, error) {
	s := CryptSummary{DryRun: dryRun}
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return s, fmt.Errorf("扫描目录 %s: %w", dir, err)
	}
	key, err := activeKey()
	if err != nil {
		return s, err
	}
	for _, f := range files {
		s.Scanned++
		raw, err := os.ReadFile(f)
		if err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 读取 %s 失败: %v", f, err)
			continue
		}
		if IsEncryptedFile(raw) {
			s.Skipped++
			continue
		}
		if _, err := parsePlain(raw); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 跳过无法解析的明文 auth 文件 %s: %v", f, err)
			continue
		}
		encrypted, err := encryptWithKey(key, raw)
		if err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 加密 %s 失败: %v", f, err)
			continue
		}
		if dryRun {
			s.Encrypted++
			continue
		}
		// 先落备份（原明文），再原子替换为密文：任一步失败保留原文件不变。
		if err := writeFileAtomic(f+".plain.bak", raw, 0o600); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 写备份 %s.plain.bak 失败: %v", f, err)
			continue
		}
		if err := writeFileAtomic(f, encrypted, 0o600); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 写入加密文件 %s 失败: %v", f, err)
			continue
		}
		s.Encrypted++
	}
	return s, nil
}

// DecryptDir 把目录下的加密 auth 文件批量解密回明文（cmd/authcrypt -decrypt 的核心）。
//   - 明文文件跳过；解密失败/解密结果不是合法 auth JSON 的文件跳过并计 Failed；
//   - 每个文件先写备份 <name>.enc.bak（原密文 0600），再原子替换为原明文内容；
//   - dryRun=true 时只统计不写盘（不生成备份）。
func DecryptDir(dir string, dryRun bool) (CryptSummary, error) {
	s := CryptSummary{DryRun: dryRun}
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return s, fmt.Errorf("扫描目录 %s: %w", dir, err)
	}
	key, err := activeKey()
	if err != nil {
		return s, err
	}
	for _, f := range files {
		s.Scanned++
		raw, err := os.ReadFile(f)
		if err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 读取 %s 失败: %v", f, err)
			continue
		}
		if !IsEncryptedFile(raw) {
			s.Skipped++
			continue
		}
		plain, err := decryptWithKey(key, raw)
		if err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 解密 %s 失败: %v", f, err)
			continue
		}
		if _, err := parsePlain(plain); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 解密结果不是合法 auth JSON，跳过 %s: %v", f, err)
			continue
		}
		if dryRun {
			s.Decrypted++
			continue
		}
		if err := writeFileAtomic(f+".enc.bak", raw, 0o600); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 写备份 %s.enc.bak 失败: %v", f, err)
			continue
		}
		if err := writeFileAtomic(f, plain, 0o600); err != nil {
			s.Failed++
			log.Printf("WARN: authcrypt: 写入明文文件 %s 失败: %v", f, err)
			continue
		}
		s.Decrypted++
	}
	return s, nil
}
