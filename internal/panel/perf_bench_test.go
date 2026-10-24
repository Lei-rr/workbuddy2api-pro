// perf_bench_test.go v2.5.0 性能专项：面板读端点微基准（只加基准文件，不改生产代码）。
//
// 覆盖 1k/5k/10k 账号下常用读端点（httptest 直打 handler）：
//   - /panel/api/overview?lite=1 与全量 overview；
//   - /panel/api/accounts 分页 light/full、q 过滤、tag 过滤、全部（2000 上限）、ETag 304；
//   - /panel/api/metrics（含全池 List 累加，Prometheus 抓取路径）。
//
// 运行：
//
//	go test -run '^$' -bench 'BenchmarkPanel' -benchmem ./internal/panel/
//	go test -run '^$' -bench 'BenchmarkPanelOverviewLite/n=10000' -benchmem \
//	    -cpuprofile D:\dsh\tmp\panel-cpu.out ./internal/panel/
//	go tool pprof -top -nodecount=20 D:\dsh\tmp\panel-cpu.out
package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/auth"
	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// benchPanelSizes 三档账号规模（与 tools/perfbench / pool 微基准对齐）。
var benchPanelSizes = []int{1000, 5000, 10000}

const benchPanelKey = "bench-key"

// newBenchPanel 构造启用面板读接口的 n 账号 Panel（t.TempDir 落 account-meta.json）。
// 状态与本基准同构：积分/冷却/模型成本非零，~10% 账号带 vip 标签。
func newBenchPanel(b *testing.B, n int) *Panel {
	b.Helper()
	dir := b.TempDir()
	pl := pool.New("") // 不落盘/不起 flusher；纯内存基准
	now := time.Now()
	exp := now.Add(72 * time.Hour).Unix()
	for i := 0; i < n; i++ {
		uid := fmt.Sprintf("u%06d", i)
		domain := "copilot.tencent.com"
		if i%5 == 0 {
			domain = "workbuddy.ai"
		}
		a := &auth.Auth{
			UID:          uid,
			AccessToken:  fmt.Sprintf("at-%06d", i),
			RefreshToken: fmt.Sprintf("rt-%06d", i),
			ExpiresAt:    exp,
			Domain:       domain,
			Nickname:     "bench-" + uid,
		}
		if i%20 == 0 {
			a.EnterpriseID = fmt.Sprintf("ent-%06d", i)
		}
		pl.Add(a)
		pl.SetCredits(uid, int64(100+(i*37)%9900), int64(1000+(i*101)%9000))
		if i%10 < 7 {
			pl.NoteCheckinDone(uid)
		}
		if i%23 == 3 {
			pl.Cooldown(uid, pool.CoolSoft, 30*time.Minute, "bench soft rate")
		}
		if i%5 == 0 {
			pl.SetCreditsDetailed(uid, int64(100+(i*37)%9900), int64(1000+(i*101)%9000),
				int64(50+i%300), now.Add(72*time.Hour), int64(50+i%300))
		}
		// 实测扣费账本（与 perfbench state.json / 真实大池同构：List/statusOf 构造 ModelCosts）。
		if i%4 == 0 {
			pl.NoteModelCost(uid, "claude-sonnet-4-20250514", 0.003, 1000)
		}
		if i%8 == 0 {
			pl.NoteModelCost(uid, "gpt-5", 0.01, 500)
		}
	}
	// account-meta.json：vip 约 10%、trial 约 5%（tag= 过滤基准用）。
	meta := map[string]any{"version": 1, "accounts": map[string]any{}}
	acc := meta["accounts"].(map[string]any)
	for i := 0; i < n; i++ {
		var tags []string
		if i%10 == 0 {
			tags = append(tags, "vip")
		}
		if i%20 == 0 {
			tags = append(tags, "trial")
		}
		if len(tags) > 0 {
			acc[fmt.Sprintf("u%06d", i)] = map[string]any{"tags": tags, "updated_at": now.Unix()}
		}
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		b.Fatalf("meta marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "account-meta.json"), raw, 0o600); err != nil {
		b.Fatalf("meta write: %v", err)
	}
	p := New(Config{
		Pool:      pl,
		APIKey:    benchPanelKey,
		Version:   "bench",
		StateFile: filepath.Join(dir, "state.json"), // 面板数据目录 → dir
	})
	// warmup：加载 account-meta 懒存储 + 请求计数，避免首个基准迭代计入初始化。
	p.ServeHTTP(httptest.NewRecorder(), benchRequest("/panel/api/overview?lite=1", ""))
	p.ServeHTTP(httptest.NewRecorder(), benchRequest("/panel/api/accounts?page=1&page_size=10&tag=vip", ""))
	return p
}

// benchRequest 构造带 Bearer 的 GET 请求（If-None-Match 可选）。
func benchRequest(target, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+benchPanelKey)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

// benchPanelTarget 对指定端点做基准：req 复用（服务端只读），每迭代新 Recorder，
// 并上报响应体字节数（resp_bytes/op）。
func benchPanelTarget(b *testing.B, p *Panel, target string, useETag bool) {
	b.Helper()
	req := benchRequest(target, "")
	warm := httptest.NewRecorder()
	p.ServeHTTP(warm, req)
	if warm.Code != http.StatusOK {
		b.Fatalf("warmup %s status=%d body=%s", target, warm.Code, warm.Body.String())
	}
	etag := warm.Header().Get("ETag")
	if useETag {
		if etag == "" {
			b.Fatalf("%s 未返回 ETag", target)
		}
		req = benchRequest(target, etag)
	}
	bodyLen := warm.Body.Len()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		if useETag {
			if w.Code != http.StatusNotModified {
				b.Fatalf("304 期望未命中：status=%d", w.Code)
			}
		} else if w.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(bodyLen), "resp_bytes")
}

