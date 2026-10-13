package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func authorityBucket(id uint64) string {
	h := sha256.Sum256([]byte(strconv.FormatUint(id, 10)))
	return "authorities." + hex.EncodeToString(h[:1])[:1]
}

// This private index locates historical attempts only. Callers must first
// authorize the selected order through the host; an index grants no access.
func (s *storage) rememberAuthority(ctx context.Context, id uint64, actor string) error {
	key := strconv.FormatUint(id, 10)
	for range 6 {
		value, err := s.getValue(ctx, authorityBucket(id))
		if err != nil {
			return err
		}
		actors := map[string]string{}
		if value.Found && json.Unmarshal(value.Value, &actors) != nil {
			return payment.ErrConflict
		}
		if saved := actors[key]; saved != "" {
			if saved != actor {
				return payment.ErrConflict
			}
			return nil
		}
		actors[key] = actor
		raw, _ := json.Marshal(actors)
		if _, err := s.putValue(ctx, authorityBucket(id), value.Revision, raw); err == nil {
			return nil
		}
	}
	return payment.ErrConflict
}

func (s *storage) historicalAttempt(ctx context.Context, id uint64) (payment.Attempt, error) {
	value, err := s.getValue(ctx, authorityBucket(id))
	if err != nil {
		return payment.Attempt{}, err
	}
	if !value.Found {
		return payment.Attempt{}, payment.ErrNotFound
	}
	var actors map[string]string
	if json.Unmarshal(value.Value, &actors) != nil {
		return payment.Attempt{}, payment.ErrConflict
	}
	actor := actors[strconv.FormatUint(id, 10)]
	if actor == "" {
		return payment.Attempt{}, payment.ErrNotFound
	}
	return s.Get(ctx, actor, id)
}
