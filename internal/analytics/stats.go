package analytics

import (
	"sort"

	"ai-trace/internal/store"
)

// Bucket 一个聚合分组（按模型 / 按 agent / 按日）。
type Bucket struct {
	Key       string  `json:"key"`
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	Input     int64   `json:"input"`
	Output    int64   `json:"output"`
	CacheRead int64   `json:"cacheRead"`
	Tools     int     `json:"tools"`
	CostUSD   float64 `json:"costUsd"`
	HasPrice  bool    `json:"hasPrice"` // false = 该模型无价格，成本未计
}

// ToolStat 工具调用统计。
type ToolStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Stats 汇总结果。
type Stats struct {
	Days    []string  `json:"days"`    // 统计覆盖的日期（旧在前）
	Totals  Bucket    `json:"totals"`
	ByModel []Bucket  `json:"byModel"`
	ByAgent []Bucket  `json:"byAgent"`
	ByDay   []Bucket  `json:"byDay"`
	Tools   []ToolStat `json:"tools"`
}

// Analyze 聚合最近 N 天的账本数据（lastNDays <= 0 表示全部，与 /api/stats 的 days=0 语义一致）。
func Analyze(st *store.Store, pt *PriceTable, lastNDays int) *Stats {
	days := st.Days()
	if lastNDays > 0 && len(days) > lastNDays {
		days = days[:lastNDays]
	}
	s := &Stats{Days: days}
	models := map[string]*Bucket{}
	agents := map[string]*Bucket{}
	perDay := map[string]*Bucket{}
	tools := map[string]int{}

	// 日期旧在前（Days 返回新在前）
	for i := len(days) - 1; i >= 0; i-- {
		day := days[i]
		recs, err := st.Records(day)
		if err != nil {
			continue
		}
		for _, r := range recs {
			u := Usage{}
			if r.Usage != nil {
				u = Usage{Input: r.Usage.Input, Output: r.Usage.Output,
					CacheRead: r.Usage.CacheRead, CacheWrite: r.Usage.CacheWrite}
			}
			cost := 0.0
			hasPrice := false
			if r.Usage != nil {
				if p := pt.Find(r.Model); p != nil {
					cost = Cost(p, u)
					hasPrice = true
				}
			}
			add := func(b *Bucket) *Bucket {
				b.Requests++
				if r.Error != "" || r.Status >= 400 {
					b.Errors++
				}
				b.Input += u.Input + u.CacheRead + u.CacheWrite
				b.Output += u.Output
				b.CacheRead += u.CacheRead
				b.Tools += len(r.ToolCalls)
				b.CostUSD += cost
				if hasPrice {
					b.HasPrice = true
				}
				return b
			}
			add(&s.Totals)
			mb := models[r.Model]
			if mb == nil {
				mb = &Bucket{Key: r.Model}
				models[r.Model] = mb
			}
			add(mb)
			ab := agents[r.Agent]
			if ab == nil {
				ab = &Bucket{Key: r.Agent}
				agents[r.Agent] = ab
			}
			add(ab)
			db := perDay[day]
			if db == nil {
				db = &Bucket{Key: day}
				perDay[day] = db
			}
			add(db)
			for _, tc := range r.ToolCalls {
				tools[tc.Name]++
			}
		}
	}
	s.ByModel = sortBuckets(models)
	s.ByAgent = sortBuckets(agents)
	// ByDay 按日期序（旧在前）
	ordered := make([]Bucket, 0, len(perDay))
	for i := len(days) - 1; i >= 0; i-- {
		if b, ok := perDay[days[i]]; ok {
			ordered = append(ordered, *b)
		}
	}
	s.ByDay = ordered
	for name, n := range tools {
		s.Tools = append(s.Tools, ToolStat{Name: name, Count: n})
	}
	sort.Slice(s.Tools, func(i, j int) bool { return s.Tools[i].Count > s.Tools[j].Count })
	return s
}

func sortBuckets(m map[string]*Bucket) []Bucket {
	out := make([]Bucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out
}
