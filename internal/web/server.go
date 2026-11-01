// Package web Dashboard：内嵌前端 + JSON API + 实时 SSE。
// 与网关共用端口（网关挂 /v1/*，dashboard 挂 / 与 /api/*，互不冲突）。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ai-trace/internal/adapter"
	"ai-trace/internal/analytics"
	"ai-trace/internal/capture"
	"ai-trace/internal/collector"
	"ai-trace/internal/live"
	"ai-trace/internal/skills"
	"ai-trace/internal/store"
)

//go:embed static
var staticFS embed.FS

// Handler dashboard 的 HTTP 处理器。col 可为 nil（无日志数据源时），
// hub 可为 nil（无实时推送，/api/live 返回 503）。
// dataDir 为数据目录（capture 开关的代理备份所在），空串则不注册 /api/capture。
func Handler(st *store.Store, col *collector.Collector, pt *analytics.PriceTable, hub *live.Hub, dataDir string) http.Handler {
	mux := http.NewServeMux()

	// ---- capture：系统代理 MITM 模式开关（桌面客户端的 UI 按钮用）----
	if dataDir != "" {
		mux.HandleFunc("GET /api/capture", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, capture.Read())
		})
		mux.HandleFunc("POST /api/capture", func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Action string `json:"action"` // "on" | "off"
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Action != "on" && in.Action != "off") {
				http.Error(w, `{"error":"action 必须是 on 或 off"}`, http.StatusBadRequest)
				return
			}
			if in.Action == "on" {
				certPath, err := capture.On(dataDir)
				if err != nil {
					writeJSON(w, map[string]any{"error": err.Error()})
					return
				}
				writeJSON(w, map[string]any{"on": true, "cert": certPath})
				return
			}
			restored, err := capture.Off(dataDir)
			if err != nil {
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"on": false, "restored": restored})
		})
	}

	// ---- 实时推送（SSE）：正在录制的请求逐字出现 ----
	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		if hub == nil {
			http.Error(w, "实时推送未启用", http.StatusServiceUnavailable)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "需要流式响应支持", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()

		ch, cancel := hub.Subscribe()
		defer cancel()
		for {
			select {
			case <-r.Context().Done():
				return
			case m, ok := <-ch:
				if !ok {
					return
				}
				b, err := json.Marshal(m)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				flusher.Flush()
			}
		}
	})

	// ---- 统计（days 可调：1=今天，默认 30，0=全部） ----
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		days := 30
		if v := r.URL.Query().Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				days = n
			}
		}
		s := analytics.Analyze(st, pt, days)
		writeJSON(w, s)
	})

	// ---- 会话（入口 C：本地日志解析） ----
	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		if col == nil {
			writeJSON(w, map[string]any{"sessions": []any{}})
			return
		}
		writeJSON(w, map[string]any{"sessions": col.Sessions()})
	})

	mux.HandleFunc("GET /api/sessions/{agent}/{id}", func(w http.ResponseWriter, r *http.Request) {
		if col == nil {
			writeJSON(w, map[string]any{"error": "日志采集未启用"})
			return
		}
		s, ok := col.Session(r.PathValue("agent"), r.PathValue("id"))
		if !ok {
			writeJSON(w, map[string]any{"error": "会话不存在"})
			return
		}
		writeJSON(w, s)
	})

	// ---- 流量会话（从录制账本按 session 分组合成，覆盖全部被录制 agent） ----
	mux.HandleFunc("GET /api/tsessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sessions": TrafficSessions(st)})
	})
	mux.HandleFunc("GET /api/tsessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		ts, ok := TrafficSessionDetail(st, r.PathValue("id"))
		if !ok {
			writeJSON(w, map[string]any{"error": "会话不存在"})
			return
		}
		writeJSON(w, map[string]any{"session": ts, "turns": ts.turns})
	})

	// ---- 用户请求轮次：会话内一次用户输入触发的 N 次模型调用 ----
	// day 必填语义同 /api/records（空 = 今天）；agent/status/q 可选过滤
	mux.HandleFunc("GET /api/turns", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		day := q.Get("day")
		turns := TurnsForDay(st, day)
		turns = filterTurns(turns, q.Get("agent"), q.Get("status"), q.Get("q"))
		writeJSON(w, map[string]any{"turns": turns, "total": len(turns)})
	})
	mux.HandleFunc("GET /api/turns/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, calls, ok := TurnDetail(st, r.PathValue("id"))
		if !ok {
			writeJSON(w, map[string]any{"error": "轮次不存在"})
			return
		}
		writeJSON(w, map[string]any{"turn": t, "calls": calls})
	})

	mux.HandleFunc("GET /api/days", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"days": st.Days()})
	})

	// ---- 记录列表：day 必填（账本按天分文件），agent/session/model/status 可选过滤 ----
	mux.HandleFunc("GET /api/records", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		recs, err := st.Records(q.Get("day"))
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		// 服务端过滤（前端也可做，这里做省流量）
		recs = filterRecords(recs,
			q.Get("agent"), q.Get("session"), q.Get("model"), q.Get("status"))
		total := len(recs)
		if len(recs) > 500 {
			recs = recs[:500] // 单日过多时截断，total 一并下发供前端提示
		}
		writeJSON(w, map[string]any{"records": recs, "total": total})
	})

	// ---- 清理杂流量：删除指定 agent 的全部记录（典型：MITM 误录的 unknown）----
	mux.HandleFunc("POST /api/records/purge", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Agent string `json:"agent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Agent == "" {
			http.Error(w, `{"error":"agent 必填"}`, http.StatusBadRequest)
			return
		}
		n, err := st.DeleteBy(func(rec store.Record) bool { return rec.Agent == in.Agent })
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "deleted": n})
			return
		}
		writeJSON(w, map[string]any{"deleted": n})
	})

	// ---- 一键清空：只删录制数据（账本/请求体及历史遗留备份），
	// 技能沉淀、配置、CA 证书、代理备份等一律保留，重头再来 ----
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) {
		coreDirs := map[string]bool{"traces": true, "bodies": true}
		deleted := map[string]int{}
		targets := []string{"traces", "bodies",
			"traces.bak-before-purge", // 早期清理流程留下的旧账本备份
			"mitm-debug.log"}          // 旧版调试日志
		for _, sub := range targets {
			root := filepath.Join(dataDir, sub)
			n := 0
			filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					n++
				}
				return nil
			})
			if err := os.RemoveAll(root); err != nil {
				continue
			}
			// 两个核心目录必须重建：store 只在 Open 时建目录，运行中追加不会再建
			if coreDirs[sub] {
				os.MkdirAll(root, 0o755)
			}
			if n > 0 {
				deleted[sub] = n
			}
		}
		writeJSON(w, map[string]any{"ok": true, "deleted": deleted})
	})

	mux.HandleFunc("GET /api/records/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		rec, body, err := recordWithBody(st, id)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"record": rec, "body": body})
	})

	mux.HandleFunc("GET /api/adapters", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"installed": adapter.Installed(),
			"all":       adapter.Registry(),
		})
	})

	// ---- 技能沉淀（<dataDir>/skills/ 的只读视图） ----
	mux.HandleFunc("GET /api/skills", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"skills": skills.List(dataDir)})
	})
	mux.HandleFunc("GET /api/skills/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		content, history, ok := skills.Detail(dataDir, name)
		if !ok {
			writeJSON(w, map[string]any{"error": "技能不存在"})
			return
		}
		writeJSON(w, map[string]any{"name": name, "content": content, "history": history})
	})
	// 目录树：本地技能包（SKILL.md/references/scripts/assets…）
	mux.HandleFunc("GET /api/skills/{name}/tree", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		entries, source, hasLocal := skills.Tree(dataDir, name)
		if !hasLocal {
			// 无本地目录：把流量沉淀的正文当作单文件 SKILL.md
			entries = []skills.Entry{{Name: "SKILL.md", Path: "SKILL.md"}}
		}
		writeJSON(w, map[string]any{"name": name, "source": source, "hasLocal": hasLocal, "entries": entries})
	})
	// 文件内容：?p=<相对路径> 文本/元信息 JSON；&raw=1 原始字节（图片内联预览用）
	mux.HandleFunc("GET /api/skills/{name}/file", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		rel := r.URL.Query().Get("p")
		data, kind, size, ok := skills.ReadFile(dataDir, name, rel)
		if !ok {
			http.Error(w, `{"error":"文件不存在"}`, http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("raw") == "1" {
			ct := "application/octet-stream"
			switch kind {
			case skills.KindImage:
				ct = "image/" + strings.TrimPrefix(strings.ToLower(filepath.Ext(rel)), ".")
			case skills.KindText:
				ct = "text/plain; charset=utf-8"
			}
			w.Header().Set("Content-Type", ct)
			w.Write(data)
			return
		}
		content := ""
		if kind == skills.KindText {
			content = string(data)
		}
		writeJSON(w, map[string]any{"path": rel, "kind": kind, "size": size, "content": content})
	})
	// 一键导出技能包（zip 下载）
	mux.HandleFunc("GET /api/skills/{name}/export", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition",
			`attachment; filename="`+strings.ReplaceAll(name, "/", "-")+`-skill.zip"`)
		n, err := skills.ExportZip(dataDir, name, w)
		if err != nil || n == 0 {
			// 头已发出，只能断开连接
			return
		}
	})

	// ---- 静态前端（目录 embed，根路径回退 index.html 供前端路由） ----
	sub, _ := fs.Sub(staticFS, "static")
	fileServer := http.FileServer(http.FS(sub))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 先按真实路径找（/app.css、/app.js…），找不到且不是 /api 则回退 index.html
		if r.URL.Path != "/" {
			if _, err := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		data, _ := staticFS.ReadFile("static/index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})

	// API 响应禁止缓存：本地 dashboard 常驻进程重启后数据会变，
	// WKWebView 等会对无 Cache-Control 的 GET 做启发式缓存，导致
	// 界面显示旧数据（技能/轮次列表凭空多出已删除条目）。
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		mux.ServeHTTP(w, r)
	})
}

// filterRecords 按可选条件过滤（空串不过滤）。status 支持 "err" 特殊值（错误记录）。
func filterRecords(recs []store.Record, agent, session, model, status string) []store.Record {
	out := make([]store.Record, 0, len(recs))
	for _, rec := range recs {
		if agent != "" && rec.Agent != agent {
			continue
		}
		if session != "" && rec.Session != session {
			continue
		}
		if model != "" && rec.Model != model {
			continue
		}
		if status == "err" {
			if rec.Error == "" && rec.Status >= 200 && rec.Status < 400 {
				continue
			}
		} else if status != "" {
			if strconv.Itoa(rec.Status) != status {
				continue
			}
		}
		out = append(out, rec)
	}
	return out
}

// recordWithBody 在账本里找到该 id 的记录（可能要翻几天），并加载 body。
func recordWithBody(st *store.Store, id string) (*store.Record, *store.FullBody, error) {
	// 记录 id 以日期开头（20260117-...），直接推算账本文件
	day := ""
	if len(id) >= 8 {
		s := id[:8]
		if validDay(s) {
			day = s[:4] + "-" + s[4:6] + "-" + s[6:8]
		}
	}
	var days []string
	if day != "" {
		days = []string{day}
	} else {
		days = st.Days()
	}
	for _, d := range days {
		recs, err := st.Records(d)
		if err != nil {
			continue
		}
		for i := range recs {
			if recs[i].ID == id {
				body, err := st.Body(id)
				if err != nil {
					return &recs[i], nil, nil
				}
				return &recs[i], body, nil
			}
		}
	}
	return nil, nil, errNotFound(id)
}

func validDay(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type notFoundErr string

func (e notFoundErr) Error() string { return "记录不存在: " + string(e) }
func errNotFound(id string) error   { return notFoundErr(id) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		// 客户端断开属正常，其他编码错误无处可写，忽略
		_ = err
	}
}
