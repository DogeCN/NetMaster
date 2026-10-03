// Package profile 采集客户端的**分段耗时**，为"这次改动到底快在哪、慢在哪"提供
// 可比较的证据。
//
// 它要解决的是两件具体的事，都不是"打日志"能解决的：
//
//  1. **意想不到的阶段耗时。** 我们只给已知的阶段配了预算（budget），超出即在报告里
//     标 `!`。于是"某一段突然变慢"不需要人去比数字 —— 它自己会跳出来。
//     只按时间顺序打印的日志做不到这点：真正贵的那一段往往不是我们预期的那一段。
//
//  2. **拿错数据得出错结论。** 两次运行要能比，前提是它们量的**是同一件事**。
//     Fingerprint 把"这次量的到底是什么"编码成一串短哈希：节点池的实际成员、
//     关键配置、代码版本。两次指纹不同就说明不可比，报告里会直接写明差在哪一项。
//     这是踩过的坑：nodes[:64] 曾经按到达顺序随机截断，于是每次启动的节点池都不同，
//     "启动 7.2s → 5.1s" 那次对比严格说并不成立。
//
// # 关闭时零开销
//
// 未启用时 New 返回 nil，方法全部是 nil 接收者上的空操作。调用点不必写 if，
// 而一次 span 的成本真的只有一次 nil 判断 —— 代理的请求路径上每条 CONNECT 都有
// 三四处调用点，开销必须小到不值得讨论。
package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EnvVar 是启用采集的环境变量。值为一个文件路径时额外把 JSON 写进该文件；
// 值为 "1" / "stdout" 时只往标准输出打印。
const EnvVar = "NETMASTER_PROFILE"

// Trace 一次运行的耗时记录。并发安全：请求路径上的 span 与主 goroutine 的
// 启动阶段会同时写进来。
type Trace struct {
	origin time.Time

	mu       sync.Mutex
	spans    []Span
	facts    map[string]string
	factsOrd []string
}

// Span 一段有起止的耗时。
type Span struct {
	Name   string        `json:"name"`
	Off    time.Duration `json:"off"` // 相对 Trace 起点的偏移
	Dur    time.Duration `json:"dur"`
	Budget time.Duration `json:"budget,omitempty"`
	Attrs  []string      `json:"attrs,omitempty"`
	Parent string        `json:"parent,omitempty"`
}

// Over 报告这个 span 是否超出了自己声明的预算。没声明预算的恒为 false。
func (s Span) Over() bool { return s.Budget > 0 && s.Dur > s.Budget }

// FromEnv 按环境变量决定是否开启，并给出 JSON 的落地路径（没有则为空）。
//
// 单独拆出来是为了让"要不要采集"这件事在 main 的最早期就有答案 —— 启动阶段里
// 恰好包含最值得量的几段，等 main 跑到一半再决定就晚了一截。
func FromEnv() (enabled bool, jsonPath string) {
	v := strings.TrimSpace(os.Getenv(EnvVar))
	if v == "" {
		return false, ""
	}
	if v == "1" || v == "stdout" || v == "true" {
		return true, ""
	}
	return true, v
}

// New 返回一个采集器；enabled 为假时返回 nil（所有方法都容忍 nil 接收者）。
func New(enabled bool) *Trace {
	if !enabled {
		return nil
	}
	return &Trace{
		origin: time.Now(),
		facts:  map[string]string{},
	}
}

