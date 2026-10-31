package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDeleteBy 删除指定 agent 的记录：账本行与 body 文件一并清掉，
// 其余记录原样保留（字节级不变）。
func TestDeleteBy(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, agent string) (Record, FullBody) {
		return Record{ID: id, Time: time.Now(), Agent: agent, Protocol: "anthropic"},
			FullBody{RequestBody: json.RawMessage(`{}`)}
	}
	for _, x := range []struct{ id, agent string }{
		{"20260101-000001-aaa", "unknown"},
		{"20260101-000002-bbb", "claude-code"},
		{"20260101-000003-ccc", "unknown"},
	} {
		rec, body := mk(x.id, x.agent)
		rec.Time = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // 同一天同一账本文件
		st.Append(rec, body)
	}
	day := st.Days()[0]
	before, _ := st.Records(day)
	if len(before) != 3 {
		t.Fatalf("前置失败：应有 3 条，得 %d", len(before))
	}

	n, err := st.DeleteBy(func(r Record) bool { return r.Agent == "unknown" })
	if err != nil || n != 2 {
		t.Fatalf("DeleteBy = (%d, %v), want (2, nil)", n, err)
	}
	after, _ := st.Records(day)
	if len(after) != 1 || after[0].Agent != "claude-code" {
		t.Fatalf("删除后应剩 1 条 claude-code: %+v", after)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), "bodies", "20260101-000001-aaa.json")); !os.IsNotExist(err) {
		t.Error("被删记录的 body 文件应一并删除")
	}
	if _, err := os.Stat(filepath.Join(st.Root(), "bodies", "20260101-000002-bbb.json")); err != nil {
		t.Error("保留记录的 body 文件不应被删")
	}
}
