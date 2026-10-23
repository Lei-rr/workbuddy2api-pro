// accountmeta_test.go 账号备注/标签服务端存储（v2.3.0 #12）的契约测试。
//
// 路由由 panel.go（W2-A）注册；本测试直接调用 handler 方法（用 SetPathValue 注入
// {uid} 路径参数），因此不依赖路由注册时序，也不会污染仓库数据（全部用 t.TempDir）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lei-rr/workbuddy2api-pro/internal/pool"
)

// newAccountMetaPanel 构造启用 account-meta 落盘的面板（dataDir = t.TempDir()）。
func newAccountMetaPanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{
		Version: "test", APIKey: accountsTestKey, Pool: pl,
		StateFile: filepath.Join(dir, "state.json"),
	})
	if p.dataDir != dir {
		t.Fatalf("dataDir=%q want %q（StateFile 同目录推导）", p.dataDir, dir)
	}
	return p, dir
}

// metaRecordResp 单账号元数据响应/列表值的 Go 镜像（契约字段名即 JSON tag）。
type metaRecordResp struct {
	OK        bool     `json:"ok"`
	UID       string   `json:"uid"`
	Note      string   `json:"note"`
	Tags      []string `json:"tags"`
	UpdatedAt int64    `json:"updated_at"`
}

// metaListResp GET /panel/api/meta 响应。
type metaListResp struct {
	OK   bool                      `json:"ok"`
	Meta map[string]metaRecordResp `json:"meta"`
}

// metaCall 直接调用 meta handler（list=true 时不注入 uid 路径参数）。
// 请求路径用固定安全串（handler 只读 SetPathValue 注入的 uid）：避免非法 uid 里的
// 空格/斜杠把 httptest.NewRequest 的 target 解析弄坏。
func metaCall(t *testing.T, p *Panel, h http.HandlerFunc, method, uid, body string, list bool) *httptest.ResponseRecorder {
	t.Helper()
	path := "/panel/api/meta"
	if !list {
		path = "/panel/api/accounts/_uid_/meta"
	}
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if !list {
		req.SetPathValue("uid", uid)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// metaPut 调 PUT 并解码响应（断言 200）。
func metaPut(t *testing.T, p *Panel, uid, note string, tags ...string) metaRecordResp {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"note": note, "tags": tags})
	rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, uid, string(body), false)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT uid=%q status=%d body=%s", uid, rec.Code, rec.Body.String())
	}
	var resp metaRecordResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode PUT: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK || resp.UID != uid {
		t.Fatalf("PUT resp ok=%v uid=%q body=%s", resp.OK, resp.UID, rec.Body.String())
	}
	return resp
}

// metaGet 调 GET 并解码响应（断言 200）。
func metaGet(t *testing.T, p *Panel, uid string) metaRecordResp {
	t.Helper()
	rec := metaCall(t, p, p.accountMetaGet, http.MethodGet, uid, "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET uid=%q status=%d body=%s", uid, rec.Code, rec.Body.String())
	}
	var resp metaRecordResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode GET: %v body=%s", err, rec.Body.String())
	}
	return resp
}

// metaList 调 LIST 并解码响应（断言 200）。
func metaList(t *testing.T, p *Panel) metaListResp {
	t.Helper()
	rec := metaCall(t, p, p.accountMetaList, http.MethodGet, "", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp metaListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode LIST: %v body=%s", err, rec.Body.String())
	}
	if !resp.OK {
		t.Fatalf("LIST ok=false body=%s", rec.Body.String())
	}
	return resp
}

// readMetaFile 读取并解码落盘的 account-meta.json。
func readMetaFile(t *testing.T, dir string) accountMetaFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, accountMetaFileName))
	if err != nil {
		t.Fatalf("读取 %s: %v", accountMetaFileName, err)
	}
	var f accountMetaFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("落盘文件不是合法 JSON: %v\n%s", err, raw)
	}
	return f
}