// Begin 开始一段计时，返回结束它的函数。惯用法是 defer：
//
//	defer tr.Begin("probe", 2*time.Second)()
//
// 用 defer 而不是要求调用点手写配对，是因为这些阶段都有提前 return 的分支
// （拨号失败、认证失败……），漏掉一次 End 就等于丢一段数据，而且丢得悄无声息。
//
// ⚠️ **不要**写成裸调用 `tr.Begin(...)()`。那样 span 会在同一行开始又结束，
// 耗时恒为 0，而且代码看起来完全正常 —— 报告里那一段会安静地显示 0ms。
// 要立即计时就用 end := tr.Begin(...)；……；end()。
//
// budget 是"超过就标红"的阈值，0 表示不设预算。attrs 是 k=v 形式的补充说明。
func (t *Trace) Begin(name string, budget time.Duration, attrs ...string) func() {
	if t == nil {
		return func() {}
	}
	start := time.Now()
	parent := ""
	t.mu.Lock()
	if n := len(t.spans); n > 0 {
		// 父链用"最后一个开始且尚未结束的 span"，够用来读懂调用结构。
		// 不追求严格树：并发时它只是一个提示。
		for i := n - 1; i >= 0; i-- {
			if t.spans[i].Name != name {
				parent = t.spans[i].Name
				break
			}
		}
	}
	t.mu.Unlock()

	return func() {
		t.mu.Lock()
		t.spans = append(t.spans, Span{
			Name:   name,
			Off:    start.Sub(t.origin),
			Dur:    time.Since(start),
			Budget: budget,
			Attrs:  attrs,
			Parent: parent,
		})
		t.mu.Unlock()
	}
}

// Fact 记下一个"这次量的到底是什么"的事实，参与 Fingerprint。
//
// 凡是会**改变耗时**的东西都应该登记：节点池的实际成员、隧道数、是否开 ECH、
// 分片参数。少了任何一项，两次运行的数字就不可比，而不可比的数字比没有数字更坏。
func (t *Trace) Fact(key, value string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.facts[key]; !seen {
		t.factsOrd = append(t.factsOrd, key)
	}
	t.facts[key] = value
}

// FactInt 是 Fact 的整数版本。
func (t *Trace) FactInt(key string, value int) {
	t.Fact(key, strconv.Itoa(value))
}

// Mark 记下一个瞬时事件（没有持续时间），例如"就绪"。
func (t *Trace) Mark(name string, attrs ...string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spans = append(t.spans, Span{
		Name:  name,
		Off:   time.Since(t.origin),
		Attrs: attrs,
	})
}

// Spans 返回快照，按开始偏移排序。调用方拿到的切片与内部状态无关。
func (t *Trace) Spans() []Span {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Span, len(t.spans))
	copy(out, t.spans)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Off < out[j].Off })
	return out
}

