// config_backup.go 配置变更安全（v2.2.0 #15）：保存前自动备份、一键回滚、危险值拦截。
//
// 背景：面板「配置」页保存走 main.go 的 saveConfig（校验 → 深合并 → 原子替换 →
// 热应用）。此前没有「上一版」备份，一次错误改动只能手工回忆恢复；listen 从回环改成
// 全网卡（公网暴露面扩大）也没有留痕。本文件提供可单测的纯逻辑：
//   - backupConfigFile / writeConfigAtomic：写盘前备份当前版本、原子替换；
//   - rollbackConfig：读取并校验 config.json.bak → 整份写回 → 热应用；
//   - verifyDangerousConfigValues / listenChangeAuditNote：危险值拦截与审计；
//   - applyConfigRuntime：保存与回滚共用的热生效逻辑。
//
// 为什么独立成文件而不是写进 config.go：Wave 1 期间 cmd/server/config.go 同时被
// 优雅停机任务（server.shutdown_timeout）修改，独立文件避免两边 read-modify-write
// 互相覆盖；同属 package main，对 main.go 的接线完全等价。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"syscall"

	"github.com/lei-rr/workbuddy2api-pro/internal/livecfg"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/scheduler"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// configBackupSuffix 配置自动备份后缀：与 config.json 同目录的 config.json.bak，
// 只保留最近 1 份（每次即将落盘的新配置通过校验后，把「当前生效版本」覆盖进去）。
const configBackupSuffix = ".bak"

// errNoConfigBackup 表示还没有可回滚的备份（首次保存前 / 备份被手工删除）。
// HTTP 层据此返回 400（用户可操作）而不是 500。
var errNoConfigBackup = errors.New("没有可回滚的配置备份（config.json.bak 不存在）")

// configBackupPath 返回配置文件的备份路径（同目录 <path>.bak）。
func configBackupPath(path string) string { return path + configBackupSuffix }

// backupConfigFile 把当前配置文件内容原子复制为 <path>.bak（0600，覆盖旧备份）。
//
// 调用时机（saveConfig 接线顺序）：新配置已通过全部校验、即将写盘前调用——因此
// 校验失败不会动 .bak；若随后的写盘失败，.bak 里仍是「当前仍生效的版本」，恰好
// 可作为回滚目标。当前配置不存在（首次生成场景）返回 ("", nil)：没有旧版本可备份。
func backupConfigFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read config for backup: %w", err)
	}
	bak := configBackupPath(path)
	tmp := bak + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", fmt.Errorf("write config backup: %w", err)
	}
	// rename 覆盖旧备份（Windows/Linux 的 os.Rename 均允许替换已存在文件）；
	// 失败时退回直接覆盖写——.bak 只是回滚兜底，一致性要求低于主配置。
	if err := os.Rename(tmp, bak); err != nil {
		if werr := os.WriteFile(bak, data, 0o600); werr != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("write config backup: %w", werr)
		}
		_ = os.Remove(tmp)
	}
	return bak, nil
}

// writeConfigAtomic 把配置内容原子写回 path：先写同目录 <path>.tmp（0600）再 rename。
//
// 单文件 Docker bind mount 无法被 rename 覆盖（Linux 返回 EBUSY / "device or resource
// busy"）：此时回退为就地截断写入；写失败时保留 tmp（内容完整，可手工恢复）。
// 该函数由 saveConfig 与 rollbackConfig 共用，保证两条路径的落盘语义一致。
func writeConfigAtomic(path string, out []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，可手工恢复）。
		if writeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}
	return nil
}