// BenchmarkPanelOverviewLite /panel/api/overview?lite=1（前端 5s 轮询主路径）。
func BenchmarkPanelOverviewLite(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/overview?lite=1", false)
		})
	}
}

// BenchmarkPanelOverviewFull /panel/api/overview（含 accounts 数组；旧接口/全量轮询）。
func BenchmarkPanelOverviewFull(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/overview", false)
		})
	}
}

// BenchmarkPanelAccountsPage 分页 accounts（默认 light 100 行：前端账号页主路径）。
func BenchmarkPanelAccountsPage(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=100&fields=light", false)
		})
	}
}

// BenchmarkPanelAccountsPageFull fields=full（含到期批次/模型成本等完整字段）。
func BenchmarkPanelAccountsPageFull(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=100&fields=full", false)
		})
	}
}

// BenchmarkPanelAccountsFilterQ q= 全匹配过滤（全量扫描 + 排序后分页）。
func BenchmarkPanelAccountsFilterQ(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=100&q=bench", false)
		})
	}
}

// BenchmarkPanelAccountsTag tag=vip 标签过滤（反向索引命中 ~10% 账号）。
func BenchmarkPanelAccountsTag(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=100&tag=vip", false)
		})
	}
}

// BenchmarkPanelAccountsAll page_size=0（全部，服务端截断 2000 条；面板「全部」按钮）。
func BenchmarkPanelAccountsAll(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=0&fields=light", false)
		})
	}
}

// BenchmarkPanelAccountsETag304 ETag 命中：服务端仍全量 List+filter+sort+sha256，仅省 body。
func BenchmarkPanelAccountsETag304(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/accounts?page=1&page_size=100&fields=light", true)
		})
	}
}

// BenchmarkPanelMetrics Prometheus 抓取（内部额外全池 List 累加 in-flight）。
func BenchmarkPanelMetrics(b *testing.B) {
	for _, n := range benchPanelSizes {
		n := n
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchPanelTarget(b, newBenchPanel(b, n), "/panel/api/metrics", false)
		})
	}
}
