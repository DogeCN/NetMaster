package profile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNilTraceIsInert(t *testing.T) {
	// 未启用时 New 返回 nil，而代理的请求路径上每条 CONNECT 都有几处调用点。
	// 全部方法必须在 nil 上安全，否则"为了可观测性给热路径加 if"就成了负担。
	var tr *Trace
	tr.Begin("x", time.Second)()
	tr.Fact("k", "v")
	tr.FactInt("n", 1)
	tr.Mark("m")
	if got := tr.Fingerprint(); got != "" {
		t.Fatalf("Fingerprint on a nil Trace = %q, want empty", got)
	}
	if tr.Spans() != nil {
		t.Fatal("Spans on a nil Trace should be nil")
	}
	if err := tr.WriteReport(&bytes.Buffer{}); err != nil {
		t.Fatalf("WriteReport on nil: %v", err)
	}
	if err := tr.WriteJSON(""); err != nil {
		t.Fatalf("WriteJSON on nil: %v", err)
	}
	if d := tr.Total(); d != 0 {
		t.Fatalf("Total on nil = %v", d)
	}
}

func TestNewDisabledReturnsNil(t *testing.T) {
	if tr := New(false); tr != nil {
		t.Fatal("New(false) must return nil so call sites need no guard")
	}
}

func TestBeginRecordsDuration(t *testing.T) {
	tr := New(true)
	end := tr.Begin("probe", time.Second)
	time.Sleep(20 * time.Millisecond)
	end()

	spans := tr.Spans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name != "probe" {
		t.Fatalf("span name %q, want probe", spans[0].Name)
	}
	if spans[0].Dur < 15*time.Millisecond {
		t.Fatalf("span duration %v, want >= 15ms", spans[0].Dur)
	}
}

// TestBeginIsNotUsableAsABareCall 钉住上面那条 API 陷阱。
//
// `tr.Begin(...)()` 看起来完全正常，实际 span 在同一行开始又结束、耗时恒为 0。
// 这条用例把这个形态本身固定下来，免得将来"简化"测试时顺手写成裸调用。
func TestBeginIsNotUsableAsABareCall(t *testing.T) {
	tr := New(true)
	tr.Begin("bare", time.Second)() // 就是那个陷阱的形态

	for _, s := range tr.Spans() {
		if s.Dur != 0 {
			t.Fatalf("bare call recorded %v; it must record ~0 — that is why call sites use defer", s.Dur)
		}
	}
}

// TestOverFlagsBudgetBreach 是"防止意外耗时"这件事本身。
//
// 预算不是文档里的数字，是会在报告里变成 `!` 的判据 —— 只有会自己跳出来的
// 超标，才不要求每个人都去人工比数字。
func TestOverFlagsBudgetBreach(t *testing.T) {
	tr := New(true)
	cheap := tr.Begin("cheap", time.Second)
	time.Sleep(30 * time.Millisecond)
	cheap()

	dear := tr.Begin("expensive", 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	dear()

	var buf bytes.Buffer
	if err := tr.WriteReport(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	lineOf := func(name string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, name) {
				return l
			}
		}
		return ""
	}
	if l := lineOf("expensive"); !strings.HasPrefix(l, "!") {
		t.Fatalf("over-budget span not marked: %q", l)
	}
	if l := lineOf("cheap"); strings.HasPrefix(l, "!") {
		t.Fatalf("within-budget span wrongly marked: %q", l)
	}
	if !strings.Contains(out, "exceeded their budget") {
		t.Errorf("report does not state that something went over budget:\n%s", out)
	}
}

func TestSpanWithoutBudgetNeverOver(t *testing.T) {
	tr := New(true)
	end := tr.Begin("no-budget", 0)
	time.Sleep(5 * time.Millisecond)
	end()
	for _, s := range tr.Spans() {
		if s.Over() {
			t.Fatalf("a span with no budget must never report Over(): %+v", s)
		}
	}
}

