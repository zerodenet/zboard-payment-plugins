package tdcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

// Version 2 pins the decimal wire contract and invoice currency, not TDCloud
// precision. Version 1 is read only for already-issued integer-wire attempts.
type AmountMetadata struct {
	Version  int    `json:"version"`
	Currency string `json:"currency"`
	Decimals int    `json:"decimals,omitempty"`
}

func validCurrency(code string) bool {
	if len(code) != 3 {
		return false
	}
	for _, c := range code {
		if c < 'A' || c > 'Z' {
			return false
		}
	}
	return true
}
func (m AmountMetadata) valid() bool {
	return validCurrency(m.Currency) && ((m.Version == 1 && validDecimals(m.Decimals)) || (m.Version == 2 && m.Decimals == 0))
}

// ZBoard's known cents become decimal text with no provider-unit conversion.
func formatHostAmount(minor int64) (string, error) {
	if minor <= 0 {
		return "", payment.ErrAmountPrecision
	}
	return fmt.Sprintf("%d.%02d", minor/100, minor%100), nil
}

// TDCloud returns ordinary decimal invoice text (including trailing zeros).
// Parse exactly into ZBoard cents without float arithmetic or rounding.
func parseHostAmount(value string) (int64, error) {
	if len(value) == 0 || len(value) > 64 {
		return 0, payment.ErrConflict
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 {
		return 0, payment.ErrConflict
	}
	for _, part := range parts {
		if part == "" {
			return 0, payment.ErrConflict
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, payment.ErrConflict
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	if len(fraction) > 2 {
		return 0, payment.ErrConflict
	}
	digits := strings.TrimLeft(parts[0]+fraction+strings.Repeat("0", 2-len(fraction)), "0")
	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || minor <= 0 {
		return 0, payment.ErrConflict
	}
	return minor, nil
}

func (c *Client) resultAmount(amount string, minor int64, currency string, decimals *int) (int64, error) {
	if !validCurrency(currency) || (c.amount != nil && c.amount.Currency != currency) {
		return 0, payment.ErrConflict
	}
	if c.usesLegacyAmount() {
		value, err := c.normalizeAmount(minor, currency, decimals)
		if err != nil {
			return 0, err
		}
		if amount != "" {
			parsed, err := parseHostAmount(amount)
			if err != nil || parsed != value {
				return 0, payment.ErrConflict
			}
		}
		return value, nil
	}
	return parseHostAmount(amount)
}

func (c *Client) PrepareSnapshot(_ context.Context, raw []byte, currency string) (payment.Channel, []byte, error) {
	settings, err := decodeSettings(raw)
	if err != nil {
		return nil, nil, err
	}
	if settings.AmountMetadata != nil {
		return nil, nil, errors.New("active config contains saved metadata")
	}
	if !validCurrency(currency) {
		return nil, nil, payment.ErrConflict
	}
	settings.PaymentNo = ""
	settings.Currencies = nil
	settings.AmountMetadata = &AmountMetadata{Version: 2, Currency: currency}
	snapshot, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := NewFromSnapshot(c.id, snapshot, c.http)
	if err != nil {
		return nil, nil, err
	}
	prepared.auth = c.auth
	return prepared, snapshot, nil
}
