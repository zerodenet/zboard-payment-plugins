package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

type storage struct{ host *pluginv1.HostStorage }

func openStorage() (*storage, error) {
	host, err := pluginv1.HostStorageFromEnvironment()
	if err != nil {
		return nil, err
	}
	return &storage{host: host}, nil
}
func (s *storage) Close() { s.host.Close() }

func accountKey(actor string) string {
	h := sha256.Sum256([]byte(actor))
	return "account." + hex.EncodeToString(h[:])
}

func attemptKey(actor string, id uint64) string {
	return "attempt." + accountKey(actor)[8:] + "." + strconv.FormatUint(id, 10)
}

func attemptBucket(actor string) string { return "attempts." + accountKey(actor)[8:9] }

func providerOrderKey(providerType, orderNo string) string {
	h := sha256.Sum256([]byte(providerType + "\x00" + orderNo))
	return "provider-order." + hex.EncodeToString(h[:])
}

func providerOrderBucket(providerType, orderNo string) string {
	return "provider-orders." + providerOrderKey(providerType, orderNo)[15:16]
}

type providerOrderLocation struct {
	ProviderType    string `json:"provider_type"`
	ProviderOrderID string `json:"provider_order_id"`
	AccountID       string `json:"account_id"`
	OrderID         uint64 `json:"order_id"`
}

func (s *storage) ensureProviderOrderIndex(ctx context.Context, attempt payment.Attempt) error {
	if attempt.ProviderOrderID == "" {
		return nil
	}
	if len(attempt.ChannelSnapshot) == 0 {
		return nil
	}
	var snapshot providerSnapshot
	if err := json.Unmarshal(attempt.ChannelSnapshot, &snapshot); err != nil || snapshot.Type == "" {
		return payment.ErrConflict
	}
	key := providerOrderKey(snapshot.Type, attempt.ProviderOrderID)
	bucket := providerOrderBucket(snapshot.Type, attempt.ProviderOrderID)
	for range 6 {
		current, err := s.getValue(ctx, bucket)
		if err != nil {
			return err
		}
		locations := map[string]providerOrderLocation{}
		if current.Found {
			if err := json.Unmarshal(current.Value, &locations); err != nil {
				return payment.ErrConflict
			}
		}
		if saved, found := locations[key]; found {
			if saved.ProviderType != snapshot.Type || saved.ProviderOrderID != attempt.ProviderOrderID ||
				saved.AccountID != attempt.AccountID || saved.OrderID != attempt.OrderID {
				return payment.ErrConflict
			}
			return nil
		}
		locations[key] = providerOrderLocation{snapshot.Type, attempt.ProviderOrderID, attempt.AccountID, attempt.OrderID}
		raw, _ := json.Marshal(locations)
		if _, err := s.putValue(ctx, bucket, current.Revision, raw); err == nil {
			return nil
		}
	}
	return fmt.Errorf("persist provider order index: %w", payment.ErrConflict)
}

func (s *storage) FindByProviderOrder(ctx context.Context, providerType, orderNo string) (payment.Attempt, error) {
	if providerType == "" || orderNo == "" || len(orderNo) > 128 {
		return payment.Attempt{}, payment.ErrNotFound
	}
	value, err := s.getValue(ctx, providerOrderBucket(providerType, orderNo))
	if err != nil {
		return payment.Attempt{}, err
	}
	if !value.Found {
		return payment.Attempt{}, payment.ErrNotFound
	}
	var locations map[string]providerOrderLocation
	if err := json.Unmarshal(value.Value, &locations); err != nil {
		return payment.Attempt{}, payment.ErrConflict
	}
	location, found := locations[providerOrderKey(providerType, orderNo)]
	if !found {
		return payment.Attempt{}, payment.ErrNotFound
	}
	if location.ProviderType != providerType || location.ProviderOrderID != orderNo || location.AccountID == "" || location.OrderID == 0 {
		return payment.Attempt{}, payment.ErrConflict
	}
	attempt, err := s.Get(ctx, location.AccountID, location.OrderID)
	if err != nil {
		return payment.Attempt{}, err
	}
	if attempt.ProviderOrderID != orderNo {
		found := false
		for _, historical := range attempt.History {
			if historical.ProviderOrderID != orderNo {
				continue
			}
			var old providerSnapshot
			if json.Unmarshal(historical.ChannelSnapshot, &old) != nil || old.Type != providerType {
				continue
			}
			attempt, found = historical, true
			break
		}
		if !found {
			return payment.Attempt{}, payment.ErrHistoricalUnavailable
		}
	}
	var snapshot providerSnapshot
	if err := json.Unmarshal(attempt.ChannelSnapshot, &snapshot); err != nil || snapshot.Type != providerType {
		return payment.Attempt{}, payment.ErrConflict
	}
	return attempt, nil
}

func (s *storage) Get(ctx context.Context, actor string, id uint64) (payment.Attempt, error) {
	value, err := s.getValue(ctx, attemptBucket(actor))
	if err != nil {
		return payment.Attempt{}, err
	}
	if !value.Found {
		return payment.Attempt{}, payment.ErrNotFound
	}
	var attempts map[string]payment.Attempt
	if err := json.Unmarshal(value.Value, &attempts); err != nil {
		return payment.Attempt{}, err
	}
	attempt, ok := attempts[attemptKey(actor, id)]
	if !ok {
		return payment.Attempt{}, payment.ErrNotFound
	}
	if attempt.AccountID != actor || attempt.OrderID != id {
		return payment.Attempt{}, payment.ErrConflict
	}
	return attempt, nil
}

func (s *storage) Put(ctx context.Context, attempt payment.Attempt) (payment.Attempt, error) {
	if attempt.AccountID == "" || attempt.OrderID == 0 {
		return payment.Attempt{}, payment.ErrConflict
	}
	key := attemptBucket(attempt.AccountID)
	for range 6 {
		current, err := s.getValue(ctx, key)
		if err != nil {
			return payment.Attempt{}, err
		}
		attempts := map[string]payment.Attempt{}
		if current.Found {
			if err := json.Unmarshal(current.Value, &attempts); err != nil {
				return payment.Attempt{}, err
			}
		}
		saved, found := attempts[attemptKey(attempt.AccountID, attempt.OrderID)]
		if found {
			if saved.Revision != attempt.Revision {
				return payment.Attempt{}, payment.ErrConflict
			}
		} else if attempt.Revision != 0 {
			return payment.Attempt{}, payment.ErrConflict
		}
		// Write the reverse pointer before publishing the attempt's provider ID.
		// A crash between these writes leaves only a harmless stale pointer,
		// while a callback never sees an unindexed published provider order.
		if err := s.ensureProviderOrderIndex(ctx, attempt); err != nil {
			return payment.Attempt{}, err
		}
		for _, historical := range attempt.History {
			if historical.AccountID != attempt.AccountID || historical.OrderID != attempt.OrderID || len(historical.History) != 0 {
				return payment.Attempt{}, payment.ErrConflict
			}
			if err := s.ensureProviderOrderIndex(ctx, historical); err != nil {
				return payment.Attempt{}, err
			}
		}
		next := attempt
		next.Revision++
		attempts[attemptKey(attempt.AccountID, attempt.OrderID)] = next
		raw, err := json.Marshal(attempts)
		if err != nil {
			return payment.Attempt{}, err
		}
		if _, err := s.putValue(ctx, key, current.Revision, raw); err == nil {
			return next, nil
		}
	}
	return payment.Attempt{}, fmt.Errorf("persist payment attempt: %w", payment.ErrConflict)
}
