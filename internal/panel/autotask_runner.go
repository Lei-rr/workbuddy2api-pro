// autotask_runner.go 任务查询/接受/领奖/一键完成执行器（v2.6.1 拆分自 autotask.go，纯移动）。
package panel

import (
	"fmt"
	"log"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/logfmt"
	"github.com/lei-rr/workbuddy2api-pro/internal/upstream"
)

// claimTaskReward 按任务码口径领奖：mp 码走 chat 域 mp 端点，其余走 Web 端。
// claim 幂等（重复领返回 already_claimed → 0 分 0 能）。
func claimTaskReward(p *Panel, a *auth.Auth, code string) (credit, energy int64, err error) {
	if isMPTaskCode(code) {
		return p.cfg.Upstream.ClaimRewardMP(a, code)
	}
	return p.cfg.Upstream.ClaimReward(a, code)
}

// taskByCode 拉取任务列表并定位单个任务；未找到返回 nil（不视为错误）。
// 双口径：mp 专属任务在默认列表查不到，自动回落 mp 列表（仅对已登记的 mp 码，
// 未知码不多打一次上游）。
func (p *Panel) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数。
// 背景：上游计分是**异步**的——行为事件上报后进度要数秒才刷新（实测 Model_chat
// 对话完成后立即回读仍是 0/1，约 5-8 秒后才变 1/1）。一次性回读会误判"未达标"，
// 从而跳过自动领奖。这里最多轮询 N 次、每次间隔 gap，总预算约 12 秒。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
// 已达标（claimable）立即返回；预算耗尽返回最后一次结果（可能仍未达标）。
func (p *Panel) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil {
			return t, nil // 轮询期间的查询失败不覆盖已拿到的结果
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskByCodeMP 以小程序口径拉取任务列表并定位单个任务；未找到返回 nil。
// 小程序限定任务（school_season / Sequential_Tasks_1）在默认口径列表不出现。
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptWithVerifyMP accept 并回读验证登记生效：上游存在 200+OK 但 accept 未真正
// 登记的形态（此时上报事件全部不归账，任务永远点不亮，上游 task_runner c793ae3
// 实测）——判定以回读 accept_status 为准，未生效重试一次。
func (p *Panel) acceptWithVerifyMP(a *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(a, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept 尝试%d: %v", logfmt.Label(a.UID, a.Nickname), code, attempt, err)
			continue
		}
		time.Sleep(mpActionGap)
		t, err := p.taskByCodeMP(a, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept 尝试%d 未登记生效（回读=%q）", logfmt.Label(a.UID, a.Nickname), code, attempt, acceptStatusOr(t))
	}
	return false
}

// acceptStatusOr 安全读取任务 accept_status（nil 任务返回 "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// ---------------------------------------------------------------------------
// 全量自动完成
// ---------------------------------------------------------------------------

// runAutoAll 对单账号依次执行所有可自动化任务，返回逐项结果。
// 供「一键完成全部可自动任务」使用；单项失败不影响后续项。
//
// 流程：先把所有未接受的任务批量 accept（规范状态机；上游脚本建议"先 accept"），
// 再逐项执行行为链路。accept 不是进度产生的必要条件，但让后续状态流转规范。
func (p *Panel) runAutoAll(a *auth.Auth) []map[string]any {
	var out []map[string]any

	// 阶段 0：批量接受尚未接受的任务（失败不阻塞——行为事件才是进度唯一判据）。
	if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
		var codes []string
		for _, t := range tasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				codes = append(codes, t.TaskCode)
			}
		}
		if len(codes) > 0 {
			if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "error",
					"message": "接受任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个任务", len(codes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	// 阶段 0b：小程序口径任务单独接受（默认列表不含 mp 码；失败不阻塞）。
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				mpCodes = append(mpCodes, t.TaskCode)
			}
		}
		if len(mpCodes) > 0 {
			if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "error",
					"message": "接受小程序任务失败（不阻塞后续）: " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(批量接受-mp)", "status": "done",
					"message": fmt.Sprintf("已接受 %d 个小程序任务", len(mpCodes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	for _, act := range autoActions {
		item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}
		before, err := p.taskByCode(a, act.TaskCode)
		if err != nil {
			item["status"] = "error"
			item["message"] = "查询失败: " + err.Error()
			out = append(out, item)
			continue
		}
		if before == nil {
			item["status"] = "skipped"
			item["message"] = "该账号无此任务"
			out = append(out, item)
			continue
		}
		if before.Claimed || before.Current >= before.Target && before.Target > 0 {
			item["status"] = "skipped"
			msg := "已完成（" + taskProgressText(before) + "）"
			// 达标未领：先补领再跳过（issue #174）。经真实对话等链路点亮的
			// 任务不经过任何自动动作，若这里只 skip，奖永远没人领（积分卡在
			// 半路）。claim 幂等（重复领返回 already_claimed → 0 分 0 能），
			// 宁可多试一轮也不漏领；判据用 progress（Current>=Target），不
			// 依赖 accept_status=="completed"（回读会短暂转 completed 后回滚，
			// 据此领奖会误领）。
			if !before.Claimed {
				credit, energy, cerr := claimTaskReward(p, a, act.TaskCode)
				switch {
				case cerr != nil:
					item["claim_error"] = cerr.Error()
					msg += "；补领失败（可在列表手动重试）"
				case credit > 0 || energy > 0:
					item["claimed"] = true
					item["credit"] = credit
					item["energy"] = energy
					msg += fmt.Sprintf("；已补领 +%d 分 +%d 能", credit, energy)
				default:
					msg += "；奖励此前已领取"
				}
			}
			item["message"] = msg
			out = append(out, item)
			continue
		}
		msg, err := act.run(p, a)
		if err != nil {
			item["status"] = "error"
			item["message"] = err.Error()
			out = append(out, item)
			continue
		}
		var after *upstream.Task
		if isMPTaskCode(act.TaskCode) {
			after, _ = p.taskByCodeMP(a, act.TaskCode)
		} else {
			after, _ = p.taskByCodeWaiting(a, act.TaskCode)
		}
		item["status"] = "done"
		item["message"] = msg
		item["progress_after"] = taskProgressText(after)
		// 进度达标即自动领奖（mp 任务走 chat 域 mp 口径，其余 Web 端接口）。
		// 领奖失败不掩盖主流程结果：status 仍为 done，附加 claim_error 供前端提示。
		if after != nil && after.Claimable {
			item["claimable"] = true
			credit, energy, cerr := claimTaskReward(p, a, act.TaskCode)
			if cerr == nil {
				item["claimed"] = true
				item["credit"] = credit
				item["energy"] = energy
				if credit > 0 || energy > 0 {
					item["message"] = msg + fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
				} else {
					item["message"] = msg + "；奖励此前已领取"
				}
			} else {
				item["claim_error"] = cerr.Error()
				item["message"] = msg + "；达标但领奖失败（可在列表手动重试）"
			}
		}
		out = append(out, item)
		time.Sleep(reportGap) // 项间节流
	}
	return out
}

// ---------------------------------------------------------------------------
// 单账号「做任务」接口（POST /panel/api/accounts/{uid}/tasks/run）
// ---------------------------------------------------------------------------

// taskRunSummary 单账号「做任务」的汇总计数（响应里的 summary 字段）。
//
// 合同（前端按字段名消费，勿改名）：
//   - total：results 条目数（含 runAutoAll 的两条「(批量接受)」合成行）；
//   - done / skipped / error：results[].status 对应计数；
//   - claimed：results[].claimed == true 的条目数，即**本轮成功走完领奖流程**的任务数。
//     注意含上游幂等返回「此前已领取」的情形（此时 credit/energy 为 0，消息里写
//     「奖励此前已领取」）——它们同样代表该条奖励已确认到账，计入 claimed 便于前端
//     展示「领奖 N」，但不代表本轮新增了 N 份积分。
type taskRunSummary struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Skipped int `json:"skipped"`
	Error   int `json:"error"`
	Claimed int `json:"claimed"`
}

// summarizeTaskRun 统计 runAutoAll 的逐项结果（纯函数，单测直接钉计数口径）。
func summarizeTaskRun(results []map[string]any) taskRunSummary {
	var s taskRunSummary
	s.Total = len(results)
	for _, item := range results {
		switch item["status"] {
		case "done":
			s.Done++
		case "skipped":
			s.Skipped++
		case "error":
			s.Error++
		}
		if claimed, _ := item["claimed"].(bool); claimed {
			s.Claimed++
		}
	}
	return s
}
