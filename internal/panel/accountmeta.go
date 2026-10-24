// accountmeta.go 账号备注/标签的服务端持久化（v2.3.0 #12；v2.3.1 增加标签索引/筛选）。
//
// 背景：备注此前只存在浏览器 localStorage（键还是 uid 前 8 位，跨浏览器/清缓存即丢、
// 前缀还有极低概率碰撞）。本文件把「备注 + 标签」提升为服务端权威数据：
//   - 落盘 <data>/account-meta.json（与 state 文件同目录；0600、tmp+rename 原子写、
//     互斥串行、首次使用时懒加载；文件缺失=空，损坏=隔离 .corrupt-* 后空启动）；
//   - 键是**完整 uid**（与 auths/、state.json 同源，不参与选号/计费/鉴权）；
//   - 限制：note ≤500 字符、tags ≤10 个且每个 ≤32 字符（按 rune 计），去空白/去重；
//   - 内存标签反向索引（小写标签 → uid 集合，加载/变更后整体重建）：供
//     accounts?tag= 服务端筛选与 GET /panel/api/meta/tags 计数复用，筛选不读盘。
//
// 设计约束（不改 panel.go 的 Panel struct）：
//   - Panel 已有 dataDir 字段（v2.2.0 审计/任务历史同源），本文件直接复用；
//   - 存储对象用包级注册表按 dataDir 懒加载单例（多个 Panel 共享同一数据目录时天然
//     共享内存状态；不同目录/测试临时目录互不干扰），因此无需给 Panel struct 加字段。
//
// 接口（路由在 panel.go routes() 注册，本文件只提供 handler/方法）：
//   - p.accountMetaList(w,r)           GET    /panel/api/meta
//   - p.accountMetaGet(w,r)            GET    /panel/api/accounts/{uid}/meta
//   - p.accountMetaPut(w,r)            PUT    /panel/api/accounts/{uid}/meta
//   - p.accountMetaDelete(w,r)         DELETE /panel/api/accounts/{uid}/meta（便利 handler）
//   - p.accountMetaTags(w,r)           GET    /panel/api/meta/tags（v2.3.1 标签索引）
//   - p.accountMetaForget(uid)         账号移除后清理由 accountRemove 调用（无 HTTP 路由）
//   - p.accountMetaMatchesTag / accountMetaUIDsForTag / accountMetaTagCounts
//     panel 侧 accounts?tag= 筛选与计数用的只读查询 helper。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// accountMetaFileName 与 state 文件同目录的元数据文件名。
	accountMetaFileName = "account-meta.json"
	// accountMetaVersion 磁盘格式版本；>1 视为「未来版本/不可识别」，按损坏隔离。
	accountMetaVersion = 1
	// accountMetaBodyMax PUT body 上限（note 500 字符 + 10 个标签足够，64KB 兜底）。
	accountMetaBodyMax = 64 << 10
	// 字段限制（按 rune 计，避免中文按字节被误判）。
	accountMetaMaxNoteRunes = 500
	accountMetaMaxTags      = 10
	accountMetaMaxTagRunes  = 32
)

// accountMetaSeq 账号元数据的内存代际号（v2.5.0 Perf-3）：每次元数据变更
// （PUT/DELETE 落盘成功后的标签索引重建）自增，供 /panel/api/accounts 的 ETag
// 缓存判断「tag 过滤结果可能已变化」。进程内计数，重启归零不影响正确性。
var accountMetaSeq atomic.Uint64

// accountMetaVersionNow 返回当前元数据代际号（无锁；ETag 缓存键/失效判断用）。
func accountMetaVersionNow() uint64 { return accountMetaSeq.Load() }

