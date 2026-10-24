// helpers.go v2.6.0 重构批：跨 handler 的公共小工具（账号级有界扇出/限长
// JSON 解码/limit 解析/账号查找）。只做行为等价的抽取与收敛，不放业务逻辑；
// 业务 handler 仍在各自域文件（panel.go/tasks.go/autotask.go/taskcenter.go...）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
)

const (
	// panelFanOutDefaultConcurrency 账号级扇出默认并发（v2.6.0 P0）。
	//
	// 背景：补领清扫（默认开启、15 分钟一次）、任务扫描、队列扫描、昵称同步、
	// 券码查询此前都是「每个账号一个 goroutine + 若干上游请求」——10k 账号时
	// 一轮瞬时 ~1 万 goroutine/连接，既打上游风控也造成内存/连接尖峰。
	//
	// 配置接线：main 把 schedule.task_sweep_concurrency 注入
	// panel.Config.TaskSweepConcurrency；未接线时用本默认值（安全兜底）。
	panelFanOutDefaultConcurrency = 8
	// panelFanOutMaxConcurrency 配置值上限（防误配置成 1 万）。
	panelFanOutMaxConcurrency = 64
	// panelJSONBodyMax 常规面板 POST JSON body 上限（1MB）：tasks/autotask/
	// taskcenter 等小 body 接口共用；import 走单独的 32MB 上限。
	panelJSONBodyMax = 1 << 20
)

// fanOutConcurrency 当前生效的账号级扇出并发：配置 >0 用之（clamp 1..64），
// 否则默认 8。
func (p *Panel) fanOutConcurrency() int {
	n := panelFanOutDefaultConcurrency
	if p != nil && p.cfg.TaskSweepConcurrency > 0 {
		n = p.cfg.TaskSweepConcurrency
	}
	if n < 1 {
		n = 1
	}
	if n > panelFanOutMaxConcurrency {
		n = panelFanOutMaxConcurrency
	}
	return n
}

// fanOutAccounts 以固定并发 worker 处理下标 [0,n)。
//
// 语义（v2.6.0 P0）：
//   - 最多 concurrency 个 fn 同时执行（并发峰值 ≤ concurrency，有测试钉住）；
//   - ctx 取消后立即停止派发新下标；已在执行的 fn 会跑完（上游 HTTP 调用自身
//     受超时约束），函数返回时所有 worker 已退出；
//   - 不保证顺序；fn 必须并发安全，由调用方自行聚合结果/加锁。
func fanOutAccounts(ctx context.Context, n, concurrency int, fn func(ctx context.Context, i int)) {
	if n <= 0 || fn == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > n {
		concurrency = n
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for w := 0; w < concurrency; w++ {
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(ctx, i)
			}
		}()
	}
	wg.Wait()
}

// readJSONBody 限长解码 JSON body：超限 → 413，格式错误 → 400；成功返回 true。
// 面板所有 POST JSON body 统一走这里（v2.6.0 P1：补齐输入体上限 + 错误文案一致）。
func readJSONBody(w http.ResponseWriter, r *http.Request, maxBytes int64, v any) bool {
	if r == nil || r.Body == nil {
		writeErr(w, http.StatusBadRequest, "missing request body")
		return false
	}
	if maxBytes <= 0 {
		maxBytes = panelJSONBodyMax
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// parseLimit 解析 ?limit（默认 def、上限 max、非法/<=0 回落 def；容忍首尾空白）。
// v2.6.0 前有 4 份等价实现（audit/task_runs/jobs/requestLogs/creditHistory），
// 统一到这里；各端点原有默认值/上限常量保持不变（契约不变）。
func parseLimit(r *http.Request, def, max int) int {
	n := def
	if r != nil {
		if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x > 0 {
				n = x
			}
		}
	}
	if max > 0 && n > max {
		n = max
	}
	return n
}

// accountForRequest 取路径参数 {uid} 对应的账号；不存在时写 404 并返回 false。
// 等价于原 accountByUID 与各 handler 手写的 Pool.AuthByUID/Status 分支（v2.6.0
// 纯重构：错误形状/状态码与原来完全一致）。
func (p *Panel) accountForRequest(w http.ResponseWriter, r *http.Request) (*auth.Auth, bool) {
	if p == nil || p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "account pool not available")
		return nil, false
	}
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil, false
	}
	return a, true
}

// logFanOut 记录一次有界扇出的规模信息（进度聚合用；不逐账号打日志）。
func logFanOut(op string, total, concurrency int) {
	log.Printf("panel: %s 开始（账号 %d，并发上限 %d）", op, total, concurrency)
}