// applyConfigRuntime 把新配置里「可热生效」的字段全部应用到运行期组件，返回仍需
// 重启才生效的字段清单（面板提示用）。saveConfig 与 rollbackConfig 共用同一套逻辑，
// 保证「保存」与「回滚」的热生效范围永远不会漂移。
//
// 热生效范围（与 saveConfig 的调用方约定一致）：
//   - api_key / cooldown.soft_rate / 指纹脱敏 / 来源记录 / 模型黑名单 / 面板轮询间隔 → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetDegrade/SetSoftRateMax/SetWeights/...
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow/...
//
// 装配期依赖（listen/auth_dir/upstream 超时等）不在本函数内，列入 restartRequiredFields。
func applyConfigRuntime(newCfg *Config, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) []string {
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(newCfg.Models.Hidden),
		PollIntervalSec:      newCfg.Panel.PollIntervalSec,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	p.SetCrossDomainFallback(newCfg.Pool.CrossDomainFallback) // 裸名跨域回退热生效（issue #135）
	_ = p.SetScheduling(newCfg.Pool.Strategy, newCfg.Pool.AccountOrder)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetTaskSweepInterval(newCfg.TaskSweepInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	// v2.0.1 #10/#11：余额刷新并发上限与补跑窗口均支持热生效（下一轮读新值）。
	sch.SetBalanceRefreshConcurrency(newCfg.Schedule.BalanceRefreshConcurrency)
	sch.SetCatchUpMinutes(newCfg.Schedule.CatchUpMinutes)
	// v2.3.1 #11：签到 worker pool 账号级并发度热生效（下一轮签到读取新值；
	// 在途 worker 不打断）。Jobs/Notify 装配期接线，不在热更新范围。
	sch.SetCheckinConcurrency(newCfg.Schedule.CheckinConcurrency)
	return restartRequiredFields(newCfg)
}

// rollbackConfig 用上一版备份（<path>.bak）恢复配置（v2.2.0 #15 一键回滚）。
//
// 流程：读 .bak → 与启动同一套 Default+normalize 校验（JSON 非法 / 字段越界即拒绝，
// 不碰当前配置）→ 危险值 + 启动安全校验兜底 → 整份原子写回 config.json（不与当前
// 配置合并，回滚语义即「回到上一版」）→ 热应用 → 返回需重启字段清单。
//
// 错误契约：无备份返回 errNoConfigBackup（HTTP 层 400）；其余错误均以"（未回滚）"
// 如实描述，HTTP 层统一按 400 透出给用户。
func rollbackConfig(path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	raw, err := os.ReadFile(configBackupPath(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errNoConfigBackup
		}
		return nil, fmt.Errorf("read config backup: %w", err)
	}
	newCfg, err := ParseConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("备份配置校验失败（未回滚）: %w", err)
	}
	// 备份可能是手工改过的旧文件：保存路径同一套危险值拦截 + 启动安全校验兜底，
	// 避免「回滚到一个会被启动校验拒绝的配置」把进程重启变成起不来。
	if err := verifyDangerousConfigValues(newCfg); err != nil {
		return nil, fmt.Errorf("备份配置未通过危险值校验（未回滚）: %w", err)
	}
	if err := validateListenSecurity(newCfg); err != nil {
		return nil, fmt.Errorf("备份配置未通过启动安全校验（未回滚）: %w", err)
	}
	curRaw, _ := os.ReadFile(path) // current config before rollback (may be absent; diff only)
	if err := writeConfigAtomic(path, raw); err != nil {
		return nil, fmt.Errorf("回滚写盘失败: %w", err)
	}
	log.Printf("panel: 配置已从备份回滚 %s → %s", configBackupPath(path), path)
	restart := applyConfigRuntime(newCfg, live, p, up, sch)
	return append(restart, startupOnlyChanges(curRaw, newCfg)...), nil
}

