// Command authcrypt 批量加密/解密 auths 目录（v2.4.0 #7 凭证静态加密迁移工具）。
//
// 用法：
//
//	# 把 config.json 里 auth_dir 指向的明文凭证全部加密（原地替换，保留 *.plain.bak）
//	authcrypt -encrypt -config config.json
//
//	# 回迁为明文（停服后执行，保留 *.enc.bak）
//	authcrypt -decrypt -config config.json
//
//	# 演练：只统计、不写盘、不生成备份
//	authcrypt -encrypt -config config.json -dry-run
//
// 主密钥来自环境变量 WB2A_MASTER_KEY（32 字节 base64 或 64 位 hex，例如
// `openssl rand -base64 32`）。该密钥必须与网关启动时使用的一致；丢失后加密凭证
// 不可恢复——请单独备份、不要和 auths/ 放在一起。
//
// 退出码：0 = 全部成功（enabled/decrypt/skip 都算成功）；1 = 有文件失败/参数错误/
// 主密钥缺失或非法（失败原因见 WARN 日志）。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

func main() {
	var (
		configPath = flag.String("config", "config.json", "配置文件路径（用其 auth_dir 定位凭证目录）")
		dirFlag    = flag.String("dir", "", "覆盖 auths 目录（默认取配置文件里的 auth_dir；相对路径相对当前目录）")
		encrypt    = flag.Bool("encrypt", false, "把明文 auth 文件加密（已加密的跳过）")
		decrypt    = flag.Bool("decrypt", false, "把加密 auth 文件还原为明文（明文的跳过）")
		dryRun     = flag.Bool("dry-run", false, "只统计不写盘（不生成 .bak 备份）")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), `authcrypt - 凭证静态加密迁移工具（v2.4.0 #7）

用法:
  authcrypt -encrypt|-decrypt [-config config.json] [-dir auths目录] [-dry-run]

示例:
  export WB2A_MASTER_KEY="$(openssl rand -base64 32)"   # 首次生成并妥善备份
  authcrypt -encrypt -config config.json -dry-run        # 先演练看统计
  authcrypt -encrypt -config config.json                 # 真正加密（停服执行更稳）
  authcrypt -decrypt -config config.json                 # 回迁明文

说明:
  - 每个文件原地原子替换；成功转换的会保留备份 <name>.plain.bak（加密前明文）/
    <name>.enc.bak（解密前密文），确认无误后可自行删除。
  - 主密钥从环境变量 %s 读取（32 字节 base64 或 64 位 hex）。
  - 退出码: 0 成功；1 有文件失败/主密钥缺失/参数错误。
`, auth.EnvMasterKey)
	}
	flag.Parse()

	if *encrypt == *decrypt {
		fatalf("必须且只能指定 -encrypt 或 -decrypt 之一（见 -h）")
	}
	if extra := flag.Args(); len(extra) > 0 {
		fatalf("不支持的额外参数: %v（见 -h）", extra)
	}

	dir := strings.TrimSpace(*dirFlag)
	if dir == "" {
		d, err := authDirFromConfig(*configPath)
		if err != nil {
			fatalf("%v", err)
		}
		dir = d
	}

	key, err := auth.LoadMasterKeyFromEnv()
	if err != nil {
		fatalf("读取 %s 失败: %v", auth.EnvMasterKey, err)
	}
	if len(key) == 0 {
		fatalf("未设置 %s：请提供 32 字节主密钥（base64 或 64 位 hex，例如 `openssl rand -base64 32`）；"+
			"主密钥丢失后加密凭证不可恢复，请单独备份", auth.EnvMasterKey)
	}
	if err := auth.SetMasterKey(key); err != nil {
		fatalf("设置主密钥失败: %v", err)
	}

	mode := "encrypt"
	var summary auth.CryptSummary
	if *encrypt {
		summary, err = auth.EncryptDir(dir, *dryRun)
	} else {
		mode = "decrypt"
		summary, err = auth.DecryptDir(dir, *dryRun)
	}
	if err != nil {
		fatalf("%s 失败: %v", mode, err)
	}

	fmt.Printf("authcrypt: dir=%s mode=%s dry_run=%v scanned=%d encrypted=%d decrypted=%d skipped=%d failed=%d\n",
		dir, mode, *dryRun, summary.Scanned, summary.Encrypted, summary.Decrypted, summary.Skipped, summary.Failed)
	if *dryRun {
		fmt.Printf("（dry-run：未写盘、未生成备份；去掉 -dry-run 后执行真实迁移）\n")
	} else if summary.Encrypted+summary.Decrypted > 0 {
		fmt.Printf("备份已写入同目录：*.plain.bak（加密前明文）/*.enc.bak（解密前密文），确认无误后可自行删除\n")
	}
	if summary.Failed > 0 {
		fmt.Printf("存在 %d 个失败文件（详见上方 WARN 日志），请处理后重跑\n", summary.Failed)
		os.Exit(1)
	}
}

// authDirFromConfig 从配置文件读取 auth_dir（只做最小 JSON 解析，不依赖 cmd/server
// 的 Config：避免跨 package main 无法 import；也刻意不触发 normalize/校验）。
func authDirFromConfig(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取配置 %s 失败: %w（也可用 -dir 直接指定 auths 目录）", path, err)
	}
	var c struct {
		AuthDir string `json:"auth_dir"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("解析配置 %s 失败: %w（也可用 -dir 直接指定 auths 目录）", path, err)
	}
	if strings.TrimSpace(c.AuthDir) == "" {
		return "./auths", nil
	}
	return c.AuthDir, nil
}

// fatalf 打印错误并以退出码 1 结束（迁移工具有失败必须能被脚本感知）。
func fatalf(format string, args ...any) {
	log.Printf("authcrypt: "+format, args...)
	os.Exit(1)
}
