// jsonl_test.go 面板侧 JSONL 追加/尾部读取的基础设施测试（v2.2.0 #5/#9 共用）。
package panel

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestJSONLAppendAndReadLast 追加后尾部读取：自动换行、新的在前、limit 截断。
func TestJSONLAppendAndReadLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.jsonl")
	log := newJSONLLog(path)
	if !log.enabled() {
		t.Fatal("已装配路径时应 enabled")
	}
	for i := 0; i < 5; i++ {
		log.append(map[string]any{"i": i, "text": fmt.Sprintf("row-%d", i)})
	}

	got, err := log.readLast(3)
	if err != nil {
		t.Fatalf("readLast: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("readLast(3)=%d 条 want 3", len(got))
	}
	want := []int{4, 3, 2} // 新的在前
	for i, raw := range got {
		var row struct {
			I int `json:"i"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("第 %d 条不是合法 JSON: %v", i, err)
		}
		if row.I != want[i] {
			t.Fatalf("第 %d 条 i=%d want %d（应从新到旧）", i, row.I, want[i])
		}
	}

	// limit 大于总条数：返回全部（新的在前）。
	all, err := log.readLast(100)
	if err != nil || len(all) != 5 {
		t.Fatalf("readLast(100) len=%d err=%v want 5", len(all), err)
	}

	// 文件内容应为逐行 JSON（每条一行，0600）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n"); len(lines) != 5 {
		t.Fatalf("落盘行数=%d want 5\n%s", len(lines), raw)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() == 0 {
		t.Fatal("文件不应为空")
	}
}

// TestJSONLReadLastMissingFile 文件不存在 → 空、nil（契约返回 []，不是错误）。
func TestJSONLReadLastMissingFile(t *testing.T) {
	log := newJSONLLog(filepath.Join(t.TempDir(), "nope.jsonl"))
	got, err := log.readLast(10)
	if err != nil || got != nil {
		t.Fatalf("缺失文件 readLast=%v err=%v want nil,nil", got, err)
	}
}

// TestJSONLDisabled 路径为空：写 no-op、读空，且不创建任何文件。
func TestJSONLDisabled(t *testing.T) {
	log := newJSONLLog("")
	if log.enabled() {
		t.Fatal("空路径应 disabled")
	}
	dir := t.TempDir()
	log.append(map[string]any{"a": 1}) // 不应 panic/写盘
	got, err := log.readLast(10)
	if err != nil || got != nil {
		t.Fatalf("disabled readLast=%v err=%v want nil,nil", got, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("disabled 模式不应产生文件，got %v", entries)
	}
}

// TestJSONLBadLinesSkipped 半行/损坏行被跳过，不影响其后的有效记录。
func TestJSONLBadLinesSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixed.jsonl")
	content := "{\"i\":1}\nnot-json\n{\"i\":2}\n{\"i\":3\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写测试文件: %v", err)
	}
	log := newJSONLLog(path)
	got, err := log.readLast(10)
	if err != nil {
		t.Fatalf("readLast: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("有效行=%d want 2（损坏行应跳过）: %v", len(got), got)
	}
	var first struct {
		I int `json:"i"`
	}
	if err := json.Unmarshal(got[0], &first); err != nil || first.I != 2 {
		t.Fatalf("最新有效行 i=%d err=%v want 2", first.I, err)
	}
}

// TestJSONLConcurrentAppend 并发追加：mutex 串行，行不交错、全部可读。
func TestJSONLConcurrentAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.jsonl")
	log := newJSONLLog(path)

	const writers, perWriter = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				log.append(map[string]any{"w": w, "i": i})
			}
		}(w)
	}
	wg.Wait()

	got, err := log.readLast(1000)
	if err != nil {
		t.Fatalf("readLast: %v", err)
	}
	if len(got) != writers*perWriter {
		t.Fatalf("并发写入后读到 %d 条 want %d（行交错/丢失）", len(got), writers*perWriter)
	}
	seen := map[string]bool{}
	for _, raw := range got {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("并发写入出现非法 JSON 行: %v（%s）", err, raw)
		}
		key := fmt.Sprintf("%v-%v", m["w"], m["i"])
		if seen[key] {
			t.Fatalf("重复记录 %s", key)
		}
		seen[key] = true
	}
}

// TestJSONLCreatesParentDir 父目录不存在时自动补建（首次写盘的常见场景）。
func TestJSONLCreatesParentDir(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "sub", "dir", "x.jsonl")
	log := newJSONLLog(path)
	log.append(map[string]any{"ok": true})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("应自动创建父目录并落盘: %v", err)
	}
	got, err := log.readLast(1)
	if err != nil || len(got) != 1 {
		t.Fatalf("readLast=%v err=%v want 1", got, err)
	}
}

// TestPanelDataDir 落盘目录推导：StateFile 优先 → ConfigPath/data 子目录 → ConfigPath 同目录 → 空。
func TestPanelDataDir(t *testing.T) {
	tmp := t.TempDir()
	dataSub := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataSub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tests := []struct {
		name       string
		stateFile  string
		configPath string
		want       string
	}{
		{"StateFile 同目录优先", filepath.Join(tmp, "state.json"), filepath.Join(tmp, "config.json"), tmp},
		{"StateFile 相对路径", filepath.Join("data", "state.json"), "", "data"},
		{"ConfigPath 下的 data 子目录", "", filepath.Join(tmp, "config.json"), dataSub},
		{"ConfigPath 同目录（无 data 子目录）", "", filepath.Join(tmp, "nomad", "config.json"), filepath.Join(tmp, "nomad")},
		{"都为空则功能关闭", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := panelDataDir(tc.stateFile, tc.configPath); got != tc.want {
				t.Fatalf("panelDataDir(%q,%q)=%q want %q", tc.stateFile, tc.configPath, got, tc.want)
			}
		})
	}
}

// TestTailLimit ?limit 解析：默认/上限/非法回落。
func TestTailLimit(t *testing.T) {
	tests := []struct {
		query string
		def   int
		max   int
		want  int
	}{
		{"", 100, 1000, 100},
		{"?limit=5", 100, 1000, 5},
		{"?limit=0", 100, 1000, 100},
		{"?limit=-3", 100, 1000, 100},
		{"?limit=abc", 100, 1000, 100},
		{"?limit=99999", 100, 1000, 1000},
	}
	for _, tc := range tests {
		r := httptest.NewRequest("GET", "/panel/api/x"+tc.query, nil)
		if got := tailLimit(r, tc.def, tc.max); got != tc.want {
			t.Errorf("tailLimit(%q)=%d want %d", tc.query, got, tc.want)
		}
	}
}

// TestTruncateRunes 按 rune 截断，不切坏中文。
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcdef", 3); got != "abc…" {
		t.Fatalf("truncateRunes(abcdef,3)=%q", got)
	}
	if got := truncateRunes("中文测试", 2); got != "中文…" {
		t.Fatalf("truncateRunes(中文测试,2)=%q", got)
	}
	if got := truncateRunes("abc", 10); got != "abc" {
		t.Fatalf("未超长不应截断: %q", got)
	}
}
