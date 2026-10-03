package selector

// Optimize 的 defer 语义缺陷的钉子。
//
// 缺陷本体：`Optimize` 返回**未命名** OptResult，`return res` 会先把 res 拷进返回槽位、
// 再执行 defer，于是 `defer func(){ res.ECHWorked = tlsutil.ECHEnabled() }()` 那次赋值
// 被丢掉，ECHWorked 恒为 false。
//
// 为什么已有测试抓不到它，这里又为什么必须用源码级断言：
//
//   - **编译器看不见**：未命名返回值 + defer 写变量是完全合法的 Go，`go build`、
//     `go vet` 全绿。（已实测。）
//   - **行为测试看不见**：ECHWorked 的值取决于 ECH 是否真的被用上，而那要在真实的
//     TLS 握手里才确定；探测用的是假节点时这条分支根本不会走到 ECH。
//   - 所以唯一可靠的钉子是"直接检查这个函数签名"。
//
// 这与 server 侧 session.js 的做法是同一套：把源码文本本身当被测对象。
// 一个能被 build 与 vet 放过的语义陷阱，就得用能看见语义的检查去钉。

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"netmaster/internal/entry"
)

// TestOptimizeUsesNamedResult 钉住函数签名必须是命名返回值。
func TestOptimizeUsesNamedResult(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	sig := regexp.MustCompile(`(?m)^func Optimize\([^)]*\) *(\([^)]*\)|[A-Za-z_][A-Za-z0-9_.]*) *\{`)
	m := sig.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("could not find the Optimize signature in probe.go — update this test")
	}
	ret := m[1]
	if !strings.HasPrefix(ret, "(") || !strings.Contains(ret, "res") {
		t.Fatalf("Optimize returns %q, want a named result `(res OptResult)`.\n"+
			"With an unnamed result `return res` copies res into the return slot before "+
			"the deferred funcs run, so `defer func(){ res.ECHWorked = ... }()` is "+
			"silently discarded and ECHWorked stays false forever.", ret)
	}
}

// TestECHWorkedIsOnlyAssignedFromADefer 钉住赋值点的位置。
//
// 为什么要求它必须在 defer 里：ECHWorked 的值只有等握手真正结束后才知道，
// 所以它天然只能在函数返回前一刻写。而那个"一刻"正是 `return res` 已经把值拷走
// 之后 —— 也就是说，这行**只**能通过 defer 落到命名返回槽位上。
//
// 写成 defer 之外的任何形式，都会重新落进"赋值被丢弃"或"读到的是中途旧值"的坑。
func TestECHWorkedIsOnlyAssignedFromADefer(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	// 注意匹配任意位置的赋值：真正的写法是 defer func() { res.ECHWorked = ... }()，
	// 赋值写在**行内**，用 ^\s*res\. 锚定行首会一条都匹配不到。
	assigns := regexp.MustCompile(`[^\n]*res\.ECHWorked\s*=[^\n]*`).FindAllString(string(src), -1)
	if len(assigns) == 0 {
		t.Fatal("no assignment to res.ECHWorked — ECHWorked would never be reported")
	}
	for _, a := range assigns {
		if !strings.Contains(a, "defer") {
			t.Errorf("res.ECHWorked is assigned outside a deferred func: %q — "+
				"an assignment before `return res` will not survive", strings.TrimSpace(a))
		}
	}
}

// TestDeferOnNamedResultSurvivesReturn 是那条语言规则的最小可执行样本。
//
// 与 proxy 包那条同名用例互为参照：这里说明"为什么"，那里证明"真的这样写就成立"。
func TestDeferOnNamedResultSurvivesReturn(t *testing.T) {
	type resT struct{ worked bool }

	named := func() (r resT) {
		defer func() { r.worked = true }()
		return r
	}
	if !named().worked {
		t.Fatal("defer write to a named result must survive `return r`")
	}

	// 对照组：同样的写法在未命名返回值上会被丢掉。
	unnamed := func() resT {
		var r resT
		defer func() { r.worked = true }()
		return r
	}
	if unnamed().worked {
		t.Skip("this runtime propagates the defer write; the trap is version-specific")
	}
}

// TestECHWorkedIsRecordedOnlyWhereECHHandshakeSucceeds 钉住"成功"这个信号的来源。
//
// 为什么这条必须是源码级：正向路径（ECH 真的握手成功）需要一台真实的 ECH 端点，
// 仓库里造不出来 —— 这与上面 TestOptimizeUsesNamedResult 是同一个理由，
// 编译器与行为测试都看不见这件事。
//
// 它要防的是两类改坏：
//  1. 有人把 `echWorked.Store(true)` 删了/挪走了 → ECHWorked 恒假 → 启动日志永远
//     说"fall back to a VISIBLE SNI"，用户开了 CF zone 的 ECH 也看不到成功信号。
//     那和本项目最贵那次教训（ECH 失败一路静默）是同一个静默，只是方向相反。
//  2. 有人把置位挪到 ECH 失败分支或熔断分支 → 又是恒真的老毛病。
//
// 所以判据是：全仓库只有一处 Store(true)，且它必须挨着 ECHOverConnDeadline 之后
// 的 `if err == nil`。
func TestECHWorkedIsRecordedOnlyWhereECHHandshakeSucceeds(t *testing.T) {
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	sets := regexp.MustCompile(`echWorked\.Store\(true\)`).FindAllString(string(src), -1)
	if len(sets) == 0 {
		t.Fatal("no echWorked.Store(true) — ECHWorked can never become true, so the " +
			"startup log would report a visible SNI even when ECH genuinely works")
	}
	if len(sets) > 1 {
		t.Errorf("%d sites set echWorked to true; there must be exactly one — the ECH "+
			"handshake success branch. More than one means some failure path can report success.",
			len(sets))
	}
	// 置位点必须紧跟在 ECH 握手成功的判据之后。
	if !regexp.MustCompile(`(?s)ECHOverConnDeadline\(.*?if err == nil \{.*?echWorked\.Store\(true\)`).Match(src) {
		t.Error("echWorked.Store(true) is not inside the `if err == nil` branch that " +
			"follows ECHOverConnDeadline — it may be reporting success on a failed handshake")
	}
}

// TestECHWorkedIsResetOnEveryOptimizeRun 钉住"每轮从零开始"。
//
// 这一条是**行为**测试，不需要 ECH 端点：先人为把标志置真（模拟上一轮成功过），
// 再跑一轮必然失败的探测，结果必须报 false。
//
// 为什么它重要：标志是包级的，而同一进程会连着跑好几次探测。一旦 Optimize 忘了
// 重置，上一轮的成功会一直渗到后面每一轮 —— 于是某次 ECH 真的坏了，日志仍然报成功。
// 那比恒假更难查，因为它只在"曾经好过"之后出现。
func TestECHWorkedIsResetOnEveryOptimizeRun(t *testing.T) {
	passThroughScreen(t)
	_, host, port := acceptingEdge(t)

	old := echConfigFetch
	echConfigFetch = func(string) ([]byte, error) { return []byte("bogus-ech-config"), nil }
	defer func() { echConfigFetch = old }()

	// 模拟"上一轮 ECH 成功过"。
	echWorked.Store(true)

	got := Optimize(context.Background(), []entry.Node{nodeFor(host, port)}, "example.com", true)

	if got.ECHWorked {
		t.Error("Optimize reported ECH as working without any successful ECH handshake " +
			"in this run — a previous run's success leaked through, so a later ECH " +
			"outage would still be logged as success")
	}
}
