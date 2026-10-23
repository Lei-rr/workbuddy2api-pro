// jsonl.go 面板侧轻量 JSONL 落盘（v2.2.0 #5 操作审计 / #9 任务执行历史共用）。
//
// 设计取舍：
//   - 一个文件一个 jsonlLog：追加写（O_APPEND，并发写由 mutex 串行）、权限 0600、
//     写失败只 WARN（首败 + 每 100 次）绝不阻断主流程——审计/历史是旁路观测，不能
//     因为磁盘满/目录只读让运维操作失败；
//   - 查询走「尾部倒读」：从文件末尾按 32KB 块向前读，最多解析最近 N 行，避免把
//     整个文件读进内存（文件长期运行会不断增长）；
//   - 路径未装配（panel.Config.StateFile 与 ConfigPath 都为空）时功能自动关闭：
//     写 no-op、读返回空——单元测试与只读装配不产生副作用文件。
package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const (
	// jsonlTailChunk 尾部倒读的单次块大小：32KB 足够覆盖 100~200 条审计/历史行。
	jsonlTailChunk = 32 << 10
	// jsonlWarnEvery 连续写失败时每 N 次补一条 WARN（首败必报，避免刷屏）。
	jsonlWarnEvery = 100
	// jsonlDetailMax 写入前 detail 类短文本的截断长度（防异常响应体撑大文件）。
	jsonlDetailMax = 240
)

// jsonlLog 单文件追加式 JSONL 落盘器（并发安全）。
type jsonlLog struct {
	path  string // 空 = 未启用（写 no-op、读空）
	mu    sync.Mutex
	fails int // 连续写失败次数（WARN 节流；写成功清零）
}

// newJSONLLog 构造落盘器；path 为空表示功能关闭。
func newJSONLLog(path string) *jsonlLog { return &jsonlLog{path: path} }

// enabled 报告是否已装配落盘路径。
func (l *jsonlLog) enabled() bool { return l != nil && l.path != "" }

// append 追加一条 JSON 记录（自动补换行）。写失败只 WARN（首败 + 每 100 次），
// 不向调用方返回错误——审计/历史写盘失败不得影响面板操作结果。
func (l *jsonlLog) append(v any) {
	if !l.enabled() {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		log.Printf("WARN: [panel] JSONL 序列化失败（%s）: %v", l.path, err)
		return
	}
	raw = append(raw, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.appendLocked(raw); err != nil {
		l.fails++
		if l.fails == 1 || l.fails%jsonlWarnEvery == 0 {
			log.Printf("WARN: [panel] JSONL 写入失败（%s，连续第 %d 次）: %v", l.path, l.fails, err)
		}
		return
	}
	l.fails = 0 // 成功即清零：下次失败重新首报
}

// appendLocked 在持锁状态下追加写；父目录不存在时补建（幂等）。
func (l *jsonlLog) appendLocked(raw []byte) error {
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(raw)
	return err
}

// readLast 返回最近 limit 条记录（**新的在前**）。文件不存在/功能未启用 → 空、nil。
// 读与写共用 l.mu：避免读到正在追加的半行（半行本身也会被 json.Valid 跳过）。
func (l *jsonlLog) readLast(limit int) ([]json.RawMessage, error) {
	if !l.enabled() || limit <= 0 {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 尚无记录：契约返回 []，不是错误
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// 尾部倒读：从文件末尾向前搬块，直到换行数 > limit 或到文件头。
	data := make([]byte, 0, jsonlTailChunk)
	start := fi.Size()
	for start > 0 && bytes.Count(data, []byte{'\n'}) <= limit {
		step := int64(jsonlTailChunk)
		if step > start {
			step = start
		}
		start -= step
		chunk := make([]byte, step)
		if _, rerr := f.ReadAt(chunk, start); rerr != nil && rerr != io.EOF {
			return nil, rerr
		}
		data = append(chunk, data...)
	}

	lines := bytes.Split(data, []byte{'\n'})
	if start > 0 && len(lines) > 0 {
		lines = lines[1:] // 未从文件头开始读，首行可能是半行：丢弃
	}
	for len(lines) > 0 && len(bytes.TrimSpace(lines[len(lines)-1])) == 0 {
		lines = lines[:len(lines)-1] // 以换行结尾时最后一个元素为空
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:] // 只要最近 limit 行
	}
	out := make([]json.RawMessage, 0, len(lines))
	for i := len(lines) - 1; i >= 0; i-- { // 倒序：新的在前
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !json.Valid(line) {
			continue // 半行/损坏行跳过，不影响其它历史
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	return out, nil
}

// tailLimit 解析 ?limit（默认 def、上限 max、非法/<=0 回落 def）。
func tailLimit(r *http.Request, def, max int) int {
	n := def
	if r != nil {
		if v := r.URL.Query().Get("limit"); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x > 0 {
				n = x
			}
		}
	}
	if n > max {
		n = max
	}
	return n
}

// panelDataDir 推导面板自有 JSONL 的落盘目录（审计/任务历史与 state 文件同目录）。
//
// 优先级：
//  1. StateFile 非空 → 与 state 文件同目录（与 usage/request-logs/credit-history 同一
//     规则；主装配注入 panel.Config.StateFile 后即为生产口径）；
//  2. 否则若 ConfigPath 同目录存在 data/ 子目录 → 用它（默认部署布局：当前目录
//     config.json + data/state.json 时无需额外 wiring 也能落到 data/）；
//  3. 否则 ConfigPath 同目录；
//  4. 都为空 → ""（功能关闭）。
func panelDataDir(stateFile, configPath string) string {
	if stateFile != "" {
		dir := filepath.Dir(stateFile)
		if dir == "" {
			return "."
		}
		return dir
	}
	if configPath != "" {
		dir := filepath.Dir(configPath)
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, "data")
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate
		}
		return dir
	}
	return ""
}

// joinDataFile 把文件名拼到已推导的数据目录；目录为空表示功能关闭，返回 ""。
func joinDataFile(dataDir, name string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, name)
}

// truncateRunes 截断字符串到 max 个字符（按 rune 计，避免把中文截成半个字节）。
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