// TestAccountMetaPutGetList 读写/覆盖基础链路：PUT → GET → LIST 一致，磁盘格式 version=1。
func TestAccountMetaPutGetList(t *testing.T) {
	p, dir := newAccountMetaPanel(t)

	before := time.Now().Unix() - 1
	put := metaPut(t, p, "uid-aaaa-1111", "采购A-备用", "采购A", "待补签")
	if put.Note != "采购A-备用" || len(put.Tags) != 2 || put.Tags[0] != "采购A" || put.Tags[1] != "待补签" {
		t.Fatalf("PUT 响应=%+v", put)
	}
	if put.UpdatedAt < before {
		t.Fatalf("updated_at=%d 应 >= %d", put.UpdatedAt, before)
	}

	got := metaGet(t, p, "uid-aaaa-1111")
	if got.Note != put.Note || len(got.Tags) != 2 || got.UpdatedAt != put.UpdatedAt {
		t.Fatalf("GET=%+v want %+v", got, put)
	}

	// 第二个账号只写 tags（note 空）：列表仍应包含且 note 为空串。
	metaPut(t, p, "uid-bbbb-2222", "", "备用池")
	list := metaList(t, p)
	if len(list.Meta) != 2 {
		t.Fatalf("LIST 条目=%d want 2: %+v", len(list.Meta), list.Meta)
	}
	if rec := list.Meta["uid-bbbb-2222"]; rec.Note != "" || len(rec.Tags) != 1 || rec.Tags[0] != "备用池" {
		t.Fatalf("LIST 第二条=%+v", rec)
	}

	// 磁盘格式：{"version":1,"accounts":{"<完整uid>":{note,tags,updated_at}}}。
	f := readMetaFile(t, dir)
	if f.Version != accountMetaVersion {
		t.Fatalf("version=%d want %d", f.Version, accountMetaVersion)
	}
	disk, ok := f.Accounts["uid-aaaa-1111"]
	if !ok || disk.Note != "采购A-备用" || len(disk.Tags) != 2 || disk.UpdatedAt != put.UpdatedAt {
		t.Fatalf("磁盘记录=%+v ok=%v", disk, ok)
	}
	// 文件权限 0600（Windows 上 Go 的 Perm 语义受限，仅 POSIX 断言）。
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, accountMetaFileName))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("文件权限=%o want 600", fi.Mode().Perm())
		}
	}
	// 原子写不应残留临时文件。
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("原子写残留临时文件: %s", e.Name())
		}
	}
}

// TestAccountMetaOverwrite 同 uid 覆盖写：GET/LIST 均只保留最新值。
func TestAccountMetaOverwrite(t *testing.T) {
	p, _ := newAccountMetaPanel(t)
	first := metaPut(t, p, "u1", "旧备注", "旧标签")
	time.Sleep(2 * time.Millisecond)
	second := metaPut(t, p, "u1", "新备注", "新标签1", "新标签2")
	if second.UpdatedAt < first.UpdatedAt {
		t.Fatalf("覆盖后 updated_at=%d 回退（前 %d）", second.UpdatedAt, first.UpdatedAt)
	}
	got := metaGet(t, p, "u1")
	if got.Note != "新备注" || len(got.Tags) != 2 {
		t.Fatalf("覆盖后 GET=%+v", got)
	}
	list := metaList(t, p)
	if len(list.Meta) != 1 {
		t.Fatalf("LIST 条目=%d want 1", len(list.Meta))
	}
}