// startupOnlyChanges compares the pre-save/pre-rollback config.json raw with the next config and
// returns fields that are hot-editable in the config model but only read at assembly time, so the
// transition needs a restart (v2.6.0 audit P1; fills the gap where restartRequiredFields cannot
// know the old value):
//   - upstash: connection/Noop mode is created at startup; set/modify/clear all need restart;
//   - schedule.balance_refresh_enabled / task_sweep_enabled: disabled at startup means no ticker
//     goroutine; enabling at runtime needs a restart (disable / interval changes are hot).
func startupOnlyChanges(oldRaw []byte, next *Config) []string {
	if len(oldRaw) == 0 || next == nil {
		return nil
	}
	var old map[string]any
	if err := json.Unmarshal(oldRaw, &old); err != nil {
		return nil
	}
	var out []string
	var oldUpstash map[string]any
	if m, ok := old["upstash"].(map[string]any); ok {
		oldUpstash = m
	}
	oldURL, _ := oldUpstash["url"].(string)
	oldToken, _ := oldUpstash["token"].(string)
	if oldURL != next.Upstash.URL || oldToken != next.Upstash.Token {
		out = append(out, "upstash")
	}
	var oldSched map[string]any
	if m, ok := old["schedule"].(map[string]any); ok {
		oldSched = m
	}
	// Same defaults as Config: balance_refresh_enabled / task_sweep_enabled default true.
	if !boolField(oldSched, "balance_refresh_enabled", true) && next.Schedule.BalanceRefreshEnabled {
		out = append(out, "schedule.balance_refresh_enabled")
	}
	if !boolField(oldSched, "task_sweep_enabled", true) && next.Schedule.TaskSweepEnabled {
		out = append(out, "schedule.task_sweep_enabled")
	}
	return out
}

// boolField reads a bool from a JSON map; missing/mistyped key returns def.
func boolField(m map[string]any, key string, def bool) bool {
	if m == nil {
		return def
	}
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// verifyDangerousConfigValues 危险值拦截（v2.2.0 #15-3）：保存/回滚都拒绝会导致进程
// 重启后起不来或数据落盘失效的配置。当前覆盖：
//   - auth_dir 为空：重启后加载不到任何账号（凭证目录丢失）；
//   - state_file 为空：账号池/用量状态无处落盘，重启即丢数据。
//
// 不拦目录可写性：跨平台/容器 uid 语义复杂（bind mount 属主不同很常见），硬拦截的
// 误杀成本高于收益——由启动自检（#13）打 WARN 并给出 chown 提示。api_key 清空另有
// rejectClearingAPIKey（需结合旧值判断是否"清空"，不在此处重复）。
func verifyDangerousConfigValues(newCfg *Config) error {
	if strings.TrimSpace(newCfg.AuthDir) == "" {
		return fmt.Errorf("auth_dir 不能保存为空：凭证目录为空会导致重启后加载不到任何账号（当前值来自 config.json / 默认 ./auths）")
	}
	if strings.TrimSpace(newCfg.StateFile) == "" {
		return fmt.Errorf("state_file 不能保存为空：状态文件为空会导致账号池/用量数据无法持久化（当前值来自 config.json / 默认 ./data/state.json）")
	}
	return nil
}

// listenChangeAuditNote 检测本次保存是否改变了 listen 的「监听面」（回环 ↔ 非回环），
// 返回审计日志文案（空串 = 无需审计）。#15 要求：允许改但留痕，便于事后追溯
// 「面板/网关从本机暴露到公网」这类操作。
//
// oldRaw 是保存前的 config.json 原文，而不是已解析的 map：saveConfig 的
// mergeConfigMaps(cur, incoming) 会原地改写 cur，调用本函数时 cur 里的 listen 已
// 是提交后的新值（拿 map 判新旧会永远判成"没变化"，属于真实踩过的坑）。
// 老配置未写 listen / 原文不可解析时按默认 :7863（全网卡）判。
func listenChangeAuditNote(oldRaw []byte, newListen string) string {
	oldListen := ""
	if len(oldRaw) > 0 {
		var cur map[string]any
		if json.Unmarshal(oldRaw, &cur) == nil {
			oldListen, _ = cur["listen"].(string)
		}
	}
	if strings.TrimSpace(oldListen) == "" {
		oldListen = Default().Listen
	}
	if isLoopbackListen(oldListen) == isLoopbackListen(newListen) {
		return ""
	}
	direction := "回环 → 非回环（暴露面扩大）"
	if isLoopbackListen(newListen) {
		direction = "非回环 → 回环（暴露面收窄）"
	}
	return fmt.Sprintf("listen 监听面变化: %s → %s（%s，注意防火墙/反代与鉴权配置）", oldListen, newListen, direction)
}
