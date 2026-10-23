package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resetCryptoState 清理进程级加密状态。本文件所有测试都不 t.Parallel（全局状态 +
// t.Setenv），cleanup 保证不污染 auth_test.go 的并行用例。
func resetCryptoState(t *testing.T) {
	t.Helper()
	clear := func() {
		cryptoMu.Lock()
		masterKey = nil
		encryptionEnabled = false
		cryptoMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// testMasterKey 测试用固定 32 字节主密钥。
func testMasterKey() []byte { return bytes.Repeat([]byte{0x42}, 32) }

// encryptJSON 用显式 key 生成加密文件内容（不依赖进程级状态）。
func encryptJSON(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	enc, err := encryptWithKey(key, plaintext)
	if err != nil {
		t.Fatalf("encryptWithKey: %v", err)
	}
	return enc
}

// validPlainAuth 一段合法明文 auth JSON。
func validPlainAuth(uid, accessToken string) []byte {
	return []byte(`{"auth":{"accessToken":"` + accessToken + `","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"` + uid + `"}}`)
}

// TestSetMasterKeyValidation 主密钥长度校验 + 内部拷贝（调用方改源切片不影响已设置密钥）。
func TestSetMasterKeyValidation(t *testing.T) {
	resetCryptoState(t)
	for _, n := range []int{0, 1, 16, 31, 33, 64} {
		if err := SetMasterKey(make([]byte, n)); err == nil {
			t.Fatalf("SetMasterKey(%d 字节) 应报错", n)
		}
	}
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatalf("SetMasterKey(32B): %v", err)
	}
	key[0] ^= 0xff // 篡改调用方切片：内部必须是拷贝
	got := getMasterKey()
	if len(got) != 32 || got[0] != 0x42 {
		t.Fatalf("主密钥不是内部拷贝: %x", got)
	}
}

// TestEncryptionEnabledToggle 开关读写（默认关闭）。
func TestEncryptionEnabledToggle(t *testing.T) {
	resetCryptoState(t)
	if EncryptionEnabled() {
		t.Fatal("默认应关闭加密")
	}
	EnableEncryption(true)
	if !EncryptionEnabled() {
		t.Fatal("EnableEncryption(true) 后应开启")
	}
	EnableEncryption(false)
	if EncryptionEnabled() {
		t.Fatal("EnableEncryption(false) 后应关闭")
	}
}

// TestLoadMasterKeyFromEnv 环境变量解析：base64（std/raw/url）、hex、空、非法长度、垃圾。
func TestLoadMasterKeyFromEnv(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()

	t.Setenv(EnvMasterKey, "")
	if got, err := LoadMasterKeyFromEnv(); err != nil || got != nil {
		t.Fatalf("未设置: got=%x err=%v want nil/nil", got, err)
	}

	for name, encoded := range map[string]string{
		"base64-std":      base64.StdEncoding.EncodeToString(key),
		"base64-raw":      base64.RawStdEncoding.EncodeToString(key),
		"base64-url":      base64.URLEncoding.EncodeToString(key),
		"base64-raw-url":  base64.RawURLEncoding.EncodeToString(key),
		"hex":             hex.EncodeToString(key),
		"padded with spc": "  " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		t.Setenv(EnvMasterKey, encoded)
		got, err := LoadMasterKeyFromEnv()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, key) {
			t.Fatalf("%s: decoded=%x want %x", name, got, key)
		}
	}

	// 长度错误：16 字节 base64
	t.Setenv(EnvMasterKey, base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := LoadMasterKeyFromEnv(); err == nil || !strings.Contains(err.Error(), "16") {
		t.Fatalf("16 字节密钥应报错：%v", err)
	}
	// 无法解码
	t.Setenv(EnvMasterKey, "%%%not-a-key%%%")
	if _, err := LoadMasterKeyFromEnv(); err == nil {
		t.Fatal("垃圾密钥应报错")
	}
	if _, err := ParseMasterKey(""); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("空串 err=%v want ErrMasterKeyMissing", err)
	}
}