// TestAccountMetaClearAndForget 清空（PUT 全空=删除）语义 + forget 清理 + 空 map 稳定。
func TestAccountMetaClearAndForget(t *testing.T) {
	p, dir := newAccountMetaPanel(t)

	metaPut(t, p, "u1", "备注", "标签")
	// PUT 全空 = 清空该条目（等价删除），响应回空值 + updated_at=0。
	cleared := metaPut(t, p, "u1", "", nil...)
	if cleared.Note != "" || len(cleared.Tags) != 0 || cleared.UpdatedAt != 0 {
		t.Fatalf("清空响应=%+v", cleared)
	}
	if got := metaGet(t, p, "u1"); got.Note != "" || len(got.Tags) != 0 || got.UpdatedAt != 0 {
		t.Fatalf("清空后 GET=%+v", got)
	}
	if list := metaList(t, p); len(list.Meta) != 0 {
		t.Fatalf("清空后 LIST=%+v", list.Meta)
	}
	if f := readMetaFile(t, dir); len(f.Accounts) != 0 {
		t.Fatalf("清空后磁盘 accounts=%+v want 空", f.Accounts)
	}

	// forget：删除后 GET/LIST 干净；重复 forget/未知 uid 幂等不报错。
	metaPut(t, p, "u2", "待删除", "x")
	p.accountMetaForget("u2")
	if got := metaGet(t, p, "u2"); got.Note != "" || len(got.Tags) != 0 {
		t.Fatalf("forget 后 GET=%+v", got)
	}
	p.accountMetaForget("u2")        // 幂等
	p.accountMetaForget("not-exist") // 无记录
	p.accountMetaForget("bad/uid")   // 非法 uid：只 WARN，不 panic
	if f := readMetaFile(t, dir); len(f.Accounts) != 0 {
		t.Fatalf("forget 后磁盘 accounts=%+v want 空", f.Accounts)
	}
	// 空 map 序列化稳定：含 "accounts": {}，且再读回是空 map。
	raw, err := os.ReadFile(filepath.Join(dir, accountMetaFileName))
	if err != nil {
		t.Fatalf("读文件: %v", err)
	}
	if !strings.Contains(string(raw), `"accounts": {}`) {
		t.Fatalf("空 map 序列化不稳定:\n%s", raw)
	}
}

// TestAccountMetaValidation 入参校验：非法 uid / note 超长 / tags 超量或超长 / 坏 JSON → 400。
func TestAccountMetaValidation(t *testing.T) {
	p, _ := newAccountMetaPanel(t)

	// 非法 uid（路径参数）：空格、斜杠、超长、空串。
	for _, uid := range []string{"bad uid", "a/b", strings.Repeat("a", 65), ""} {
		rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, uid, `{"note":"x"}`, false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法 uid=%q PUT status=%d want 400 body=%s", uid, rec.Code, rec.Body.String())
		}
		rec = metaCall(t, p, p.accountMetaGet, http.MethodGet, uid, "", false)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("非法 uid=%q GET status=%d want 400", uid, rec.Code)
		}
	}

	// note 超长（按 rune 计）：500 通过、501 拒绝。
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1",
		`{"note":"`+strings.Repeat("测", accountMetaMaxNoteRunes)+`"}`, false); rec.Code != http.StatusOK {
		t.Fatalf("note=500 字符 status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1",
		`{"note":"`+strings.Repeat("测", accountMetaMaxNoteRunes+1)+`"}`, false); rec.Code != http.StatusBadRequest {
		t.Fatalf("note=501 字符 status=%d want 400", rec.Code)
	}

	// tags 超量（11 个不同标签）→ 400；10 个 → 200。
	tags11 := make([]string, 11)
	tags10 := make([]string, 10)
	for i := 0; i < 11; i++ {
		tags11[i] = "tag" + strings.Repeat("x", i+1)
		if i < 10 {
			tags10[i] = tags11[i]
		}
	}
	b11, _ := json.Marshal(map[string]any{"tags": tags11})
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", string(b11), false); rec.Code != http.StatusBadRequest {
		t.Fatalf("11 个标签 status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	b10, _ := json.Marshal(map[string]any{"tags": tags10})
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", string(b10), false); rec.Code != http.StatusOK {
		t.Fatalf("10 个标签 status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}

	// 单个标签超长（33 rune）→ 400；32 rune → 200。
	tag33, _ := json.Marshal(map[string]any{"tags": []string{strings.Repeat("标", accountMetaMaxTagRunes+1)}})
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", string(tag33), false); rec.Code != http.StatusBadRequest {
		t.Fatalf("33 字符标签 status=%d want 400", rec.Code)
	}
	tag32, _ := json.Marshal(map[string]any{"tags": []string{strings.Repeat("标", accountMetaMaxTagRunes)}})
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", string(tag32), false); rec.Code != http.StatusOK {
		t.Fatalf("32 字符标签 status=%d want 200", rec.Code)
	}

	// 坏 JSON body → 400。
	if rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", `not-json`, false); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 JSON status=%d want 400", rec.Code)
	}

	// 去重/去空/去空白：重复与空白标签不计数，最终 2 个；未知字段容忍。
	rec := metaCall(t, p, p.accountMetaPut, http.MethodPut, "u2",
		`{"note":"  前后空白  ","tags":["a","a","","  ","b","a"],"extra":1}`, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("去重场景 status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	var resp metaRecordResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Note != "前后空白" {
		t.Fatalf("note 应去首尾空白，got %q", resp.Note)
	}
	if len(resp.Tags) != 2 || resp.Tags[0] != "a" || resp.Tags[1] != "b" {
		t.Fatalf("tags=%v want [a b]（去重保序）", resp.Tags)
	}
}

