package tlsutil

import (
	"errors"
	"fmt"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// retryConfigFrom 应只对带 RetryConfigList 的 ECHRejectionError 给出新配置。
func TestRetryConfigFrom(t *testing.T) {
	retry := []byte{0xfe, 0x0d, 0x00, 0x01}
	if got, ok := retryConfigFrom(&utls.ECHRejectionError{RetryConfigList: retry}); !ok || string(got) != string(retry) {
		t.Fatalf("ECHRejectionError 未提取出 RetryConfigList: ok=%v got=%v", ok, got)
	}
	// 空 RetryConfigList 是"服务端明确的拒绝信号"，不算可自愈。
	if _, ok := retryConfigFrom(&utls.ECHRejectionError{}); ok {
		t.Fatalf("空 RetryConfigList 不应被判为可自愈")
	}
	wrapped := fmt.Errorf("handshake: %w", &utls.ECHRejectionError{RetryConfigList: retry})
	if _, ok := retryConfigFrom(wrapped); !ok {
		t.Fatalf("包装后的 ECHRejectionError 应能被 errors.As 解出")
	}
	if _, ok := retryConfigFrom(errors.New("normal failure")); ok {
		t.Fatalf("普通错误不应被判为可自愈")
	}
}

// storeECHRetryConfig 写入的配置必须被 FetchECHConfigList（默认 key）直接捡走，
// 而不是等 TTL 过期重新打 DoH。
func TestStoreECHRetryConfigFeedsCache(t *testing.T) {
	domain := "self-heal.test"
	retry := []byte{0x01, 0x02, 0x03}
	storeECHRetryConfig(domain, retry)
	got, err := FetchECHConfigList(domain, "")
	if err != nil {
		t.Fatalf("fetch after store: %v", err)
	}
	if string(got) != string(retry) {
		t.Fatalf("缓存里的配置未被 RetryConfig 替换: got=%v", got)
	}
}