// Fingerprint 是"这次量的到底是什么"的短哈希。
//
// 相同的指纹才可以比。不同的指纹意味着至少有一项事实不同，两次的耗时差里就
// 混着这项目标的差异 —— 拿它当"这次改动快了多少"是错的。
func (t *Trace) Fingerprint() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	facts := make([]string, 0, len(t.factsOrd))
	for _, k := range t.factsOrd {
		facts = append(facts, k+"="+t.facts[k])
	}
	t.mu.Unlock()
	sort.Strings(facts)
	sum := sha256.Sum256([]byte(strings.Join(facts, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// Facts 返回事实表快照（按 key 排序），用于报告里逐项列出。
func (t *Trace) Facts() [][2]string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([][2]string, 0, len(t.facts))
	for k, v := range t.facts {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// DiffFacts 返回两次运行之间**不同**的事实项。这是"为什么这两次不可比"的答案。
func DiffFacts(a, b *Trace) [][2]string {
	af, bf := map[string]string{}, map[string]string{}
	for _, kv := range a.Facts() {
		af[kv[0]] = kv[1]
	}
	for _, kv := range b.Facts() {
		bf[kv[0]] = kv[1]
	}
	keys := map[string]bool{}
	for k := range af {
		keys[k] = true
	}
	for k := range bf {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out [][2]string
	for _, k := range sorted {
		av, bv := af[k], bf[k]
		if av == bv {
			continue
		}
		out = append(out, [2]string{k, fmt.Sprintf("%s → %s", orNone(av), orNone(bv))})
	}
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(缺)"
	}
	return s
}

// Total 返回全部 span 的耗时之和。它不等于"总时长"（并发段会重复计入），
// 但用来比较两次运行的"做了多少工作"是有效的。
func (t *Trace) Total() time.Duration {
	var sum time.Duration
	for _, s := range t.Spans() {
		sum += s.Dur
	}
	return sum
}

// WriteReport 写人读的���告。
//
// 两块内容，顺序是有意的：
//
//	【按耗时排序】把最贵的排在前面 —— 这是"意外耗时在哪"的答案。
//	【时间线】按真实先后排列 —— 这是"那一刻到底发生了什么"的答案。
//
// 只给时间线不给排序，等于把"哪一段最贵"的判断完全交给读者；
// 而实测里真正花钱的阶段往往不是启动时盯着的那个。
func (t *Trace) WriteReport(w io.Writer) error {
	if t == nil {
		return nil
	}
	spans := t.Spans()
	if len(spans) == 0 {
		_, err := fmt.Fprintln(w, "profile: no spans recorded")
		return err
	}

	timed := make([]Span, 0, len(spans))
	for _, s := range spans {
		if s.Dur > 0 {
			timed = append(timed, s)
		}
	}

	fmt.Fprintf(w, "\n=== profile %s ===\n", t.Fingerprint())
	if facts := t.Facts(); len(facts) > 0 {
		fmt.Fprintln(w, "facts (same fingerprint ⇒ comparable):")
		for _, kv := range facts {
			fmt.Fprintf(w, "  %-22s %s\n", kv[0], kv[1])
		}
	}

	if len(timed) > 0 {
		sorted := make([]Span, len(timed))
		copy(sorted, timed)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Dur > sorted[j].Dur })
		fmt.Fprintln(w, "\nby duration (where the time went):")
		fmt.Fprintf(w, "  %-9s %-9s %-10s %s\n", "DUR", "BUDGET", "OFFSET", "SPAN")
		over := 0
		for _, s := range sorted {
			mark := " "
			if s.Over() {
				mark = "!"
				over++
			}
			budget := "-"
			if s.Budget > 0 {
				budget = s.Budget.Round(time.Millisecond).String()
			}
			fmt.Fprintf(w, "%s %-9s %-10s %-9s %s\n",
				mark, s.Dur.Round(time.Millisecond), budget,
				s.Off.Round(time.Millisecond), s.Name)
			for _, a := range s.Attrs {
				fmt.Fprintf(w, "  %-9s %-9s %-10s   └─ %s\n", "", "", "", a)
			}
		}
		if over > 0 {
			fmt.Fprintf(w, "%d stage(s) exceeded their budget — marked ! above\n", over)
		}
	}

	fmt.Fprintln(w, "\ntimeline:")
	for _, s := range spans {
		if s.Dur == 0 {
			fmt.Fprintf(w, "  %-10s ● %s %s\n", s.Off.Round(time.Millisecond), s.Name, joinAttrs(s.Attrs))
			continue
		}
		fmt.Fprintf(w, "  %-10s ├─%s %s %s\n",
			s.Off.Round(time.Millisecond),
			s.Dur.Round(time.Millisecond), s.Name, joinAttrs(s.Attrs))
	}
	return nil
}

func joinAttrs(attrs []string) string {
	if len(attrs) == 0 {
		return ""
	}
	return "  [" + strings.Join(attrs, " ") + "]"
}

// snapshot 是落盘的 JSON 结构。
type snapshot struct {
	Fingerprint string            `json:"fingerprint"`
	Generated   string            `json:"generated"`
	Facts       map[string]string `json:"facts"`
	Spans       []Span            `json:"spans"`
}

// WriteJSON 把采集结果写成机器可读的 JSON。
//
// 有了它，两次运行的对比才能自动化 —— 而人眼比两屏数字正是"改错方向"最容易
// 发生的地方。
func (t *Trace) WriteJSON(path string) error {
	if t == nil || path == "" {
		return nil
	}
	facts := map[string]string{}
	for _, kv := range t.Facts() {
		facts[kv[0]] = kv[1]
	}
	b, err := json.MarshalIndent(snapshot{
		Fingerprint: t.Fingerprint(),
		Generated:   time.Now().Format(time.RFC3339),
		Facts:       facts,
		Spans:       t.Spans(),
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
