package tdcloud

import (
	"math"
	"strings"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

// Only persisted v1 or unversioned attempts use the former integer wire format.
// New attempts never select provider precision or convert provider minor units.
const hostDecimals = 2
const maxProviderMinor int64 = 9007199254740991

func validDecimals(decimals int) bool { return decimals >= 0 && decimals <= 8 }
func (c *Client) usesLegacyAmount() bool {
	return c.legacyAmount || (c.amount != nil && c.amount.Version == 1)
}
func (c *Client) legacyCreateAmount(amount int64) (int64, error) {
	decimals := hostDecimals
	if c.amount != nil {
		decimals = c.amount.Decimals
	}
	return toProviderMinor(amount, decimals)
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
