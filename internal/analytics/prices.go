// Package analytics 成本与用量分析：价格表（可配置）+ 多维聚合。
// 数据来自两路：网关账本（请求级）与本地会话日志（会话级累计）。
package analytics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Price 一个模型的价格（美元 / 百万 token）。CacheWrite5m 与 1h 档未细分，按写价。
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// defaultPrices 内置默认价格（$/Mtok，公开定价，可被 ~/.ai-trace/prices.json 覆盖）。
// 模型名做前缀匹配（如 claude-sonnet-5 匹配 claude-sonnet-5-2026xxxx）。
var defaultPrices = map[string]Price{
	"claude-opus-5":   {5, 25, 0.5, 6.25},
	"claude-sonnet-5": {3, 15, 0.3, 3.75},
	"claude-haiku-4":  {1, 5, 0.1, 1.25},
	"claude-fable-5":  {6, 30, 0.6, 7.5},
	"gpt-5":           {1.25, 10, 0.125, 0},
	"gpt-5-mini":      {0.25, 2, 0.025, 0},
	"o4-mini":         {1.1, 4.4, 0.275, 0},
	"gemini-3-pro":    {2, 12, 0.2, 0},
	"gemini-3-flash":  {0.15, 0.6, 0.015, 0},
	"deepseek-v4":     {0.27, 1.1, 0.07, 0.27},
	"kimi-k3":         {0.6, 2.5, 0.07, 0},
	"glm-5.3":         {0.55, 2.2, 0.07, 0},
	"glm-5":           {0.55, 2.2, 0.07, 0},
}

// PriceTable 价格表：默认 + 用户覆盖合并。
type PriceTable struct { prices map[string]Price }

// LoadPriceTable 加载价格表；dataDir 下的 prices.json 覆盖同名默认项
// （格式：{"模型名": {"input":3,"output":15,"cacheRead":0.3,"cacheWrite":3.75}}）。
func LoadPriceTable(dataDir string) *PriceTable {
	pt := &PriceTable{prices: map[string]Price{}}
	for k, v := range defaultPrices {
		pt.prices[k] = v
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "prices.json"))
	if err == nil {
		var override map[string]Price
		if json.Unmarshal(data, &override) == nil {
			for k, v := range override {
				pt.prices[strings.ToLower(k)] = v
			}
		}
	}
	return pt
}

// Find 模型价格：最长前缀匹配（精确优先）。找不到返回 nil（成本记 0）。
func (pt *PriceTable) Find(model string) *Price {
	m := strings.ToLower(model)
	// 先精确
	if p, ok := pt.prices[m]; ok {
		return &p
	}
	// 最长前缀
	best := ""
	for k := range pt.prices {
		if strings.HasPrefix(m, k) && len(k) > len(best) {
			best = k
		}
	}
	if best != "" {
		p := pt.prices[best]
		return &p
	}
	return nil
}

// Usage 统计用的用量接口（ir.Usage 满足）。
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Cost 美元成本（保留 6 位小数足够）。
func Cost(p *Price, u Usage) float64 {
	if p == nil {
		return 0
	}
	m := 1e-6 // per-token = $/Mtok / 1e6
	return float64(u.Input)*p.Input*m +
		float64(u.Output)*p.Output*m +
		float64(u.CacheRead)*p.CacheRead*m +
		float64(u.CacheWrite)*p.CacheWrite*m
}
