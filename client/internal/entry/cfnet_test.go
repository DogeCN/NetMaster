package entry

import "testing"

func TestIsCloudflareIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		{"104.16.0.1", true, "104.16.0.0/13 官方段"},
		{"104.18.33.154", true, "实测用过的入口"},
		{"172.66.1.228", true, "实测用过的入口"},
		{"131.0.72.1", true, "131.0.72.0/22 官方段"},
		{"162.158.1.1", true, "162.158.0.0/15 官方段"},
		{"8.8.8.8", false, "Google 不是 CF"},
		{"202.160.130.66", false, "污染 RR 里出现过的假 IP"},
		{"104.16.0.0", true, "段内边界：网络地址本身算在内"},
		{"not-an-ip", false, "解析失败按 false 处理"},
	}
	for _, c := range cases {
		if got := IsCloudflareIP(c.ip); got != c.want {
			t.Errorf("IsCloudflareIP(%s) = %v, want %v (%s)", c.ip, got, c.want, c.why)
		}
	}
}

func TestFilterCloudflareKeepsHostnames(t *testing.T) {
	in := []Node{
		{Addr: "104.16.1.1", Port: 443, Name: "good"},
		{Addr: "8.8.8.8", Port: 443, Name: "not-cf"},
		{Addr: "relay.example.com", Port: 443, Name: "hostname"},
	}
	out := FilterCloudflare(in)
	if len(out) != 2 {
		t.Fatalf("kept %d entries, want 2 (drop the non-CF IP, keep the IP and the hostname)", len(out))
	}
	if out[0].Addr != "104.16.1.1" || out[1].Addr != "relay.example.com" {
		t.Fatalf("kept %v / %v, want 104.16.1.1 and relay.example.com", out[0].Addr, out[1].Addr)
	}
}

// TestInterleaveRoundRobin 是 B1 的核心断言：名额在多个源之间**按比例稳定分配**，
// 而不是按到达顺序盲取前 N 条。修复前 pages.dev 单源 150 条 > 上限 64，每次启动
// 活下来的 64 条都不同 —— 节点池不可复现，任何 A/B 测量都失去前提。
func TestInterleaveRoundRobin(t *testing.T) {
	lists := [][]Node{
		mkNodes("a", 10),
		mkNodes("b", 3),
		mkNodes("c", 10),
	}
	got := Interleave(lists, 8)
	if len(got) != 8 {
		t.Fatalf("got %d entries, want 8", len(got))
	}
	counts := map[string]int{}
	order := []string{}
	for _, n := range got {
		if _, seen := counts[n.Name]; !seen {
			order = append(order, n.Name)
		}
		counts[n.Name]++
	}
	// 前 3 个必须是三个源各一个（轮转），而不是同一个源刷屏
	if len(order) != 3 {
		t.Fatalf("first pass covered %d sources (%v), want 3", len(order), order)
	}
	// 轮转 = 按轮均分，任意两个源的份数相差不超过 1（a=10/b=3/c=10 取 8 → 3/3/2）
	for _, pair := range [][2]string{{"a", "b"}, {"a", "c"}, {"b", "c"}} {
		d := counts[pair[0]] - counts[pair[1]]
		if d < 0 {
			d = -d
		}
		if d > 1 {
			t.Fatalf("shares %v are unbalanced (%d vs %d) — round-robin should differ by at most 1", pair, counts[pair[0]], counts[pair[1]])
		}
	}
	// 短源耗尽后名额要让给长源：把上限提到 12（超过三源总轮数的一半）时，
	// b 取尽之后 a/c 必须继续吃到名额，而不是留空。
	full := Interleave(lists, 12)
	fullCounts := map[string]int{}
	for _, n := range full {
		fullCounts[n.Name]++
	}
	if len(full) != 12 {
		t.Fatalf("got %d entries at limit 12, want 12 (b exhausted, its turns must go to a/c)", len(full))
	}
	if fullCounts["b"] != 3 {
		t.Fatalf("b contributed %d, want 3 (it only has 3)", fullCounts["b"])
	}
}

func TestInterleaveDeterministic(t *testing.T) {
	lists := [][]Node{mkNodes("a", 6), mkNodes("b", 6)}
	first := Interleave(lists, 7)
	for i := 0; i < 5; i++ {
		again := Interleave(lists, 7)
		for j := range first {
			if first[j].Addr != again[j].Addr {
				t.Fatalf("interleave is not deterministic at %d: %s vs %s", j, first[j].Addr, again[j].Addr)
			}
		}
	}
}

func mkNodes(prefix string, n int) []Node {
	out := make([]Node, n)
	for i := range out {
		name := prefix + string(rune('a'+i))
		out[i] = Node{Addr: name, Port: 443, Name: prefix}
	}
	return out
}
