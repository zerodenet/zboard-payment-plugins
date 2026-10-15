package tdcloud

import (
	"errors"
	"math"
	"net/http"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestDecimalInvoiceTextPreservesZBoardAmountsExactly(t *testing.T) {
	for _, item := range []struct {
		minor int64
		text  string
	}{
		{1, "0.01"}, {1000, "10.00"}, {1050, "10.50"}, {1500, "15.00"}, {math.MaxInt64, "92233720368547758.07"},
	} {
		text, err := formatHostAmount(item.minor)
		if err != nil || text != item.text {
			t.Fatalf("format %d: %q %v", item.minor, text, err)
		}
		minor, err := parseHostAmount(text)
		if err != nil || minor != item.minor {
			t.Fatalf("round trip %q: %d %v", text, minor, err)
		}
	}
	for _, text := range []string{"10.5", "10.50000000", "00010.500", "10.50"} {
		if minor, err := parseHostAmount(text); err != nil || minor != 1050 {
			t.Fatalf("equivalent decimal %q: %d %v", text, minor, err)
		}
	}
	for _, text := range []string{"", "0", "0.00", "-1.00", "+1.00", "1e1", " 10.50", "10.50 ", "10.", ".50", "10.5.0", "10.501", "10.50000001", "92233720368547758.08"} {
		if _, err := parseHostAmount(text); !errors.Is(err, payment.ErrConflict) {
			t.Fatalf("accepted %q: %v", text, err)
		}
	}
}

func TestModernResultsUseDecimalAmountWithoutPrecisionFallback(t *testing.T) {
	c := &Client{amount: &AmountMetadata{Version: 2, Currency: "TDC"}}
	for _, decimals := range []*int{nil, intPointer(0), intPointer(8)} {
		minor, err := c.resultAmount("10.50000000", 123, "TDC", decimals)
		if err != nil || minor != 1050 {
			t.Fatalf("derived provider units changed invoice: %d %v", minor, err)
		}
	}
	for _, text := range []string{"", "10.50000001"} {
		if _, err := c.resultAmount(text, 1050000000, "TDC", intPointer(8)); !errors.Is(err, payment.ErrConflict) {
			t.Fatalf("invalid decimal fell back to minor units: %v", err)
		}
	}
	if _, err := c.resultAmount("10.50", 1050, "USD", nil); !errors.Is(err, payment.ErrConflict) {
		t.Fatal("snapshot currency mismatch accepted")
	}
}

func TestOnlyFormalAmountRejectionsReleasePaymentAttempts(t *testing.T) {
	for _, code := range []string{"invalid_amount", "invalid_amount_precision"} {
		if !errors.Is(&APIError{StatusCode: http.StatusBadRequest, Code: code}, payment.ErrAmountPrecision) {
			t.Fatal("formal amount rejection not classified")
		}
	}
	if !errors.Is(&APIError{StatusCode: 422, Code: "currency_unavailable"}, payment.ErrCheckoutFailed) {
		t.Fatal("unsupported currency not classified")
	}
	for _, err := range []*APIError{{StatusCode: 500, Code: "invalid_amount"}, {StatusCode: 409, Code: "payment_conflict"}, {StatusCode: 503, Code: "server_error"}} {
		if errors.Is(err, payment.ErrAmountPrecision) || errors.Is(err, payment.ErrCheckoutFailed) {
			t.Fatal("ambiguous failure released attempt")
		}
	}
}
