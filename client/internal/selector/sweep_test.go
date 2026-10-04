package selector

import (
	"testing"
	"time"
)

// SweepExpired 必须清掉三张表里过期的条目、留下新鲜的。
//
// 过期原本靠"读到才删"，写入方用回填时间戳来构造"过期但从未被读过"的状态 ——
// 那正是惰性删除永远覆盖不到、只有清扫能回收的形态。
func TestSweepExpiredRemovesOnlyStaleEntries(t *testing.T) {
	p := New(Config{Nodes: nil})
	fresh := time.Now()
	stale := fresh.Add(-2 * time.Hour) // 比 5min/30min 老，比 6h 分片记忆新

	p.directDown["down-stale.test"] = stale
	p.directDown["down-fresh.test"] = fresh
	p.directBlocked["blk-stale.test"] = stale
	p.directBlocked["blk-fresh.test"] = fresh
	p.fragDirect["frag-stale.test"] = fresh.Add(-7 * time.Hour) // 超过 6h
	p.fragDirect["frag-fresh.test"] = fresh

	p.SweepExpired()

	for _, h := range []string{"down-stale.test", "blk-stale.test", "frag-stale.test"} {
		if _, ok := p.directDown[h]; ok {
			t.Errorf("%s: stale directDown survived the sweep", h)
		}
		if _, ok := p.directBlocked[h]; ok {
			t.Errorf("%s: stale directBlocked survived the sweep", h)
		}
		if _, ok := p.fragDirect[h]; ok {
			t.Errorf("%s: stale fragDirect survived the sweep", h)
		}
	}
	if p.directDown["down-fresh.test"].IsZero() {
		t.Error("fresh directDown entry was swept")
	}
	if p.directBlocked["blk-fresh.test"].IsZero() {
		t.Error("fresh directBlocked entry was swept")
	}
	if _, ok := p.fragDirect["frag-fresh.test"]; !ok {
		t.Error("fresh fragDirect entry was swept")
	}
}