// accountMetaRecord 单个账号的持久化记录（磁盘格式；空值省略让 JSON 紧凑）。
type accountMetaRecord struct {
	Note      string   `json:"note,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt int64    `json:"updated_at,omitempty"` // unix 秒
}

// accountMetaFile account-meta.json 顶层磁盘格式。
type accountMetaFile struct {
	Version int `json:"version"`
	// Accounts 键为**完整 uid**（白名单 [A-Za-z0-9_-]{1,64}）。
	Accounts map[string]accountMetaRecord `json:"accounts"`
}

// accountMetaView API 响应里的单条元数据：字段恒定出现（无记录时 note=""、tags=[]、
// updated_at=0），前端可无条件读取，不必做 null 兜底。
type accountMetaView struct {
	Note      string   `json:"note"`
	Tags      []string `json:"tags"`
	UpdatedAt int64    `json:"updated_at"`
}

// metaView 记录 → 响应视图（Tags 保证是 [] 而不是 null）。
func metaView(rec accountMetaRecord) accountMetaView {
	tags := rec.Tags
	if tags == nil {
		tags = []string{}
	}
	return accountMetaView{Note: rec.Note, Tags: tags, UpdatedAt: rec.UpdatedAt}
}

// accountMetaStore 单个 data 目录的 account-meta.json 访问器（并发安全、懒加载）。
type accountMetaStore struct {
	mu       sync.Mutex
	path     string
	loaded   bool
	accounts map[string]accountMetaRecord // 懒加载后的内存权威副本
	// 标签反向索引（v2.3.1 #12 扩展）：小写标签 → uid 集合；变更/加载后整体重建。
	// 供 accounts?tag= 服务端筛选与 /panel/api/meta/tags 计数复用，避免每次筛全量读盘。
	tagIndex map[string]map[string]struct{}
	// tagDisplay 小写标签 → 展示用原始拼写（稳定：按 uid 排序后首个出现的拼写）。
	tagDisplay map[string]string
}

// 包级注册表：dataDir → *accountMetaStore。键为空（未装配数据目录）时返回 nil。
var (
	accountMetaStoresMu sync.Mutex
	accountMetaStores   = map[string]*accountMetaStore{}
)

// accountMetaStoreFor 返回 dataDir 对应的存储单例；dataDir 为空 = 未装配，返回 nil。
// 多个 Panel 使用同一数据目录时共享同一 store（内存状态一致）；测试用 t.TempDir 隔离。
func accountMetaStoreFor(dataDir string) *accountMetaStore {
	if dataDir == "" {
		return nil
	}
	accountMetaStoresMu.Lock()
	defer accountMetaStoresMu.Unlock()
	if s := accountMetaStores[dataDir]; s != nil {
		return s
	}
	s := &accountMetaStore{path: filepath.Join(dataDir, accountMetaFileName)}
	accountMetaStores[dataDir] = s
	return s
}

// resetAccountMetaStores 清空包级注册表（**仅测试用**：模拟进程重启，验证磁盘
// 持久化；生产代码不要调用）。
func resetAccountMetaStores() {
	accountMetaStoresMu.Lock()
	accountMetaStores = map[string]*accountMetaStore{}
	accountMetaStoresMu.Unlock()
}

// loadLocked 懒加载磁盘内容（调用方须持 s.mu）。文件缺失=空；损坏=隔离后空启动。
func (s *accountMetaStore) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.accounts = map[string]accountMetaRecord{}
	s.tagIndex = map[string]map[string]struct{}{}
	s.tagDisplay = map[string]string{}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("WARN: [panel] 读取账号元数据失败（%s）：%v；按空处理", s.path, err)
		}
		return
	}
	var f accountMetaFile
	if err := json.Unmarshal(raw, &f); err != nil {
		s.quarantineLocked("JSON 解析失败: " + err.Error())
		return
	}
	if f.Version != 0 && f.Version != accountMetaVersion {
		s.quarantineLocked(fmt.Sprintf("不支持的 version=%d", f.Version))
		return
	}
	if f.Accounts != nil {
		s.accounts = f.Accounts
	}
	// 过滤手改文件注入的脏数据：非法 uid / 空记录（空记录等价于不存在）。
	for uid, rec := range s.accounts {
		if !validUID(uid) || (rec.Note == "" && len(rec.Tags) == 0) {
			delete(s.accounts, uid)
		}
	}
	s.rebuildTagIndexLocked()
}

// rebuildTagIndexLocked 从内存 records 重建标签反向索引（调用方须持 s.mu）。
// 标签匹配统一按「TrimSpace + 小写」归一（大小写不敏感）；展示拼写取 uid 升序遍历下
// 首个出现值——保证同一份数据多次查询输出稳定（map 遍历随机，不能用来定拼写）。
func (s *accountMetaStore) rebuildTagIndexLocked() {
	s.tagIndex = map[string]map[string]struct{}{}
	s.tagDisplay = map[string]string{}
	uids := make([]string, 0, len(s.accounts))
	for uid := range s.accounts {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		for _, tag := range s.accounts[uid].Tags {
			display := strings.TrimSpace(tag)
			key := strings.ToLower(display)
			if key == "" {
				continue
			}
			set := s.tagIndex[key]
			if set == nil {
				set = map[string]struct{}{}
				s.tagIndex[key] = set
			}
			set[uid] = struct{}{}
			if _, ok := s.tagDisplay[key]; !ok {
				s.tagDisplay[key] = display
			}
		}
	}
	// 每次索引重建都视为元数据内容发生变更（含 note-only 变更，属保守过度失效）：
	// 自增代际号，让 accounts 的 ETag 缓存下次请求重新计算。
	accountMetaSeq.Add(1)
}

// quarantineLocked 把损坏文件改名隔离为 <name>.corrupt-<unixnano>，保留现场供人工恢复，
// 然后以空数据启动（下次写入生成新文件）。隔离失败只 WARN，不阻塞功能。
func (s *accountMetaStore) quarantineLocked(reason string) {
	target := s.path + fmt.Sprintf(".corrupt-%d", time.Now().UnixNano())
	if err := os.Rename(s.path, target); err != nil {
		log.Printf("WARN: [panel] 账号元数据损坏（%s），隔离失败：%v；按空处理（后续写入会覆盖）", reason, err)
		return
	}
	log.Printf("WARN: [panel] 账号元数据损坏（%s），已隔离为 %s 并按空启动", reason, target)
}

// snapshot 返回全部记录的深拷贝（仅非空条目；空 map 非 nil）。
func (s *accountMetaStore) snapshot() map[string]accountMetaRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make(map[string]accountMetaRecord, len(s.accounts))
	for uid, rec := range s.accounts {
		if rec.Note == "" && len(rec.Tags) == 0 {
			continue
		}
		if rec.Tags != nil {
			rec.Tags = append([]string(nil), rec.Tags...)
		}
		out[uid] = rec
	}
	return out
}

// get 读取单账号记录（无记录返回零值,false）。
func (s *accountMetaStore) get(uid string) (accountMetaRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	rec, ok := s.accounts[uid]
	if rec.Tags != nil {
		rec.Tags = append([]string(nil), rec.Tags...)
	}
	return rec, ok
}

// put 覆盖写入单账号记录并原子落盘；note/tags 全空 = 删除该条目（保持文件不膨胀）。
// 落盘失败时回滚内存副本并返回错误，避免内存与磁盘不一致。
func (s *accountMetaStore) put(uid string, rec accountMetaRecord) (accountMetaRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	prev, had := s.accounts[uid]
	effective := rec
	if rec.Note == "" && len(rec.Tags) == 0 {
		delete(s.accounts, uid)
		effective = accountMetaRecord{} // 清空即无记录：响应 updated_at=0
	} else {
		s.accounts[uid] = rec
	}
	if err := s.saveLocked(); err != nil {
		if had {
			s.accounts[uid] = prev
		} else {
			delete(s.accounts, uid)
		}
		return accountMetaRecord{}, err
	}
	s.rebuildTagIndexLocked() // 落盘成功才更新标签索引：与内存/磁盘保持同一版本
	if effective.Tags != nil {
		effective.Tags = append([]string(nil), effective.Tags...)
	}
	return effective, nil
}

// remove 删除单账号记录（幂等：无记录不写盘）；落盘失败回滚内存。
func (s *accountMetaStore) remove(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()

	prev, had := s.accounts[uid]
	if !had {
		return nil
	}
	delete(s.accounts, uid)
	if len(s.accounts) == 0 {
		s.accounts = map[string]accountMetaRecord{} // 清理空 map，保持落盘 {"accounts":{}}
	}
	if err := s.saveLocked(); err != nil {
		s.accounts[uid] = prev
		return err
	}
	s.rebuildTagIndexLocked() // 落盘成功才更新标签索引
	return nil
}

// saveLocked 原子落盘（同目录 tmp 文件 + rename，0600；fsync 后改名保证不产生半截文件）。
// 调用方须持 s.mu。JSON 里 map 键由 encoding/json 排序，输出稳定可 diff。
func (s *accountMetaStore) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	accounts := s.accounts
	if accounts == nil {
		accounts = map[string]accountMetaRecord{}
	}
	raw, err := json.MarshalIndent(accountMetaFile{Version: accountMetaVersion, Accounts: accounts}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp, err := os.CreateTemp(dir, ".account-meta-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 Remove 失败无妨
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // 与 #13 state 可靠性同精神：rename 前落盘
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// normalizeAccountMeta 校验并归一化 PUT 入参：
//   - note：去首尾空白，≤500 字符；
//   - tags：逐个去空白、去空、去重（保序），每个 ≤32 字符，最终 ≤10 个。
//
// 返回校验失败原因（外层映射 400）。
func normalizeAccountMeta(note string, tags []string) (string, []string, error) {
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > accountMetaMaxNoteRunes {
		return "", nil, fmt.Errorf("备注超长（最多 %d 字符）", accountMetaMaxNoteRunes)
	}
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > accountMetaMaxTagRunes {
			return "", nil, fmt.Errorf("标签 %q 超长（每个最多 %d 字符）", tag, accountMetaMaxTagRunes)
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > accountMetaMaxTags {
		return "", nil, fmt.Errorf("标签过多（最多 %d 个，去重后 %d 个）", accountMetaMaxTags, len(out))
	}
	return note, out, nil
}

// writeAccountMeta 输出单账号元数据响应（字段恒定：note/tags/updated_at）。
func writeAccountMeta(w http.ResponseWriter, uid string, rec accountMetaRecord) {
	v := metaView(rec)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "uid": uid, "note": v.Note, "tags": v.Tags, "updated_at": v.UpdatedAt,
	})
}

// accountMetaList GET /panel/api/meta：全量备注/标签（仅非空条目；无记录时 meta:{}）。
// 路由由 panel.go routes() 注册；本方法只依赖 p.dataDir（未装配数据目录 → 501）。
func (p *Panel) accountMetaList(w http.ResponseWriter, r *http.Request) {
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		writeErr(w, http.StatusNotImplemented, "account meta storage not configured (panel.StateFile/ConfigPath)")
		return
	}
	meta := make(map[string]accountMetaView)
	for uid, rec := range store.snapshot() {
		meta[uid] = metaView(rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "meta": meta})
}

// accountMetaGet GET /panel/api/accounts/{uid}/meta：单账号备注/标签。
// 无记录返回 200 + 空 note/tags（不是 404：备注是可选覆盖层，前端可无条件渲染）。
func (p *Panel) accountMetaGet(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid")
		return
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		writeErr(w, http.StatusNotImplemented, "account meta storage not configured (panel.StateFile/ConfigPath)")
		return
	}
	rec, _ := store.get(uid)
	writeAccountMeta(w, uid, rec)
}

// accountMetaPut PUT /panel/api/accounts/{uid}/meta：覆盖保存 body {note,tags}。
// 校验失败 400；写盘失败 500；note/tags 全空 = 清空（等价删除）。
func (p *Panel) accountMetaPut(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid")
		return
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		writeErr(w, http.StatusNotImplemented, "account meta storage not configured (panel.StateFile/ConfigPath)")
		return
	}
	var body struct {
		Note string   `json:"note"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, accountMetaBodyMax)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	note, tags, err := normalizeAccountMeta(body.Note, body.Tags)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := store.put(uid, accountMetaRecord{Note: note, Tags: tags, UpdatedAt: time.Now().Unix()})
	if err != nil {
		log.Printf("WARN: [panel] 保存账号元数据失败 uid=%s: %v", uid, err)
		writeErr(w, http.StatusInternalServerError, "save account meta: "+err.Error())
		return
	}
	writeAccountMeta(w, uid, saved)
}