// TestAccountMetaMissingFile 无文件/无记录：LIST 返回 meta:{}，GET 返回空值（200 非 404），
// 且只读操作不创建文件（懒加载 + 懒落盘）。
func TestAccountMetaMissingFile(t *testing.T) {
	p, dir := newAccountMetaPanel(t)

	rec := metaCall(t, p, p.accountMetaList, http.MethodGet, "", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIST status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"meta":{}`) {
		t.Fatalf("空 LIST 应序列化为 {}: body=%s", rec.Body.String())
	}

	getRec := metaCall(t, p, p.accountMetaGet, http.MethodGet, "u1", "", false)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET 无记录 status=%d want 200", getRec.Code)
	}
	var got metaRecordResp
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if got.Note != "" || got.UpdatedAt != 0 || len(got.Tags) != 0 {
		t.Fatalf("无记录 GET=%+v", got)
	}
	if !strings.Contains(getRec.Body.String(), `"tags":[]`) {
		t.Fatalf("无记录 tags 应序列化为 []（非 null）: %s", getRec.Body.String())
	}

	if _, err := os.Stat(filepath.Join(dir, accountMetaFileName)); !os.IsNotExist(err) {
		t.Fatalf("只读不应创建 account-meta.json（err=%v）", err)
	}
}

// TestAccountMetaCorruptQuarantine 损坏文件/不支持版本：隔离为 .corrupt-* 后空启动，
// 之后写入恢复正常（不静默覆盖现场）。
func TestAccountMetaCorruptQuarantine(t *testing.T) {
	t.Run("非法 JSON", func(t *testing.T) {
		p, dir := newAccountMetaPanel(t)
		path := filepath.Join(dir, accountMetaFileName)
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatalf("写损坏文件: %v", err)
		}
		list := metaList(t, p)
		if len(list.Meta) != 0 {
			t.Fatalf("损坏文件应按空启动，got %+v", list.Meta)
		}
		if !hasCorruptSibling(t, dir) {
			t.Fatalf("损坏文件应隔离为 .corrupt-*（目录=%v）", dirEntries(t, dir))
		}
		// 隔离后写入正常，磁盘文件可解析。
		metaPut(t, p, "u1", "恢复", "x")
		if f := readMetaFile(t, dir); len(f.Accounts) != 1 || f.Accounts["u1"].Note != "恢复" {
			t.Fatalf("恢复后落盘=%+v", f.Accounts)
		}
	})
	t.Run("不支持的 version", func(t *testing.T) {
		p, dir := newAccountMetaPanel(t)
		path := filepath.Join(dir, accountMetaFileName)
		if err := os.WriteFile(path, []byte(`{"version":99,"accounts":{"u1":{"note":"未来格式"}}}`), 0o600); err != nil {
			t.Fatalf("写未来版本文件: %v", err)
		}
		if list := metaList(t, p); len(list.Meta) != 0 {
			t.Fatalf("version=99 应按损坏隔离，got %+v", list.Meta)
		}
		if !hasCorruptSibling(t, dir) {
			t.Fatalf("应隔离为 .corrupt-*（目录=%v）", dirEntries(t, dir))
		}
	})
}

// TestAccountMetaPersistenceAcrossReload 模拟进程重启：清空包级注册表后，新 Panel
// 从磁盘懒加载同一 data 目录，数据不丢。
func TestAccountMetaPersistenceAcrossReload(t *testing.T) {
	dir := t.TempDir()
	makePanel := func() *Panel {
		pl := pool.New("")
		return New(Config{
			Version: "test", APIKey: accountsTestKey, Pool: pl,
			StateFile: filepath.Join(dir, "state.json"),
		})
	}
	p1 := makePanel()
	metaPut(t, p1, "u1", "持久化备注", "标签A")

	resetAccountMetaStores() // 模拟重启（进程内注册表清空）
	p2 := makePanel()
	got := metaGet(t, p2, "u1")
	if got.Note != "持久化备注" || len(got.Tags) != 1 || got.Tags[0] != "标签A" {
		t.Fatalf("重启后 GET=%+v", got)
	}
}

// TestAccountMetaConcurrent 并发 PUT：mutex 串行、无丢失、落盘始终是合法 JSON。
func TestAccountMetaConcurrent(t *testing.T) {
	p, dir := newAccountMetaPanel(t)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := "uid-" + strings.Repeat("x", i%5) + "-" + string(rune('a'+i))
			body, _ := json.Marshal(map[string]any{"note": "并发", "tags": []string{"t"}})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/panel/api/accounts/"+uid+"/meta", strings.NewReader(string(body)))
			req.SetPathValue("uid", uid)
			p.accountMetaPut(rec, req)
		}(i)
	}
	wg.Wait()
	if f := readMetaFile(t, dir); len(f.Accounts) == 0 || len(f.Accounts) > n {
		t.Fatalf("并发落盘 accounts=%d want 1..%d", len(f.Accounts), n)
	}
	if list := metaList(t, p); len(list.Meta) == 0 || len(list.Meta) > n {
		t.Fatalf("并发 LIST=%d want 1..%d", len(list.Meta), n)
	}

	// 同 uid 并发覆盖：最终值必须是其中一个，且文件可解析、只 1 条。
	var wg2 sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			body, _ := json.Marshal(map[string]any{"note": "备注"})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/panel/api/accounts/same/meta", strings.NewReader(string(body)))
			req.SetPathValue("uid", "same")
			p.accountMetaPut(rec, req)
		}(i)
	}
	wg2.Wait()
	if got := metaGet(t, p, "same"); got.Note != "备注" {
		t.Fatalf("同 uid 并发覆盖后 GET=%+v", got)
	}
	_ = readMetaFile(t, dir) // 可解析
}

// TestAccountMetaDeleteHandler DELETE 便利 handler：幂等 200 并清理记录。
func TestAccountMetaDeleteHandler(t *testing.T) {
	p, _ := newAccountMetaPanel(t)
	metaPut(t, p, "u1", "待删", "x")

	rec := metaCall(t, p, p.accountMetaDelete, http.MethodDelete, "u1", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := metaGet(t, p, "u1"); got.Note != "" || len(got.Tags) != 0 {
		t.Fatalf("DELETE 后 GET=%+v", got)
	}
	// 幂等：重复 DELETE 仍 200。
	rec = metaCall(t, p, p.accountMetaDelete, http.MethodDelete, "u1", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复 DELETE status=%d want 200", rec.Code)
	}
}

// TestAccountMetaNoStorage 未装配数据目录（panel.Config 无 StateFile/ConfigPath）：
// 接口返回 501、forget 安全 no-op（不 panic、不落盘）。
func TestAccountMetaNoStorage(t *testing.T) {
	p := New(Config{Version: "test", APIKey: accountsTestKey, Pool: pool.New("")})
	if p.dataDir != "" {
		t.Fatalf("dataDir=%q want 空", p.dataDir)
	}
	for _, rec := range []*httptest.ResponseRecorder{
		metaCall(t, p, p.accountMetaList, http.MethodGet, "", "", true),
		metaCall(t, p, p.accountMetaGet, http.MethodGet, "u1", "", false),
		metaCall(t, p, p.accountMetaPut, http.MethodPut, "u1", `{"note":"x"}`, false),
	} {
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("未装配存储 status=%d want 501 body=%s", rec.Code, rec.Body.String())
		}
	}
	p.accountMetaForget("u1") // 不 panic
}

// TestNormalizeAccountMeta 归一化纯函数：去空白/去空/去重（保序）、按 rune 限制。
func TestNormalizeAccountMeta(t *testing.T) {
	note, tags, err := normalizeAccountMeta("  备注  ", []string{" a ", "", "a", "b", "  "})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if note != "备注" || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Fatalf("note=%q tags=%v want 备注/[a b]", note, tags)
	}
	if _, _, err := normalizeAccountMeta(strings.Repeat("中", accountMetaMaxNoteRunes+1), nil); err == nil {
		t.Fatal("note 超长应报错")
	}
	if _, _, err := normalizeAccountMeta("", []string{strings.Repeat("中", accountMetaMaxTagRunes+1)}); err == nil {
		t.Fatal("tag 超长应报错")
	}
	tags11 := make([]string, accountMetaMaxTags+1)
	for i := range tags11 {
		tags11[i] = "t" + strings.Repeat("x", i)
	}
	if _, _, err := normalizeAccountMeta("", tags11); err == nil {
		t.Fatal("tags 超量应报错")
	}
}

// TestAccountMetaRoutesWiring 模拟 panel.go（W2-A）的注册方式：四个 handler 可直接作为
// http.HandlerFunc、{uid} 由 ServeMux 注入、withAuth 生效。正式路由仍由 backend-b 注册。
func TestAccountMetaRoutesWiring(t *testing.T) {
	p, _ := newAccountMetaPanel(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /panel/api/meta", p.withAuth(p.accountMetaList))
	mux.HandleFunc("GET /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaGet))
	mux.HandleFunc("PUT /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaPut))
	mux.HandleFunc("DELETE /panel/api/accounts/{uid}/meta", p.withAuth(p.accountMetaDelete))

	do := func(method, path, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// 鉴权：无 key → 401。
	if rec := do(http.MethodGet, "/panel/api/meta", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 key LIST status=%d want 401", rec.Code)
	}
	// PUT → GET → LIST → DELETE 全链路。
	if rec := do(http.MethodPut, "/panel/api/accounts/u1/meta", `{"note":"备注","tags":["a"]}`, accountsTestKey); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/panel/api/accounts/u1/meta", "", accountsTestKey); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"note":"备注"`) {
		t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/panel/api/meta", "", accountsTestKey); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"u1"`) {
		t.Fatalf("LIST status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodDelete, "/panel/api/accounts/u1/meta", "", accountsTestKey); rec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/panel/api/meta", "", accountsTestKey); strings.Contains(rec.Body.String(), `"u1"`) {
		t.Fatalf("DELETE 后 LIST 仍含 u1: %s", rec.Body.String())
	}
	// 路径里的非法 uid（URL 解码后含空格）→ 400。
	if rec := do(http.MethodGet, "/panel/api/accounts/bad%20uid/meta", "", accountsTestKey); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 uid status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
}

// hasCorruptSibling 目录下是否存在 account-meta.json.corrupt-* 隔离文件。
func hasCorruptSibling(t *testing.T, dir string) bool {
	t.Helper()
	for _, name := range dirEntries(t, dir) {
		if strings.Contains(name, accountMetaFileName+".corrupt-") {
			return true
		}
	}
	return false
}

// dirEntries 目录项名列表（读失败视为空）。
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
