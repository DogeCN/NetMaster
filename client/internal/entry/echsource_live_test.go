package entry

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
)

// TestLiveSourcesUseECH 是"入口源真的在用 ECH"的端到端钉子。
//
// 做法：把**分片那条退路堵死**（换成一个必然失败的 fragClient），然后拉社区源。
// 如果 ECH 没生效，这里会一个候选都拿不到 —— 于是"ECH 优先"就不是文档里的一句话，
// 而是一条会红的判据。
//
// 需要真网络，默认跳过（与项目的 E2E 用例同一惯例）：
//
//	cd client && NETMASTER_LIVE_SOURCES=1 go test ./internal/entry -run TestLiveSourcesUseECH -v
func TestLiveSourcesUseECH(t *testing.T) {
	if os.Getenv("NETMASTER_LIVE_SOURCES") == "" {
		t.Skip("set NETMASTER_LIVE_SOURCES=1 to run (needs the real network)")
	}
	old := fragClient
	defer func() { fragClient = old }()
	fragClient = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("fragmentation path disabled by the test")
	})}

	nodes, src := Community(context.Background())
	if len(nodes) == 0 {
		t.Fatalf("no candidates with the fragmented path disabled: ECH did not carry the fetch (source=%q)", src)
	}
	t.Logf("ECH-only fetch: %d candidates (source=%q)", len(nodes), src)
}
