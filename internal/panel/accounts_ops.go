// accounts_ops.go 单账号运维 + 批量任务触发 + 昵称同步（v2.6.1 拆分自 panel.go，纯移动）。
// 单号：revive/disable/pause/resume/rescan/checkin/balance/remove；批量：5 个 *_all。
package panel

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/jobs"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// expiringSoonWindow 返回调度器当前生效的快过期路由窗口；测试面板无调度器时返回 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// poolExpiryBuckets 把 upstream 的按日到期批次转换为 pool 结构。两个包字段同构，
// 手工转换是为了不让底层 pool 反向依赖 upstream。
func poolExpiryBuckets(in []upstream.ExpiryBucket) []pool.ExpiryBucket {
	if len(in) == 0 {
		return nil
	}
	out := make([]pool.ExpiryBucket, len(in))
	for i, b := range in {
		out[i] = pool.ExpiryBucket{Date: b.Date, ExpireAt: b.ExpireAt, Remain: b.Remain}
	}
	return out
}

// ---------------------------------------------------------------------------
// 账号运维
// ---------------------------------------------------------------------------

// accountRevive 手动复活：清禁用 + 冷却 + 熔断（运维口径无条件恢复）。
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（人工清除禁用/冷却/熔断）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable 人工禁用（不再参与选号，需面板 revive 或重登恢复）。
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	// v2.3.0 #19：账号被禁用 → Emit account_disabled（Notifier 关闭时 no-op；uid 脱敏）。
	if p.cfg.Notify != nil {
		p.cfg.Notify.AccountDisabled(shortUID(uid), "manual disable (panel)")
	}
	log.Printf("panel: disable uid=%s（人工禁用）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountPause 暂停选号：账号退出选号候选，但**照常参与**签到 / 活跃上报 / 保活 /
// 余额刷新。与 disable 的区别：不写 reason、不清冷却域、不重置计数——账号是「临时
// 让位」而非「判死」，点「恢复选号」即可立刻回到池子（无需重登或解冻）。
func (p *Panel) accountPause(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Pause(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: pause uid=%s（暂停选号，保号任务照常）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountResume 解除暂停选号（幂等，对未暂停账号为空操作）。
func (p *Panel) accountResume(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !p.cfg.Pool.Resume(uid) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	log.Printf("panel: resume uid=%s（恢复参与选号）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountsRescan 按 auths/ 目录重新对齐账号池：外部脚本批量导入/删除凭证文件后
// 免重启生效。只增删账号，已存在账号的积分/冷却/计数等状态保留（pool.SyncToDir 语义）。
func (p *Panel) accountsRescan(w http.ResponseWriter, r *http.Request) {
	if p.cfg.AuthDir == "" {
		writeErr(w, http.StatusNotImplemented, "auth_dir not configured")
		return
	}
	before := make(map[string]bool, 1024)
	for _, s := range p.cfg.Pool.List() {
		before[s.UID] = true
	}
	auths, err := auth.LoadDir(p.cfg.AuthDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scan auth dir: "+err.Error())
		return
	}
	// 扫到 0 个文件而池非空 = 目录挂载丢失或路径配错。此时 SyncToDir 会清空整池
	// 并把结果落盘（不可逆），必须拒绝，交给人去确认挂载。
	if len(auths) == 0 && len(before) > 0 {
		writeErr(w, http.StatusConflict, "扫描到 0 个凭证文件（疑似 auth_dir 挂载异常），已拒绝执行")
		return
	}
	p.cfg.Pool.SyncToDir(auths)
	after := make(map[string]bool, len(auths))
	added := 0
	for _, s := range p.cfg.Pool.List() {
		after[s.UID] = true
		if !before[s.UID] {
			added++
		}
	}
	removed := 0
	for uid := range before {
		if !after[uid] {
			removed++
		}
	}
	log.Printf("panel: rescan auths 目录 文件=%d 新增=%d 剔除=%d", len(auths), added, removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "scanned": len(auths), "added": added, "removed": removed,
	})
}

// accountCheckin 单号签到：DailyCheckin + 余额查询解冻（已签到等业务错误不阻塞余额刷新），
// 与 scheduler.RunCheckinNow 的单号语义一致。
//
// v2.2.0 #9：收尾写一条任务执行历史（kind=checkin，trigger=manual，total=1）——
// 企业版（无签到体系）记 skip，上游拒绝记 fail，成功/幂等已签记 done。
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	checkinDone := false
	started := time.Now()
	fail, skip := 0, 0
	defer func() {
		done := 0
		if fail == 0 && skip == 0 {
			done = 1
		}
		errMsg := ""
		if fail > 0 {
			errMsg = checkinMsg
		}
		p.recordTaskRun(TaskRun{
			Kind: taskKindCheckin, Trigger: taskTriggerManual, UID: uid,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
			Total: 1, Done: done, Fail: fail, Skip: skip, Error: errMsg,
		})
	}()
	// 企业版无签到体系（上游 400 code 10001「企业账号不支持该操作」）：不发起该调用，
	// 端点退化为「刷新额度」——企业额度走 get-enterprise-user-usage 口径（见 upstream）。
	// 前端对企业号不渲染「签到」按钮；此处是 API 侧防御（外部脚本/旧缓存前端仍可能调用）。
	if a.IsEnterprise() {
		skip = 1
		checkinMsg = "企业账号无签到体系（已跳过签到，仅刷新额度）"
	} else if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "今天已签到"等业务错误照常查余额
		// 幂等拒绝同样是「今日已签」，标记后按钮显示「已签」。
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		} else {
			fail = 1
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if a.IsEnterprise() {
		resp["enterprise"] = true
	}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance 单号余额刷新：更新余额与到期快照，不触碰冷却状态。
// v2.2.0 #9：收尾写一条任务执行历史（kind=balance，trigger=manual，total=1）。
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	started := time.Now()
	refreshErr := ""
	defer func() {
		done, fail := 0, 0
		if refreshErr == "" {
			done = 1
		} else {
			fail = 1
		}
		p.recordTaskRun(TaskRun{
			Kind: taskKindBalance, Trigger: taskTriggerManual, UID: uid,
			StartedAt: started.Unix(), FinishedAt: time.Now().Unix(),
			Total: 1, Done: done, Fail: fail, Error: refreshErr,
		})
	}()
	remain, total, expiring, earliestAt, earliestRemaining, buckets, err := p.cfg.Upstream.UserResourceSnapshot(a, p.expiringSoonWindow())
	if err != nil {
		refreshErr = err.Error()
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsSnapshot(uid, pool.CreditsSnapshot{
		Remain:            remain,
		Total:             total,
		Expiring:          expiring,
		EarliestAt:        earliestAt,
		EarliestRemaining: earliestRemaining,
		ExpiryBuckets:     poolExpiryBuckets(buckets),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove 移除账号：先出池（立即落盘 state），再删 auth 文件。
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	// v2.3.0 #12：账号已出池 → 同步清理其备注/标签（accountmeta.go）。
	// 放在 Remove 之后、两个成功响应分支之前，保证「删号但备注残留」不会发生。
	p.accountMetaForget(uid)
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth 文件删除失败: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（已出池并删除凭证文件）", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 批量任务（v2.0.1 #10：同 kind 单飞 + 异步 + 409）
//
// 五个 *_all 端点共用调度器的任务单飞 gate（scheduler.BeginTask/EndTask）：
// 置位成功 → 起后台 goroutine 执行并立即返回 {ok:true,started:true}；置位失败
// （同类全池任务正在执行，手动双击或定时批次撞车）→ 409 + {"error":"…正在执行中"}。
// 前端 api() 会把非 2xx 的 error 文案原样 toast（frontend2 已接 409 文案）。
//
// balance_all 过去是「同步等全池刷完再返回 + 随响应带回 accounts」——上万账号池
// 会让反代 60s 超时（504），且与 #10 的并发闸门叠加时页面长时间无反馈。现改为
// 异步：先占 gate → 立即返回 → 后台按有界并发跑完（含昵称同步），前端 2.5s 后
// 补拉 overview 看到首批余额，其余交给常规轮询。旧响应字段 ok/started 保留。
// ---------------------------------------------------------------------------

// writeBatchConflict 已是同类任务执行中：409 + 与其它面板冲突接口同口径的 error 文案。
func writeBatchConflict(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusConflict, msg)
}

// checkinAll 手动触发全量签到（异步执行，进度看日志区/账号状态变化）。
//
// v2.3.0 #10：统一经 Job Runner 起 Job——先占 scheduler 同 kind 单飞 gate（409
// 语义不变），再注册 Job（返回 job_id，可经 GET /jobs/{id} 看进度/取消）。
// 旧响应字段 ok/started 保留；Registry 未装配时退回 v2.2 异步实现（无 job_id）。
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindCheckin)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量签到已触发（含连登管家；job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// travelAll 手动触发全量猫猫旅行巡检（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindTravel)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量旅行巡检已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// activityAll 手动触发全量活跃上报（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindActivity)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量活跃上报已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// keepaliveAll 手动触发全量 token 保活（v2.3.0 #10 Job Runner，响应附 job_id）。
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindKeepalive)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量保活已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// balanceAll 手动全量刷新余额（v2.3.0 #10 Job Runner，响应附 job_id）。
// 后台按 schedule.balance_refresh_concurrency 有界并发查上游、写回池内 credits
// （含解冻语义）并同步昵称；重复点击/周期刷新撞车 → 409。
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	spec, ok := p.jobSpecFor(jobs.KindBalance)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	jobID, started := p.triggerJob(spec)
	if !started {
		writeBatchConflict(w, spec.conflict)
		return
	}
	log.Printf("panel: 手动全量余额刷新已触发（job=%s）", jobID)
	writeJSON(w, http.StatusOK, batchStartedBody(jobID))
}

// syncNicknames 手动刷新时的昵称同步（issue #94：上游改名免重登）。
// 只在面板手动「刷新」路径调用——后台余额定时器不触发（用户明确要求资料接口
// 仅手动触达）。逐号拉 /console/account，只取 nickname（手机号等敏感字段在
// upstream.FetchAccountProfile 内即被丢弃）；单号失败静默跳过，不打断余额刷新
// 的既有结果。
func (p *Panel) syncNicknames() {
	type job struct {
		uid string
		a   *auth.Auth
	}
	if p.cfg.Pool == nil {
		return
	}
	snapshot := p.poolSnapshot()
	jobs := make([]job, 0, len(snapshot))
	for i := range snapshot {
		st := &snapshot[i]
		if st.Disabled {
			continue
		}
		if a := p.cfg.Pool.AuthByUID(st.UID); a != nil && a.AccessTokenValue() != "" {
			jobs = append(jobs, job{uid: st.UID, a: a})
		}
	}
	if len(jobs) == 0 {
		return
	}
	var (
		mu      sync.Mutex
		updated int
		failed  int
	)
	// v2.6.0 P0：固定 3 并发 worker（不再每账号一个 goroutine，10k 账号不瞬时
	// 创建 1 万 goroutine）；上游压力与历史 sem=3 完全一致。
	fanOutAccounts(context.Background(), len(jobs), 3, func(_ context.Context, i int) {
		j := jobs[i]
		nick, err := p.cfg.Upstream.FetchAccountProfile(j.a)
		if err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			return
		}
		if p.cfg.Pool.SetNickname(j.uid, nick) {
			mu.Lock()
			updated++
			mu.Unlock()
		}
	})
	if updated > 0 || failed > 0 {
		log.Printf("panel: 昵称同步：更新 %d 个，失败 %d 个（未变化不计数）", updated, failed)
	}
}
