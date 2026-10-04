package selector

import "testing"

// TestShouldTryFragDirectMemory 钉住"要不要先赌一次分片直连"的三态。
//
// 这个判据决定的是**每个新域名**要不要付一次分片直连的探测成本（成功即 2 跳、
// 失败落隧道 3 跳），所以它的三态必须精确：默认值得、被隧道证实过就不值得、
// 分片验证过就值得且优先级更高。
func TestShouldTryFragDirectMemory(t *testing.T) {
	p := New(Config{
		Nodes:     repeatNodes("127.0.0.1", 1, 1),
		SNI:       "example.com",
		Password:  "pw",
		MuxTarget: 1,
	})
	defer p.Close()

	const host = "a.example:443"

	// 1. 没有记忆 → 值得试。用户装这个软件本身就说明普通直连不通，而分片是另一条路。
	if !p.ShouldTryFragDirect(host) {
		t.Fatal("no memory must mean 'worth one try'")
	}

	// 2. 隧道真的送出过字节 → 30 分钟内不再试，别让每个连接都付探测成本。
	p.NoteProxyConfirmed(host)
	if p.ShouldTryFragDirect(host) {
		t.Fatal("a host the tunnel has actually served must not keep paying the direct probe")
	}

	// 3. 分片验证过 → 值得再试，而且这条记忆优先于上面那条冷却。
	//    按冷却把它丢给代理，等于把已经付过代价学到的能力作废。
	p.NoteFragDirect(host)
	if !p.ShouldTryFragDirect(host) {
		t.Fatal("a host where fragmentation worked must be tried again (fragDirect beats directBlocked)")
	}
}