// TestEncryptDecryptRoundtrip 加密/解密 roundtrip + 随机 nonce（同明文两次密文不同）。
func TestEncryptDecryptRoundtrip(t *testing.T) {
	resetCryptoState(t)
	if err := SetMasterKey(testMasterKey()); err != nil {
		t.Fatal(err)
	}
	EnableEncryption(true)
	plain := validPlainAuth("u1", "at-secret")

	enc1, err := Encrypt(plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !IsEncryptedFile(enc1) {
		t.Fatal("输出应为加密包装")
	}
	var doc encryptedFile
	if err := json.Unmarshal(enc1, &doc); err != nil {
		t.Fatalf("wrapper 非法 JSON: %v", err)
	}
	if !doc.Encrypted || doc.Version != 1 || doc.Nonce == "" || doc.Data == "" {
		t.Fatalf("wrapper 字段异常: %+v", doc)
	}
	if got, err := Decrypt(enc1); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Decrypt: got=%q err=%v", got, err)
	}
	if bytes.Contains(enc1, []byte("at-secret")) {
		t.Fatal("密文里不应出现明文 accessToken")
	}

	enc2, err := Encrypt(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(enc1, enc2) {
		t.Fatal("随机 nonce 应让两次加密结果不同")
	}
	if IsEncryptedFile(plain) {
		t.Fatal("明文不应被识别为加密文件")
	}
	if err := SetMasterKey(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(enc1); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("错误密钥解密 err=%v want ErrDecryptFailed", err)
	}
}

// TestDecryptTamperedAndBadWrappers 篡改密文/坏格式/不支持版本：明确报错且不泄露密文。
func TestDecryptTamperedAndBadWrappers(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	enc := encryptJSON(t, key, validPlainAuth("u1", "at"))

	// 篡改 data 中一个字节
	var doc encryptedFile
	if err := json.Unmarshal(enc, &doc); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(doc.Data)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 0xff
	doc.Data = base64.StdEncoding.EncodeToString(data)
	tampered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	_, decErr := Decrypt(tampered)
	if !errors.Is(decErr, ErrDecryptFailed) {
		t.Fatalf("篡改密文 err=%v want ErrDecryptFailed", decErr)
	}
	if strings.Contains(decErr.Error(), doc.Data) {
		t.Fatal("错误信息不应包含密文")
	}

	badCases := map[string][]byte{
		"not json":         []byte(`not-json`),
		"not encrypted":    []byte(`{"encrypted":false}`),
		"unsupported ver":  []byte(`{"encrypted":true,"version":99,"nonce":"AA==","data":"AA=="}`),
		"bad nonce base64": []byte(`{"encrypted":true,"version":1,"nonce":"!!!","data":"AA=="}`),
		"bad data base64":  []byte(`{"encrypted":true,"version":1,"nonce":"AA==","data":"!!!"}`),
	}
	for name, raw := range badCases {
		if _, err := Decrypt(raw); err == nil {
			t.Errorf("%s: 应报错", name)
		}
	}
}

// TestParseEncryptedAndPlain Parse：明文/密文都能解析（混合目录兼容）。
func TestParseEncryptedAndPlain(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	plain := validPlainAuth("u1", "at-enc")

	// 明文（未配置密钥也应可读）
	a, err := Parse(plain)
	if err != nil || a.UID != "u1" || a.AccessToken != "at-enc" {
		t.Fatalf("明文 Parse: %+v err=%v", a, err)
	}

	// 密文：设置密钥后 Parse 自动解密
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	enc := encryptJSON(t, key, plain)
	a, err = Parse(enc)
	if err != nil || a.UID != "u1" || a.AccessToken != "at-enc" {
		t.Fatalf("密文 Parse: %+v err=%v", a, err)
	}

	// 错误密钥：报解密失败，不含密文
	if err := SetMasterKey(bytes.Repeat([]byte{0x11}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(enc); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("错误密钥 Parse err=%v", err)
	}
}

// TestParseFileErrorHasFilename ParseFile：解密失败错误带文件名、不含密文。
func TestParseFileErrorHasFilename(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-bad.json")
	enc := encryptJSON(t, key, validPlainAuth("u1", "at"))
	if err := os.WriteFile(fp, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	// 换错误密钥 → ParseFile 错误应点名文件、不含密文
	if err := SetMasterKey(bytes.Repeat([]byte{0x22}, 32)); err != nil {
		t.Fatal(err)
	}
	_, err := ParseFile(fp)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), fp) {
		t.Fatalf("错误应包含文件名 %s: %v", fp, err)
	}
	var doc encryptedFile
	_ = json.Unmarshal(enc, &doc)
	if strings.Contains(err.Error(), doc.Data) {
		t.Fatal("错误信息不应包含密文")
	}

	// 正常路径：ParseFile 回填 FilePath
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "workbuddy-good.json")
	if err := os.WriteFile(good, validPlainAuth("u2", "at2"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseFile(good)
	if err != nil || a.FilePath != good || a.UID != "u2" {
		t.Fatalf("ParseFile: %+v err=%v", a, err)
	}
}

// TestSaveAtomicEncrypted 开启加密后 SaveAtomic 落盘密文（内部 JSON 不变、可 Parse 回读）。
func TestSaveAtomicEncrypted(t *testing.T) {
	resetCryptoState(t)
	if err := SetMasterKey(testMasterKey()); err != nil {
		t.Fatal(err)
	}
	EnableEncryption(true)

	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at-secret-xyz", RefreshToken: "rt", ExpiresAt: 1, UID: "u1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncryptedFile(raw) {
		t.Fatal("开启加密后落盘应为密文")
	}
	for _, plain := range []string{"at-secret-xyz", "accessToken", "refreshToken"} {
		if bytes.Contains(raw, []byte(plain)) {
			t.Fatalf("密文文件泄露明文 %q: %s", plain, raw)
		}
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("回读密文: %v", err)
	}
	if b.AccessToken != "at-secret-xyz" || b.UID != "u1" || b.Nickname != "n1" {
		t.Fatalf("roundtrip: %+v", b)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp 文件不应残留")
	}
}

// TestSaveAtomicRefusesWithoutKey 加密开启但主密钥不可用：拒绝写回（不静默降级明文）。
func TestSaveAtomicRefusesWithoutKey(t *testing.T) {
	resetCryptoState(t)
	t.Setenv(EnvMasterKey, "")
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	original := `{"auth":{"accessToken":"old","refreshToken":"r","expiresAt":1},"account":{"uid":"u1"}}`
	if err := os.WriteFile(fp, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	EnableEncryption(true) // 故意不设主密钥
	a := &Auth{AccessToken: "new-at", UID: "u1", FilePath: fp}
	if err := a.SaveAtomic(); !errors.Is(err, ErrEncryptionUnavailable) {
		t.Fatalf("SaveAtomic err=%v want ErrEncryptionUnavailable", err)
	}
	if raw, _ := os.ReadFile(fp); string(raw) != original {
		t.Fatalf("拒绝写回时文件必须保持不变: %s", raw)
	}
	// Encrypt 同样应返回缺密钥错误
	if _, err := Encrypt([]byte(`{}`)); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("Encrypt err=%v want ErrMasterKeyMissing", err)
	}
}

// TestLoadDirMixed 混合目录：明文+密文都加载；损坏密文重点 WARN（含文件名）；
// 加密开启时对明文文件汇总 WARN 提示跑 authcrypt。
func TestLoadDirMixed(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	EnableEncryption(true)

	dir := t.TempDir()
	plainPath := filepath.Join(dir, "workbuddy-plain.json")
	if err := os.WriteFile(plainPath, validPlainAuth("u-plain", "at1"), 0o600); err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "workbuddy-enc.json")
	if err := os.WriteFile(encPath, encryptJSON(t, key, validPlainAuth("u-enc", "at2")), 0o600); err != nil {
		t.Fatal(err)
	}
	// 损坏密文：改一个字节
	badEnc := encryptJSON(t, key, validPlainAuth("u-bad", "at3"))
	var doc encryptedFile
	_ = json.Unmarshal(badEnc, &doc)
	data, _ := base64.StdEncoding.DecodeString(doc.Data)
	data[0] ^= 0x01
	doc.Data = base64.StdEncoding.EncodeToString(data)
	badEnc, _ = json.Marshal(doc)
	badPath := filepath.Join(dir, "workbuddy-corrupt.json")
	if err := os.WriteFile(badPath, badEnc, 0o600); err != nil {
		t.Fatal(err)
	}

	// 捕获日志（本测试不 parallel；log 是全局的）。
	old := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	list, err := LoadDir(dir)
	log.SetOutput(old)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 accounts(明文+密文), got %d", len(list))
	}
	uids := map[string]bool{}
	for _, a := range list {
		uids[a.UID] = true
	}
	if !uids["u-plain"] || !uids["u-enc"] {
		t.Fatalf("加载结果=%v", uids)
	}
	logs := buf.String()
	if !strings.Contains(logs, badPath) || !strings.Contains(logs, "解密失败") {
		t.Fatalf("损坏密文应 WARN 文件名+解密失败:\n%s", logs)
	}
	if !strings.Contains(logs, "明文凭证文件") || !strings.Contains(logs, "authcrypt") {
		t.Fatalf("加密开启时明文文件应提示 authcrypt:\n%s", logs)
	}
}

