// travel.go 猫猫旅行巡检状态机：随旅行时点（travel_hours，默认 09 点）对池内每个可用账号单趟推进一次。
// 无猫 → 同意协议 + 领养；有猫 → 按 travel/status 分派 派出 / 领奖 / 跳过。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

const (
	// travelLocationID 派出地点固定 4（古镇客栈）：4 个地点收益/时长区间完全相同，无最优解。
	travelLocationID = 4

	// travelStateIdle 空闲可派出；travelStateTraveling 在途；travelStateArrived 到站可领奖。
	travelStateIdle      = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived   = "arrived"
)

// travelAccountDelay 账号间限速：全量账号约 40s，避免上游风控。测试可置 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay 活跃上报账号间限速：与旅行同口径，避免上游风控。测试可置 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap 领养前置上报后的等待：给上游事件处理留时间再发 buddy/first。
// 对齐 scripts/task_first_buddy.py 实测的 1.05s 间隔口径。测试可置 0。
var adoptReportGap = 1050 * time.Millisecond

// cstZone 上游每日重置按自然日 00:00 CST（Asia/Shanghai）。中国无夏令时，固定 +8 即可，
// 不依赖容器 tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay 返回 t 所属的上游自然日（CST），格式 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow 立即对池内所有可用账号执行一趟旅行巡检。
// 禁用账号跳过；暂停选号（paused）账号照常参与——旅行是纯 RPC（状态/派出/
// 领奖 + 领养前置上报），不发模型对话，与「让位防风控」不冲突；
// 401/查询失败只跳过该账号本轮（不强刷 token，交 22:00 keepalive）；
// 账号间限速 travelAccountDelay。
// 本函数是无 ctx 的外部入口（面板/测试一次性触发）；排程主循环走 runTravel。
func (s *Scheduler) RunTravelNow() {
	_ = s.RunTravelCtx(context.Background(), nil)
}

// RunTravelCtx 是 RunTravelNow 的 ctx + 进度变体（v2.3.0 #10 Job Runner 用）。
// 进度口径：done=已处理账号数，fail=该账号本轮巡检失败（状态机返回 error），
// skip=禁用/无凭证/域门控跳过数。
func (s *Scheduler) RunTravelCtx(ctx context.Context, progress ProgressFunc) error {
	s.runTravel(ctx, progress)
	return nil
}

// runTravel 旅行巡检遍历（ctx 感知）：账号间限速与领养前置等待都改用 sleepCtx，
// ctx 取消立即收尾（优雅停机不必等限速睡满，剩余账号留给下一轮）。
func (s *Scheduler) runTravel(ctx context.Context, progress ProgressFunc) {
	first := true
	stList := s.cfg.Pool.List()
	total := len(stList)
	done, fail, skip := 0, 0, 0
	for _, st := range stList {
		if ctx.Err() != nil {
			return
		}
		if st.Disabled {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			skip++
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		if a.IsGlobal() {
			skip++ // D4 门控：global 无 CN 任务体系，不发起任何上游调用
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		if a.IsEnterpriseSession() {
			skip++ // 企业版 / 企业切换会话门控：无成长体系（growth 端点企业号 403；切换会话不计分），不发起任何上游调用
			reportProgress(progress, done, total, fail, skip, "")
			continue
		}
		if !first {
			if !sleepCtx(ctx, travelAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再巡检
			}
		}
		first = false
		if err := s.travelOne(ctx, a); err != nil {
			fail++
			done++
			reportProgress(progress, done, total, fail, skip,
				fmt.Sprintf("travel %s: %v", logfmt.Label(a.UID, a.Nickname), err))
			continue
		}
		done++
		reportProgress(progress, done, total, fail, skip, "")
	}
	if ctx.Err() == nil {
		s.noteTaskDone(taskTravel, s.now())
	}
}

// travelOne 单账号单趟状态机：查有无猫 + 查状态 + 最多一个动作，不轮询不等待。
// 返回 error 表示该账号本轮失败（调用方计入 Job.Fail）；travelling/未知状态等
// 「本轮按状态跳过」属正常返回（不计失败）。
func (s *Scheduler) travelOne(ctx context.Context, a *auth.Auth) error {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	if buddy == nil {
		return s.travelAdopt(ctx, a)
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	switch ts.State {
	case travelStateArrived:
		return s.travelClaim(a, ts)
	case travelStateIdle:
		return s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
		return nil
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
		return nil
	}
}

// travelDepart 空闲且未达当日上限时派出（每日 1 次，自然日 00:00 CST 重置）。
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) error {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return nil
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
	return nil
}

// travelClaim 到站领奖（必须带 record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) error {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return nil
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return err
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
	return nil
}

// travelAdopt 无猫时领养，链路：report → agreement → buddy/first。
//
// report 必须先跑（scripts/task_first_buddy.py 实测）：一条 chat_request_send 上报
// 点亮 growth 连登并**解锁 first_buddy 任务**；未上报时 buddy/first 会返回
// 400 "first_buddy task not completed yet"——该门槛的真实来源是"当日无活跃上报"，
// 不是账号问题（report.go 注释亦明确「解锁 first_buddy 任务（领养前置）」）。
// conversation 门槛未达标仍属预期行为，记一次当日已试后静默跳过，不再重试。
func (s *Scheduler) travelAdopt(ctx context.Context, a *auth.Auth) error {
	if s.adoptTriedToday(a.UID) {
		return nil
	}
	// 前置：解锁 first_buddy 任务（幂等；失败不阻塞，让 buddy/first 按既有错误路径暴露）。
	if err := s.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else if !sleepCtx(ctx, adoptReportGap) {
		return ctx.Err() // ctx 取消：不再等前置上报的落地间隔，放弃本号剩余动作（下轮再试）
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
		return nil // 门槛未达属预期跳过，不计本轮失败
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
		return err
	}
	return nil
}

// adoptTriedToday 该账号当日是否已判定领养门槛未达。
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried 记录该账号当日已尝试领养且未过门槛。
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
