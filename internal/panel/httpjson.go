// httpjson.go 面板 JSON 响应/错误/请求体解码统一出口（v2.6.1 从 panel.go /
// helpers.go 抽出，行为不变）。
//
// 约定：所有 JSON handler 一律走 writeJSON/writeJSONStatus/writeErr/readJSONBody，
// 禁止裸 w.Write 写 JSON；错误响应形状固定 {ok:false,error}；writeJSON 序列化
// 失败统一 500（v2.6.0 P1）；POST body 默认 1MB 上限（readJSONBody，超限 413）。
package panel

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

// panelJSONBodyMax 常规面板 POST JSON body 上限（1MB）：tasks/autotask/
// taskcenter 等小 body 接口共用；import 走单独的 32MB 上限。
const panelJSONBodyMax = 1 << 20

// writeJSON 写 JSON 响应（统一出口）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		// v2.6.0 P1：绝不写半截/空 body。序列化失败（如上游脏数据里的 NaN/Inf）
		// 记日志后回 500 + 固定错误形状，前端能拿到可读原因、审计中间件也能解析。
		log.Printf("WARN: [panel] 响应 JSON 序列化失败: %v", err)
		raw = []byte(`{"ok":false,"error":"encode response failed"}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeJSONStatus writeJSON 的显式状态码别名（handler 可读性；行为与 writeJSON
// 完全一致）：新 handler 用 writeJSONStatus(w, http.StatusOK, body) 或 writeErr。
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

// writeErr 统一错误响应：{ok:false,error:msg}（状态码语义由调用方决定，保持不变）。
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
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
