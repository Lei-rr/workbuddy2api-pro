// 持久化：本地 state.json 落盘/加载、滚动备份与损坏回退（v2.2 #13）、
// Redis 快照镜像（StoreSnapshotter）、后台 flusher、择新恢复（RestoreFromSnapshot）。
//
// v2.6.0（Audit-1 P0-3）：落盘改为「锁内快照 + 锁外 marshal/写盘」。p.mu 只保护
// stateOverviewLocked 的快照拷贝（O(n) 内存操作），磁盘 IO 在 persistMu 下串行执行，
// 不再阻塞 Pick/Acquire 等 chat 热路径。快照带池版本号（p.version），persistState
// 跳过比已写盘版本更旧的快照，保证并发快照乱序时不会用旧内容覆盖新内容。
package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

var flushInterval = 5 * time.Second

// persistWriteHook 仅测试注入：persistState 在写盘前调用（用于验证「写盘期间不持
// p.mu」的锁外 IO 契约）。生产恒 nil。
var persistWriteHook func()

// backupSuffix state.json 滚动备份的后缀（保留最近 1 份；v2.2 #13）。
// 每次主文件**写成功后**把上一版内容复制到这里；主文件损坏时 load 自动回退。
const backupSuffix = ".bak"

// persistLogEvery 连续落盘失败每 N 次打一条提醒（flusher 5s 一把 ≈ 1 分钟一次），
// 避免磁盘持续满/权限丢失时日志刷屏。
const persistLogEvery = 12