// accountMetaDelete DELETE /panel/api/accounts/{uid}/meta：删除单账号备注/标签（幂等 200）。
// 便利 handler：路由注册方也可用 p.accountMetaForget(uid) + 自写响应。
func (p *Panel) accountMetaDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid")
		return
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		writeErr(w, http.StatusNotImplemented, "account meta storage not configured (panel.StateFile/ConfigPath)")
		return
	}
	if err := store.remove(uid); err != nil {
		log.Printf("WARN: [panel] 删除账号元数据失败 uid=%s: %v", uid, err)
		writeErr(w, http.StatusInternalServerError, "remove account meta: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
}

// accountMetaForget 账号移除后清理其备注/标签（accountRemove 成功后调用）。
// 无记录/未装配存储时为空操作；失败只 WARN（账号已删除，元数据残留不应阻塞移除流程）。
func (p *Panel) accountMetaForget(uid string) {
	if !validUID(uid) {
		log.Printf("WARN: [panel] accountMetaForget 跳过非法 uid=%q", uid)
		return
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		return
	}
	if err := store.remove(uid); err != nil {
		log.Printf("WARN: [panel] 清理账号元数据失败 uid=%s: %v（账号已移除，元数据残留）", uid, err)
	}
}

// ---------------------------------------------------------------------------
// 标签索引/筛选（v2.3.1 #12 扩展）
// ---------------------------------------------------------------------------

// accountMetaTagCount 标签索引的一行（GET /panel/api/meta/tags 响应元素）。
type accountMetaTagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// accountMetaMatchesTag 单个账号是否带指定标签（大小写不敏感、TrimSpace；O(tags)）。
// 未装配存储/空标签/未知账号 → false。上千~上万账号批量筛选请用
// accountMetaUIDsForTag：一次加锁查反向索引，避免逐号加锁遍历。
func (p *Panel) accountMetaMatchesTag(uid, tag string) bool {
	key := strings.ToLower(strings.TrimSpace(tag))
	if uid == "" || key == "" {
		return false
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		return false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loadLocked()
	for _, t := range store.accounts[uid].Tags {
		if strings.ToLower(strings.TrimSpace(t)) == key {
			return true
		}
	}
	return false
}

// accountMetaUIDsForTag 返回带指定标签的账号 uid 集合（只读副本；一次加锁 + 索引查表）。
// 供 accounts?tag= 服务端筛选使用：上万账号场景下避免逐号加锁/遍历元数据。
// 未装配存储或空标签 → nil（调用方按「不筛选」语义处理）；标签无匹配 → 空 map。
func (p *Panel) accountMetaUIDsForTag(tag string) map[string]struct{} {
	key := strings.ToLower(strings.TrimSpace(tag))
	if key == "" {
		return nil
	}
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loadLocked()
	set := store.tagIndex[key]
	out := make(map[string]struct{}, len(set))
	for uid := range set {
		out[uid] = struct{}{}
	}
	return out
}

// accountMetaTagCounts 返回标签索引：{tag,count} 按 count 降序、同 count 按 tag 字典序。
// 数据来自内存反向索引（加载/变更后整体重建，不读盘）；只统计非空标签；
// 未装配存储 → 空切片（调用方序列化为 []）。
func (p *Panel) accountMetaTagCounts() []accountMetaTagCount {
	store := accountMetaStoreFor(p.dataDir)
	if store == nil {
		return []accountMetaTagCount{}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.loadLocked()
	out := make([]accountMetaTagCount, 0, len(store.tagIndex))
	for key, set := range store.tagIndex {
		if len(set) == 0 {
			continue
		}
		out = append(out, accountMetaTagCount{Tag: store.tagDisplay[key], Count: len(set)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Tag < out[j].Tag
	})
	return out
}

// accountMetaTags GET /panel/api/meta/tags：标签索引（count 降序、同 count 字典序）。
// 未装配数据目录时返回 200 + 空数组（与 accounts?tag= 的「无匹配返回空 rows」语义一致）。
func (p *Panel) accountMetaTags(w http.ResponseWriter, r *http.Request) {
	tags := p.accountMetaTagCounts()
	if tags == nil {
		tags = []accountMetaTagCount{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tags": tags})
}
