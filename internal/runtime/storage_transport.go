package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

// The current storage SDK represents lifecycle contention as an HTTP 503
// error. Retry only that response; revision conflicts must reload the value.
func retryStorage(ctx context.Context, call func() (pluginv1.StoredValue, error)) (pluginv1.StoredValue, error) {
	for attempt := 0; ; attempt++ {
		value, err := call()
		if err == nil || !strings.HasSuffix(err.Error(), "(HTTP 503)") || attempt == 5 {
			return value, err
		}
		timer := time.NewTimer(time.Duration(1<<attempt) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return pluginv1.StoredValue{}, ctx.Err()
		case <-timer.C:
		}
	}
}
func (s *storage) getValue(ctx context.Context, key string) (pluginv1.StoredValue, error) {
	return retryStorage(ctx, func() (pluginv1.StoredValue, error) { return s.host.Get(ctx, key) })
}
func (s *storage) putValue(ctx context.Context, key string, revision uint64, raw json.RawMessage) (pluginv1.StoredValue, error) {
	return retryStorage(ctx, func() (pluginv1.StoredValue, error) { return s.host.Put(ctx, key, revision, raw) })
}