// TestReportSortsByDuration 是"最贵的排在最前"的承诺。
//
// 只按时间顺序打印的话，读者必须自己扫一遍全表才知道哪段贵 —— 而真正花钱的
// 阶段往往不是启动时盯着的那个。
func TestReportSortsByDuration(t *testing.T) {
	tr := New(true)
	time.Sleep(30 * time.Millisecond)
	cheap := tr.Begin("first-but-cheap", time.Second)
	time.Sleep(10 * time.Millisecond)
	cheap()

	dear := tr.Begin("second-but-dear", time.Second)
	time.Sleep(60 * time.Millisecond)
	dear()

	var buf bytes.Buffer
	if err := tr.WriteReport(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	di := strings.Index(out, "second-but-dear")
	fi := strings.Index(out, "first-but-cheap")
	if di < 0 || fi < 0 || di > fi {
		t.Fatalf("duration-sorted block is not sorted:\n%s", out)
	}
}

// TestFingerprintTracksFacts 是"防止拿错数据得出错结论"的核心。
//
// 指纹相同才可比。节点池成员、隧道数、是否开 ECH 任何一项变了，指纹就得变。
func TestFingerprintTracksFacts(t *testing.T) {
	a, b := New(true), New(true)
	a.FactInt("pool", 10)
	a.Fact("ech", "off")
	b.FactInt("pool", 10)
	b.Fact("ech", "off")

	if a.Fingerprint() != b.Fingerprint() {
		t.Fatalf("identical facts produced different fingerprints: %s vs %s",
			a.Fingerprint(), b.Fingerprint())
	}

	b.FactInt("pool", 11)
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("a different node pool must change the fingerprint — that is the whole point")
	}
}

func TestFingerprintIgnoresFactOrder(t *testing.T) {
	a, b := New(true), New(true)
	a.Fact("x", "1")
	a.Fact("y", "2")
	b.Fact("y", "2")
	b.Fact("x", "1")
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("fact insertion order must not change the fingerprint")
	}
}

func TestDiffFactsNamesTheReason(t *testing.T) {
	a, b := New(true), New(true)
	a.Fact("pool", "10 nodes")
	a.Fact("tunnels", "4")
	b.Fact("pool", "10 nodes")
	b.Fact("tunnels", "2")

	d := DiffFacts(a, b)
	if len(d) != 1 {
		t.Fatalf("expected exactly 1 differing fact, got %v", d)
	}
	if d[0][0] != "tunnels" || !strings.Contains(d[0][1], "4 → 2") {
		t.Fatalf("diff = %v, want tunnels: 4 → 2", d)
	}
}

func TestDiffFactsReportsMissingSide(t *testing.T) {
	a, b := New(true), New(true)
	a.Fact("ech", "off")
	d := DiffFacts(a, b)
	if len(d) != 1 {
		t.Fatalf("a fact present in only one run must show as a difference, got %v", d)
	}
	if !strings.Contains(d[0][1], "(缺)") {
		t.Fatalf("diff = %v, want the missing side spelled out", d)
	}
}

func TestWriteJSONRoundTrip(t *testing.T) {
	tr := New(true)
	tr.FactInt("pool", 10)
	end := tr.Begin("probe", 2*time.Second)
	end()

	path := filepath.Join(t.TempDir(), "profile.json")
	if err := tr.WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Fingerprint string            `json:"fingerprint"`
		Facts       map[string]string `json:"facts"`
		Spans       []Span            `json:"spans"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("profile JSON does not parse: %v\n%s", err, b)
	}
	if got.Fingerprint != tr.Fingerprint() {
		t.Fatalf("JSON fingerprint %q != live %q", got.Fingerprint, tr.Fingerprint())
	}
	if got.Facts["pool"] != "10" {
		t.Fatalf("facts lost in JSON: %v", got.Facts)
	}
	if len(got.Spans) != 1 {
		t.Fatalf("JSON has %d spans, want 1", len(got.Spans))
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvVar, "")
	if on, _ := FromEnv(); on {
		t.Fatal("empty env must mean disabled")
	}
	t.Setenv(EnvVar, "1")
	if on, path := FromEnv(); !on || path != "" {
		t.Fatalf("env=1 → enabled=%v path=%q, want true/\"\"", on, path)
	}
	t.Setenv(EnvVar, filepath.Join(t.TempDir(), "p.json"))
	on, path := FromEnv()
	if !on || !strings.HasSuffix(path, "p.json") {
		t.Fatalf("env=path → enabled=%v path=%q", on, path)
	}
}

func TestConcurrentSpansAreSafe(t *testing.T) {
	// 启动阶段的 span 在主 goroutine，请求路径的 span 在各自的 goroutine。
	// 这里不做 -race（本机缺 cgo），但至少确认不会丢记录或 panic。
	tr := New(true)
	const workers, each = 8, 20
	done := make(chan struct{})
	for w := 0; w < workers; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < each; i++ {
				endStage := tr.Begin("stage", time.Second)
				endStage()
				tr.Mark("mark")
				tr.FactInt("counter", i)
			}
		}()
	}
	for w := 0; w < workers; w++ {
		<-done
	}
	if got := len(tr.Spans()); got != workers*each*2 {
		t.Fatalf("got %d records, want %d", got, workers*each*2)
	}
}
