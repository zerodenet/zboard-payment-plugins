package tdcloud

import (
	"errors"
	"math"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func intPointer(value int) *int { return &value }

func TestMinorUnitsConvertExactlyWithoutRoundingOrOverflow(t *testing.T) {
	for _, item := range []struct {
		name     string
		host     int64
		decimals int
		provider int64
	}{
		{"USD", 1050, 2, 1050}, {"TDC", 1050, 8, 1050000000}, {"JPY", 1000, 0, 10}, {"precision1", 1050, 1, 105}, {"precision3", 1050, 3, 10500}, {"max-provider", 9007199254, 8, 9007199254000000},
	} {
		t.Run(item.name, func(t *testing.T) {
			actual, err := toProviderMinor(item.host, item.decimals)
			if err != nil || actual != item.provider {
				t.Fatalf("create amount=%d err=%v", actual, err)
			}
			back, err := scaleMinor(actual, item.decimals, hostDecimals)
			if err != nil || back != item.host {
				t.Fatalf("lookup amount=%d err=%v", back, err)
			}
		})
	}
	for _, item := range []struct {
		amount   int64
		decimals int
	}{{1050, 0}, {1051, 1}, {9007199255, 8}, {math.MaxInt64, 8}, {maxProviderMinor + 1, 2}, {0, 2}, {-1, 2}, {1050, -1}, {1050, 9}} {
		if _, err := toProviderMinor(item.amount, item.decimals); !errors.Is(err, payment.ErrAmountPrecision) {
			t.Fatalf("accepted %+v: %v", item, err)
		}
	}
	client := &Client{amount: &AmountMetadata{Version: 1, Currency: "TDC", Decimals: 8}}
	for _, item := range []struct {
		amount   int64
		currency string
		decimals *int
	}{{1050000001, "TDC", intPointer(8)}, {1050000000, "TDC", intPointer(2)}, {1050000000, "USD", intPointer(8)}, {1050000000, "TDC", nil}, {maxProviderMinor + 1, "TDC", intPointer(8)}} {
		if _, err := client.normalizeAmount(item.amount, item.currency, item.decimals); !errors.Is(err, payment.ErrConflict) {
			t.Fatalf("accepted conflicting result %+v: %v", item, err)
		}
	}
	legacy := &Client{legacyAmount: true}
	if _, err := legacy.normalizeAmount(1050000000, "TDC", intPointer(8)); !errors.Is(err, payment.ErrConflict) {
		t.Fatal("legacy snapshot silently reinterpreted amount units")
	}
}
