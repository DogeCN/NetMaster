package main

import (
	"netmaster/internal/rules"
	"testing"
)

// --rules 的动作必须由文件名后缀明确给出。
//
// 先前的实现按"文件名里含不含 direct"来猜，`myrules.list` 会被静默当成
// proxy —— 用户完全不知道自己的文件被当成什么处理了。现在猜不出就 fatal，
// 所以"猜得出"与"猜不出"的边界本身要有测试。
func TestRulesActionFromFile(t *testing.T) {
	cases := []struct {
		path string
		want rules.Action
		ok   bool
	}{
		{"mylist.direct", rules.Direct, true},
		{"mylist.proxy", rules.Proxy, true},
		{"/abs/path/gfw.proxy", rules.Proxy, true},
		{"MyList.DIRECT", rules.Direct, true}, // 大小写不敏感
		{`C:\rules\my.proxy`, rules.Proxy, true},

		// 猜不出：不能静默兜底
		{"myrules.list", "", false},
		{"myrules", "", false},
		{"direct", "", false}, // 含 direct 但不是后缀
		{"direct.txt", "", false},
		{"", "", false},
		// 旧实现会命中的陷阱：文件名里出现 direct 但语义上是别的词
		{"not-direct-at-all.proxy", rules.Proxy, true}, // 后缀优先，正确
		{"proxy.direct", rules.Direct, true},           // 后缀说了算，不看内容
	}
	for _, c := range cases {
		got, ok := rulesActionFromFile(c.path)
		if ok != c.ok || (c.ok && got != c.want) {
			t.Errorf("rulesActionFromFile(%q) = (%q, %v), want (%q, %v)",
				c.path, got, ok, c.want, c.ok)
		}
	}
}