// TestLoadDirEncryptedWithoutKey 未配置密钥时：明文照常加载，密文跳过并 WARN（点名文件）。
func TestLoadDirEncryptedWithoutKey(t *testing.T) {
	resetCryptoState(t)
	t.Setenv(EnvMasterKey, "")
	key := testMasterKey()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-plain.json"), validPlainAuth("u-plain", "at1"), 0o600); err != nil {
		t.Fatal(err)
	}
	encPath := filepath.Join(dir, "workbuddy-enc.json")
	if err := os.WriteFile(encPath, encryptJSON(t, key, validPlainAuth("u-enc", "at2")), 0o600); err != nil {
		t.Fatal(err)
	}

	old := log.Writer()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	list, err := LoadDir(dir)
	log.SetOutput(old)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(list) != 1 || list[0].UID != "u-plain" {
		t.Fatalf("无密钥时应只加载明文账号: %+v", list)
	}
	logs := buf.String()
	if !strings.Contains(logs, encPath) || !strings.Contains(logs, EnvMasterKey) {
		t.Fatalf("应 WARN 缺失主密钥且点名文件:\n%s", logs)
	}
}

// TestEncryptDirDecryptDir 目录迁移：加密→备份/跳过/失败计数；解密→还原并保留 .enc.bak。
func TestEncryptDirDecryptDir(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	plain1 := validPlainAuth("u1", "at1")
	plain2 := validPlainAuth("u2", "at2")
	plain3 := validPlainAuth("u3", "at3")
	p1 := filepath.Join(dir, "workbuddy-u1.json")
	p2 := filepath.Join(dir, "workbuddy-u2.json")
	p3 := filepath.Join(dir, "workbuddy-u3.json")
	if err := os.WriteFile(p1, plain1, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, plain2, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p3, encryptJSON(t, key, plain3), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(dir, "workbuddy-bad.json")
	if err := os.WriteFile(badPath, []byte(`not-json`), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := EncryptDir(dir, false)
	if err != nil {
		t.Fatalf("EncryptDir: %v", err)
	}
	if s.Scanned != 4 || s.Encrypted != 2 || s.Skipped != 1 || s.Failed != 1 {
		t.Fatalf("EncryptDir summary=%+v want scanned=4 encrypted=2 skipped=1 failed=1", s)
	}
	for _, p := range []string{p1, p2} {
		raw, _ := os.ReadFile(p)
		if !IsEncryptedFile(raw) {
			t.Fatalf("%s 应已加密", p)
		}
		bak, err := os.ReadFile(p + ".plain.bak")
		if err != nil {
			t.Fatalf("%s 缺 .plain.bak: %v", p, err)
		}
		if !bytes.Equal(bak, plain1) && !bytes.Equal(bak, plain2) {
			t.Fatalf("%s .plain.bak 应是原明文内容", p)
		}
	}
	if raw, _ := os.ReadFile(p3); !IsEncryptedFile(raw) {
		t.Fatal("已加密文件应被跳过且保持加密")
	}
	if raw, _ := os.ReadFile(badPath); string(raw) != "not-json" {
		t.Fatal("无法解析的明文文件应保持不变")
	}

	// 解密回明文：3 个密文（2 个新加密 + 1 个原有）→ Decrypted=3；坏明文文件 Skip。
	s2, err := DecryptDir(dir, false)
	if err != nil {
		t.Fatalf("DecryptDir: %v", err)
	}
	if s2.Scanned != 4 || s2.Decrypted != 3 || s2.Skipped != 1 || s2.Failed != 0 {
		t.Fatalf("DecryptDir summary=%+v want scanned=4 decrypted=3 skipped=1 failed=0", s2)
	}
	for _, p := range []string{p1, p2, p3} {
		raw, _ := os.ReadFile(p)
		if IsEncryptedFile(raw) {
			t.Fatalf("%s 应已还原为明文", p)
		}
		if _, err := Parse(raw); err != nil {
			t.Fatalf("%s 还原后应可解析: %v", p, err)
		}
		if _, err := os.Stat(p + ".enc.bak"); err != nil {
			t.Fatalf("%s 缺 .enc.bak: %v", p, err)
		}
	}
	if raw, _ := os.ReadFile(p1); !bytes.Equal(raw, plain1) {
		t.Fatalf("解密后内容应逐字节等于原明文: %s", raw)
	}
}

// TestEncryptDirDryRun dry-run：只统计不写盘、不生成备份。
func TestEncryptDirDryRun(t *testing.T) {
	resetCryptoState(t)
	if err := SetMasterKey(testMasterKey()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-u1.json")
	plain := validPlainAuth("u1", "at1")
	if err := os.WriteFile(p, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := EncryptDir(dir, true)
	if err != nil {
		t.Fatalf("EncryptDir dry-run: %v", err)
	}
	if !s.DryRun || s.Scanned != 1 || s.Encrypted != 1 || s.Failed != 0 {
		t.Fatalf("dry-run summary=%+v", s)
	}
	if raw, _ := os.ReadFile(p); !bytes.Equal(raw, plain) {
		t.Fatal("dry-run 不应改动文件")
	}
	if _, err := os.Stat(p + ".plain.bak"); !os.IsNotExist(err) {
		t.Fatal("dry-run 不应生成备份")
	}
}

// TestEncryptDirMissingKey 未配置密钥：迁移工具返回明确错误（不产生半成品）。
func TestEncryptDirMissingKey(t *testing.T) {
	resetCryptoState(t)
	t.Setenv(EnvMasterKey, "")
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-u1.json")
	if err := os.WriteFile(p, validPlainAuth("u1", "at1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EncryptDir(dir, false); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("EncryptDir err=%v want ErrMasterKeyMissing", err)
	}
	if _, err := DecryptDir(dir, false); !errors.Is(err, ErrMasterKeyMissing) {
		t.Fatalf("DecryptDir err=%v want ErrMasterKeyMissing", err)
	}
}

// TestDecryptDirWrongKey 错误密钥：全部失败、文件保持密文、不生成 .enc.bak。
func TestDecryptDirWrongKey(t *testing.T) {
	resetCryptoState(t)
	key := testMasterKey()
	if err := SetMasterKey(key); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-u1.json")
	enc := encryptJSON(t, key, validPlainAuth("u1", "at1"))
	if err := os.WriteFile(p, enc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetMasterKey(bytes.Repeat([]byte{0x33}, 32)); err != nil {
		t.Fatal(err)
	}
	s, err := DecryptDir(dir, false)
	if err != nil {
		t.Fatalf("DecryptDir: %v", err)
	}
	if s.Decrypted != 0 || s.Failed != 1 {
		t.Fatalf("错误密钥 summary=%+v", s)
	}
	if raw, _ := os.ReadFile(p); !IsEncryptedFile(raw) {
		t.Fatal("失败时文件应保持密文")
	}
	if _, err := os.Stat(p + ".enc.bak"); !os.IsNotExist(err) {
		t.Fatal("失败时不应生成备份")
	}
}

// TestMasterKeyFingerprint 指纹：未配置为空；配置后稳定 16 hex；不同密钥不同。
func TestMasterKeyFingerprint(t *testing.T) {
	resetCryptoState(t)
	if got := MasterKeyFingerprint(); got != "" {
		t.Fatalf("未配置指纹=%q want 空", got)
	}
	if err := SetMasterKey(testMasterKey()); err != nil {
		t.Fatal(err)
	}
	fp1 := MasterKeyFingerprint()
	if len(fp1) != 16 {
		t.Fatalf("指纹长度=%d want 16: %q", len(fp1), fp1)
	}
	if again := MasterKeyFingerprint(); again != fp1 {
		t.Fatalf("指纹应稳定: %q vs %q", again, fp1)
	}
	if err := SetMasterKey(bytes.Repeat([]byte{0x99}, 32)); err != nil {
		t.Fatal(err)
	}
	if MasterKeyFingerprint() == fp1 {
		t.Fatal("不同密钥指纹应不同")
	}
}
