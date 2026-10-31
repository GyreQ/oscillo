package analytics

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ai-trace/internal/ir"
	"ai-trace/internal/store"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPriceFind(t *testing.T) {
	pt := LoadPriceTable(t.TempDir())
	cases := map[string]float64{
		"claude-opus-5-5":            5,  // 前缀匹配
		"claude-sonnet-5-20260101":   3,  // 带日期后缀
		"claude-haiku-4-5-20251001":  1,
		"glm-5.3":                    0.55,
		"unknown-model":              -1, // 无价格
	}
	for model, wantIn := range cases {
		p := pt.Find(model)
		if wantIn < 0 {
			if p != nil {
				t.Errorf("%s 不应有价格", model)
			}
			continue
		}
		if p == nil || p.Input != wantIn {
			t.Errorf("%s 价格不对: %+v", model, p)
		}
	}
}

func TestPriceOverride(t *testing.T) {
	dir := t.TempDir()
	// 用户覆盖 glm-5.3 价格
	writeFile(t, filepath.Join(dir, "prices.json"),
		`{"glm-5.3": {"input": 100, "output": 200, "cacheRead": 1, "cacheWrite": 1}}`)
	pt := LoadPriceTable(dir)
	p := pt.Find("glm-5.3")
	if p == nil || p.Input != 100 || p.Output != 200 {
		t.Fatalf("覆盖未生效: %+v", p)
	}
	// 其他默认仍在
	if q := pt.Find("gpt-5"); q == nil || q.Input != 1.25 {
		t.Errorf("默认价格被影响: %+v", q)
	}
}

func TestCost(t *testing.T) {
	pt := LoadPriceTable(t.TempDir())
	p := pt.Find("claude-sonnet-5-5")
	// 1M input + 1M output + 2M cache read → 3 + 15 + 0.6 = 18.6
	c := Cost(p, Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 2_000_000})
	if c < 18.59 || c > 18.61 {
		t.Errorf("成本计算不对: %v", c)
	}
	if Cost(nil, Usage{Input: 100}) != 0 {
		t.Error("无价格模型成本应为 0")
	}
}

// TestAnalyze 聚合：多天、多模型、错误与工具统计。
func TestAnalyze(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.Open(dir)
	today := time.Now().Format("2006-01-02")
	st.Append(store.Record{
		ID: "r1", Time: time.Now(), Agent: "claude-code", Protocol: "anthropic",
		Model: "claude-sonnet-5-5", Status: 200, DurationMS: 100,
		Usage:     &ir.Usage{Input: 1000, Output: 500, CacheRead: 200},
		ToolCalls: []ir.ToolCall{{ID: "t1", Name: "Bash"}, {ID: "t2", Name: "Read"}},
	}, store.FullBody{})
	st.Append(store.Record{
		ID: "r2", Time: time.Now(), Agent: "codex", Protocol: "openai-responses",
		Model: "gpt-5.2", Status: 500, DurationMS: 50, Error: "boom",
		Usage: &ir.Usage{Input: 100, Output: 0},
	}, store.FullBody{})

	s := Analyze(st, LoadPriceTable(dir), 7)
	if s.Totals.Requests != 2 || s.Totals.Errors != 1 {
		t.Errorf("总数不对: %+v", s.Totals)
	}
	if s.Totals.Input != 1000+200+100 {
		t.Errorf("输入 token 聚合不对: %d", s.Totals.Input)
	}
	if len(s.ByModel) != 2 || len(s.ByAgent) != 2 {
		t.Errorf("分组不对: %d %d", len(s.ByModel), len(s.ByAgent))
	}
	if s.Totals.Tools != 2 {
		t.Errorf("工具数不对: %d", s.Totals.Tools)
	}
	if len(s.Tools) != 2 || s.Tools[0].Count != 1 {
		t.Errorf("工具统计不对: %+v", s.Tools)
	}
	if len(s.ByDay) != 1 || s.ByDay[0].Key != today {
		t.Errorf("按日不对: %+v", s.ByDay)
	}
	// 成本：sonnet (1000×3 + 500×15 + 200×0.3)/1e6 + gpt-5.2 100×1.25/1e6
	want := (1000*3+500*15+200*0.3)/1e6 + 100*1.25/1e6
	if s.Totals.CostUSD < want-0.0001 || s.Totals.CostUSD > want+0.0001 {
		t.Errorf("成本不对: %v vs %v", s.Totals.CostUSD, want)
	}
}