// snapshot 池状态快照（Redis 镜像用）。与本地 state.json 同源（stateFile），
// 额外带 savedAt 时间戳供"择新恢复"（比较本地与 Redis 快照的新旧）。
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// StoreSnapshotter 池状态快照镜像的最小接口（redisstore.Store 满足；Noop 空实现安全）。
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot 择新恢复：比较本地 state.json 与 Redis 快照，采用较新者。
//
// 本地**可用**时按新旧择一（快照不早于本地 → 采用快照，否则本地优先）；本地**不可用**
// （state.json 缺失或不可读，典型为首次在新卷/新节点启动）时**采用快照**——此时本地根本
// 没有可"优先"的状态，快照是本轮唯一的运行态来源，这正是快照作为「启动恢复备份」的核心
// 场景。分支情形：无快照 / 快照无 savedAt → 本地优先（无判据可比）。
//
// 每种情形都打一条对应的恢复来源日志，便于对账。必须在 SyncToDir 之前调用
// （SyncToDir 只增删不入值：值只能来自本地 load 或本函数采用快照）。
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: 恢复来源=本地 state.json（无 Redis 快照）")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// 快照无 savedAt：无法比较新旧，本地优先。
		log.Printf("pool: 恢复来源=本地 state.json（Redis 快照无 saved_at）")
		return
	}
	if localErr != nil {
		// 本地不可用 → 采用快照（本地没有可"优先"的状态）。
		//
		// 旧实现把该情形与「本地较新」合并成同一个 fall-through：既不改内存、不置 dirty
		//（有效快照被静默丢弃），又打出"本地 state.json（较新于 Redis 快照 …）"——一次
		// 从未发生过的比较，把排障引向根本不存在的本地文件；随后 SyncToDir 只增删不入值，
		// 全池运行态（credits/冷却/熔断计数/usedSeq/lastUsed）被清零。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)（本地 state.json 不可用: %v）",
			snap.SavedAt.Format(time.RFC3339), localErr)
		return
	}
	if !localInfo.ModTime().After(snap.SavedAt) {
		// 快照不早于本地 → 采用快照。
		p.adoptSnapshot(snap)
		log.Printf("pool: 恢复来源=Redis 快照 (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	// 走到这里必然是「本地存在且严格新于快照」，日志结论属实。
	log.Printf("pool: 恢复来源=本地 state.json（较新于 Redis 快照 %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire 为账号占一个在途名额；false 表示该账号已达上限（或不存在）。
// 必须在成功 Pick 后调用；调用方负责 defer Release。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// 在启动 goroutine 前同步写入（避免与测试对 flushInterval 的恢复写竞争）；
	// stopCh 同步登记，Close 才能可靠停止（New 与 startFlusher 之间无并发窗口）。
	p.stopCh = stopCh
	p.flusherDone = make(chan struct{})
	interval := flushInterval
	go func() {
		defer close(p.flusherDone)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				// v2.6.0：只在锁内做快照，marshal/写盘在锁外（persistState，persistMu 串行）。
				p.mu.Lock()
				sf, ver, doSave := stateFile{}, uint64(0), false
				if p.dirty.Swap(false) {
					sf, ver, doSave = p.snapshotForPersistLocked()
				}
				p.mu.Unlock()
				if doSave {
					p.persistState(sf, ver)
				}
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush 同步把内存状态落盘（幂等：无变更不写盘）。供进程退出前调用。
// v2.6.0：锁内快照、锁外写盘（与 flusher 共用 persistState）。
func (p *Pool) Flush() {
	p.mu.Lock()
	sf, ver, doSave := stateFile{}, uint64(0), false
	if p.dirty.Swap(false) {
		sf, ver, doSave = p.snapshotForPersistLocked()
	}
	p.mu.Unlock()
	if doSave {
		p.persistState(sf, ver)
	}
}

// backupPath 返回 state.json 的滚动备份路径（保留最近 1 份）。
func (p *Pool) backupPath() string { return p.stateFp + backupSuffix }

// load 从本地 state.json 恢复池状态（New 构造期调用，此时无并发，不持 p.mu）。
//
// v2.2 #13 可靠性：
//   - 首次运行（文件不存在）静默返回；
//   - 主文件不可读 / JSON 损坏 → 打 ERROR/WARN，**隔离坏文件**（.corrupt-<ts>，
//     保留人工抢救价值，避免下一次 flush 用空状态静默覆盖证据），再尝试 .bak；
//   - .bak 有效 → 恢复并立即回写主文件；.bak 也无效 → 空池启动（与引入前一致，
//     不 fatal，进程仍可用）。
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		if os.IsNotExist(err) {
			return // 首次运行：无 state.json 属正常
		}
		log.Printf("WARN: pool: 读取 state.json 失败（尝试 .bak 恢复）: %v", err)
		p.loadBackup("主文件不可读")
		return
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("ERROR: pool: state.json 解析失败（文件损坏）: %v（尝试 .bak 恢复）", err)
		p.quarantineCorrupt()
		p.loadBackup("主文件损坏")
		return
	}
	p.applyAccountsLocked(sf.Accounts)
	p.restoreSchedulingLocked(sf.Scheduling)
	p.bumpVersionLocked()
	p.bumpMembershipLocked()
}

// quarantineCorrupt 把损坏的 state.json 改名隔离为 state.json.corrupt-<UnixNano>。
// 改名失败只 WARN（退化为旧行为：主文件随后可能被下一次 flush 覆盖）——隔离是
// 尽力而为的抢救措施，不能反过来阻断启动。
func (p *Pool) quarantineCorrupt() {
	bad := fmt.Sprintf("%s.corrupt-%d", p.stateFp, time.Now().UnixNano())
	if err := os.Rename(p.stateFp, bad); err != nil {
		log.Printf("WARN: pool: 隔离损坏的 state.json 失败: %v（下次落盘会覆盖它，建议尽快手工备份）", err)
		return
	}
	log.Printf("WARN: pool: 损坏的 state.json 已隔离为 %s（保留抢救价值；可人工修复后改名回主文件）", filepath.Base(bad))
}

// loadBackup 主文件不可用时尝试 state.json.bak 恢复。
// 恢复成功后置 dirty 并立即回写主文件（下一次重启不再依赖损坏的主文件；损坏文件
// 已隔离时主文件不存在 → 回写不会滚动覆盖 .bak）；.bak 缺失/损坏 → 空池
// 启动（保持引入前「损坏即空池」的非 fatal 语义，但已打 WARN + 隔离证据）。
func (p *Pool) loadBackup(reason string) {
	bakRaw, err := os.ReadFile(p.backupPath())
	if err != nil {
		log.Printf("WARN: pool: %s，且无可用 state.json.bak（%v）：以空池启动（数据目录请人工检查）", reason, err)
		return
	}
	var sf stateFile
	if err := json.Unmarshal(bakRaw, &sf); err != nil {
		log.Printf("WARN: pool: %s，且 state.json.bak 同样不可解析（%v）：以空池启动", reason, err)
		return
	}
	p.applyAccountsLocked(sf.Accounts)
	p.restoreSchedulingLocked(sf.Scheduling)
	p.bumpVersionLocked()
	p.bumpMembershipLocked()
	p.dirty.Store(true)
	log.Printf("WARN: pool: 已从 state.json.bak 恢复 %d 个账号（%s）；主文件将按恢复后状态重写",
		len(sf.Accounts), reason)
	// 立即回写主文件。构造期（New）调用，flusher 尚未启动，无并发；v2.6.0 起
	// 仍是「锁内快照 → 锁外写盘」同一路径（构造期无需 p.mu，但 snapshotForPersistLocked
	// 只读内存状态，安全）。
	snapSF, snapVer, ok := p.snapshotForPersistLocked()
	if ok {
		p.persistState(snapSF, snapVer)
	}
}

// applyAccountsLocked 用持久化账号状态覆盖/插入 byUID（placeholder 凭证，Add 时换全）。
// 本地 load() 与 Redis 快照恢复共用；调用方必须已持有 p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total 优先；旧文件的 err_count（连续错误）作一次性迁移源映射进来（二者取较大者，
		// 尽最大可能保留历史观测信号——旧语义下 err_count 也真实发生过错误，不应丢）。
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a:                        &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:                  s.Credits,
			creditsTotal:             s.CreditsTotal,
			creditsExpiring:          s.CreditsExpiring,
			creditsEarliestExpiry:    s.CreditsEarliestExpiry,
			creditsEarliestRemaining: s.CreditsEarliestRemaining,
			disabled:                 s.Disabled,
			paused:                   s.Paused,
			reason:                   s.Reason,
			until:                    s.Until,
			coolKind:                 s.CoolKind,
			successCount:             s.SuccessCount,
			errTotal:                 errTotal,
			lastErr:                  s.LastErr,
			lastSuccess:              s.LastSuccess,
			lastCheckinDay:           s.LastCheckinDay,
			tokenUsage:               s.TokenUsage,
			softStreak:               s.SoftStreak,
			sessionDeadFails:         s.SessionDeadFails,
			consecutiveFails:         s.ConsecutiveFails,
		}
		// 到期快照按当前时刻惰性清洗：已过期、零剩余或超出总余额的脏数据不恢复。
		if e.creditsExpiring < 0 {
			e.creditsExpiring = 0
		}
		if e.creditsExpiring > e.credits {
			e.creditsExpiring = e.credits
		}
		if e.creditsEarliestRemaining < 0 || e.creditsEarliestRemaining > e.credits {
			e.creditsEarliestRemaining = 0
		}
		if e.creditsEarliestRemaining == 0 || e.creditsEarliestExpiry.IsZero() || !now.Before(e.creditsEarliestExpiry) {
			e.creditsEarliestExpiry = time.Time{}
			e.creditsEarliestRemaining = 0
		}
		// 按日到期批次：惰性剔除已过期/零剩余条目；有批次时以批次为准同步最早批次，
		// 保证重启后首页快照与「检查」仍同口径。
		e.expiryBuckets = sanitizeExpiryBuckets(s.CreditsExpiryBuckets, now)
		if len(e.expiryBuckets) > 0 {
			e.creditsEarliestExpiry = e.expiryBuckets[0].ExpireAt
			e.creditsEarliestRemaining = e.expiryBuckets[0].Remain
		}
		// 熔断器持久化恢复：breakerUntil 未过期才恢复（过期不复活），retryCount 仅在
		// 熔断仍有效时保留（否则归零，不保留无用退避指数）。
		if s.BreakerUntil != nil && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// 连败降权：未过期才恢复（过期/零值不写不复活）。
		if s.DegradeUntil != nil && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// 模型级独立冷却（6004 重置墙钟 / 11102 负缓存）：惰性过滤已过期条目。
		if len(s.ModelCooldowns) > 0 {
			for m, mc := range s.ModelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if e.modelCooldowns == nil {
					e.modelCooldowns = map[string]modelCooldown{}
				}
				e.modelCooldowns[m] = modelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// 成本账本：惰性过滤过期（modelCostTTL 外不恢复）+ 剔除结构破损条目
		// （负 per1k / 零 LastSeen——上游异常或旧文件手改产生的脏数据）。
		if len(s.ModelCosts) > 0 {
			for m, mc := range s.ModelCosts {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL || mc.CostPer1k < 0 {
					continue
				}
				if e.modelCost == nil {
					e.modelCost = map[string]modelCostEntry{}
				}
				e.modelCost[m] = modelCostEntry{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		p.byUID[uid] = e
		if n := len(e.modelCooldowns); n > 0 {
			p.addModelLockEntriesLocked(n) // 模型锁条目计数随 state 恢复
		}
	}
}

// applySnapshotLocked 用 Redis 快照覆盖内存状态（已在择新判定后采用）。调用方必须已持有 p.mu。
// adoptSnapshot 采用 Redis 快照为当前池状态，并置 dirty 让下一次落盘把它物化回本地
// state.json（否则快照只在内存生效，下次崩溃恢复又回到旧本地文件）。
func (p *Pool) adoptSnapshot(s snapshot) {
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	p.dirty.Store(true)
}
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.modelLockEntries.Store(0) // 旧条目计数随状态整体替换归零，由 applyAccountsLocked 重建
	p.applyAccountsLocked(s.Accounts)
	p.restoreSchedulingLocked(s.Scheduling)
	p.bumpVersionLocked()
	p.bumpMembershipLocked()
}

// snapshotForPersistLocked 锁内构造落盘快照（O(n) 内存拷贝，不含 IO）。调用方必须
// 持有 p.mu；返回 stateFile、池版本号（用于 persistState 的乱序保护）与是否可落盘。
// v2.6.0：磁盘 IO 不再在锁内做（P0-3），本函数只做快照。
func (p *Pool) snapshotForPersistLocked() (stateFile, uint64, bool) {
	if p.stateFp == "" {
		return stateFile{}, 0, false
	}
	return p.stateOverviewLocked(), p.version.Load(), true
}

// persistState 锁外序列化 + 原子写盘（v2.6.0 P0-3）。
//
// 并发契约：
//   - persistMu 串行化所有写盘者（flusher / Flush / Remove / SyncToDir / 构造期回写），
//     目录/临时文件/rename 不会互相竞争；
//   - snapVer 是快照生成时的池版本号（p.version）。若已有**更新版本**的快照写盘，
//     本次旧快照直接跳过——避免两个 goroutine 先快照后写盘的乱序用旧内容覆盖新内容；
//   - p.mu 在本函数内完全不持有：慢盘/大 state（10k+ 账号数 MB）不再阻塞 Pick/chat；
//   - 原子写/.bak 滚动/失败不覆盖语义与 v2.5.0 完全一致（先写 tmp、rename 成功后才
//     滚动备份；失败只记节流日志，不标记成功）。
func (p *Pool) persistState(sf stateFile, snapVer uint64) {
	if p.stateFp == "" {
		return
	}
	p.persistMu.Lock()
	defer p.persistMu.Unlock()
	if last := p.persistedVer.Load(); last > snapVer {
		return // 已写过更新版本的快照：旧快照不覆盖新内容
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if persistWriteHook != nil {
		persistWriteHook()
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// 先读上一版内容（用于写成功后的滚动备份）。读失败 = 首次写盘或文件不可读，
	// 此时没有可滚动的旧版本——备份缺失不阻断主写入。
	var prev []byte
	if b, rerr := os.ReadFile(p.stateFp); rerr == nil {
		prev = b
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	// 主文件已成功落盘 → 才把上一版滚动为 .bak。写失败路径在上面直接返回，
	// 绝不覆盖上一份可用备份（「备份只在写成功后更新」）。
	p.rotateBackup(prev)
	p.persistedVer.Store(snapVer)
	p.persistFailMu.Lock()
	if p.persistFails > 0 {
		// 从连续失败中恢复：打一条恢复日志，避免"错误打完却无人知道已恢复"。
		log.Printf("pool: state.json 落盘恢复（此前连续失败 %d 次）", p.persistFails)
		p.persistFails = 0
	}
	p.persistFailMu.Unlock()
	// 同步镜像一份快照到 Redis（fire-and-forget），与本地 state.json 并存作恢复备份。
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// rotateBackup 把上一版 state.json 内容滚动写入 state.json.bak（保留最近 1 份）。
// 只在主文件写成功后调用；备份本身用 tmp+rename 原子替换，写/改名失败只 WARN
// （主文件已成功，备份是附加保险，不能反过来把主状态标记为落盘失败）。
// 调用方必须持有 persistMu（persistState 内调用）；不要求 p.mu。
func (p *Pool) rotateBackup(prev []byte) {
	if len(prev) == 0 {
		return // 首次写盘 / 旧文件为空：没有可滚动的上一版
	}
	bak := p.backupPath()
	tmp := bak + ".tmp"
	if err := os.WriteFile(tmp, prev, 0o600); err != nil {
		log.Printf("WARN: pool: 写 state.json.bak 失败（主文件已成功落盘）: %v", err)
		return
	}
	if err := os.Rename(tmp, bak); err != nil {
		log.Printf("WARN: pool: 替换 state.json.bak 失败（主文件已成功落盘）: %v", err)
		_ = os.Remove(tmp)
	}
}

// notePersistFail 记录一次本地 state.json 落盘失败，并按节流规则决定是否打日志：
// 首败（状态成功→失败）打完整错误、每 persistLogEvery 次连续失败打一条提醒、
// 其余连续失败静默（flusher 5s 一把，磁盘持续满时不刷屏）。
// 恢复成功的日志由 persistState 在成功路径统一打。与 redisstore 三处异步写的
// "失败仅打日志、不向上抛"范式对齐，但落盘失败对运维是盲区，故多一层节流（notification）。
//
// v2.6.0：调用方可能是锁外 persistState，persistFails 用独立 persistFailMu 保护
// （不再依赖 p.mu）。
func (p *Pool) notePersistFail(err error) {
	p.persistFailMu.Lock()
	defer p.persistFailMu.Unlock()
	if p.persistFails == 0 {
		log.Printf("pool: state.json 落盘失败: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json 连续落盘失败 %d 次: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked 收集当前内存状态为 stateFile（供落盘 + 快照镜像复用）。
// 调用方必须已持 p.mu。返回值与池内存完全解耦（scheduling map 深拷贝），可在锁外
// marshal/写盘（v2.6.0：快照与 IO 分离，防锁外序列化与并发写竞争）。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	// Scheduling 深拷贝：stateOverview 的返回值在锁外 marshal；若直接共享 p.scheduling，
	// 并发 restoreSchedulingLocked/pick 写 scheduling 会构成数据竞争。
	var scheduling map[string]scheduleCursor
	if len(p.scheduling) > 0 {
		scheduling = make(map[string]scheduleCursor, len(p.scheduling))
		for k, v := range p.scheduling {
			scheduling[k] = v
		}
	}
	sf := stateFile{Accounts: map[string]stateAccount{}, Scheduling: scheduling}
	for uid, e := range p.byUID {
		s := stateAccount{
			Credits:                  e.credits,
			CreditsTotal:             e.creditsTotal,
			Disabled:                 e.disabled,
			Paused:                   e.paused,
			Reason:                   e.reason,
			Until:                    e.until,
			CoolKind:                 e.coolKind,
			SuccessCount:             e.successCount,
			ErrTotal:                 e.errTotal,
			LastSuccess:              e.lastSuccess,
			LastErr:                  e.lastErr,
			LastCheckinDay:           e.lastCheckinDay,
			TokenUsage:               e.tokenUsage,
			SoftStreak:               e.softStreak,
			SessionDeadFails:         e.sessionDeadFails,
			ConsecutiveFails:         e.consecutiveFails,
			CreditsExpiring:          e.creditsExpiring,
			CreditsEarliestExpiry:    e.creditsEarliestExpiry,
			CreditsEarliestRemaining: e.creditsEarliestRemaining,
			CreditsExpiryBuckets:     append([]ExpiryBucket(nil), e.expiryBuckets...),
		}
		// 熔断截止：仅未过期才落盘（指针 nil 才能被 omitempty 真省略）。
		if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			u := e.breakerUntil
			s.BreakerUntil = &u
			s.RetryCount = e.retryCount
		}
		// 连败降权截止：仅未过期才落盘。
		if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			u := e.degradeUntil
			s.DegradeUntil = &u
		}
		// 模型级独立冷却：惰性过滤已过期条目（Hits 不落盘，重启后 11102 退避从基数重学）。
		if len(e.modelCooldowns) > 0 {
			for m, mc := range e.modelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if s.ModelCooldowns == nil {
					s.ModelCooldowns = map[string]stateModelCooldown{}
				}
				s.ModelCooldowns[m] = stateModelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// 成本账本：惰性过滤过期观测（modelCostTTL 外不写——陈旧价格不复活）。
		if len(e.modelCost) > 0 {
			for m, mc := range e.modelCost {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
					continue
				}
				if s.ModelCosts == nil {
					s.ModelCosts = map[string]stateModelCost{}
				}
				s.ModelCosts[m] = stateModelCost{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		sf.Accounts[uid] = s
	}
	return sf
}
