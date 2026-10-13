package tdcloud

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

// TDCloud owns invoice units. Missing provider metadata cannot be replaced
// by merchant input, cashier-method precision or an assumed two-decimal value.
var ErrCurrencyMetadataUnavailable = payment.ErrProviderMetadataUnavailable

const hostDecimals = 2
const maxProviderMinor int64 = 9007199254740991

// AmountMetadata is pinned inside private checkout settings before first
// create. Directory changes never change an ambiguous attempt's amount units.
type AmountMetadata struct {
	Version  int    `json:"version"`
	Currency string `json:"currency"`
	Decimals int    `json:"decimals"`
}

func validDecimals(decimals int) bool { return decimals >= 0 && decimals <= 8 }
func (m AmountMetadata) valid() bool {
	return m.Version == 1 && len(m.Currency) == 3 && m.Currency == strings.ToUpper(m.Currency) && validDecimals(m.Decimals)
}

func scaleMinor(amount int64, from, to int) (int64, error) {
	if amount <= 0 || !validDecimals(from) || !validDecimals(to) {
		return 0, payment.ErrAmountPrecision
	}
	factor := int64(1)
	if from < to {
		for n := from; n < to; n++ {
			factor *= 10
		}
		if amount > math.MaxInt64/factor {
			return 0, payment.ErrAmountPrecision
		}
		return amount * factor, nil
	}
	for n := to; n < from; n++ {
		factor *= 10
	}
	if amount%factor != 0 {
		return 0, payment.ErrAmountPrecision
	}
	return amount / factor, nil
}

func toProviderMinor(amount int64, decimals int) (int64, error) {
	value, err := scaleMinor(amount, hostDecimals, decimals)
	if err != nil || value > maxProviderMinor {
		return 0, payment.ErrAmountPrecision
	}
	return value, nil
}

func (c *Client) normalizeAmount(amount int64, currency string, decimals *int) (int64, error) {
	if decimals == nil || !validDecimals(*decimals) || amount <= 0 || amount > maxProviderMinor || len(currency) != 3 {
		return 0, payment.ErrConflict
	}
	if c.amount != nil && (!strings.EqualFold(currency, c.amount.Currency) || *decimals != c.amount.Decimals) {
		return 0, payment.ErrConflict
	}
	if c.legacyAmount && *decimals != hostDecimals {
		return 0, payment.ErrConflict
	}
	value, err := scaleMinor(amount, *decimals, hostDecimals)
	if err != nil {
		return 0, payment.ErrConflict
	}
	return value, nil
}

func (c *Client) resolveAmount(ctx context.Context, currency string) (AmountMetadata, error) {
	if c.amount != nil {
		if !strings.EqualFold(currency, c.amount.Currency) {
			return AmountMetadata{}, payment.ErrConflict
		}
		return *c.amount, nil
	}
	if c.legacyAmount {
		// Old attempts sent host minor units without conversion. Preserve that
		// exact request; never reinterpret its idempotency key with new units.
		return AmountMetadata{Version: 1, Currency: strings.ToUpper(currency), Decimals: hostDecimals}, nil
	}
	if len(c.currencies) == 0 {
		return AmountMetadata{}, ErrCurrencyMetadataUnavailable
	}
	decimals, ok := c.currencies[strings.ToUpper(currency)]
	if !ok {
		return AmountMetadata{}, payment.ErrConflict
	}
	return AmountMetadata{Version: 1, Currency: strings.ToUpper(currency), Decimals: decimals}, nil
}

func (c *Client) PrepareSnapshot(ctx context.Context, raw []byte, currency string) (payment.Channel, []byte, error) {
	settings, err := decodeSettings(raw)
	if err != nil {
		return nil, nil, err
	}
	if settings.AmountMetadata != nil {
		return nil, nil, errors.New("active config contains saved metadata")
	}
	metadata, err := c.resolveAmount(ctx, currency)
	if err != nil {
		return nil, nil, err
	}
	settings.PaymentNo = ""
	settings.AmountMetadata = &metadata
	snapshot, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := NewFromSnapshot(c.id, snapshot, c.http)
	if err != nil {
		return nil, nil, err
	}
	prepared.auth = c.auth // OAuth identity is unchanged; amount metadata is immutable.
	return prepared, snapshot, nil
}
