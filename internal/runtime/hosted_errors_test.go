package runtime

import (
	"errors"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	pluginv1 "github.com/zerodenet/zboard/backend/pkg/pluginapi/v1"
)

func TestHostedBusinessRejectionsUseExplicitPublicSDKContract(t *testing.T) {
	for _, test := range []struct {
		err       error
		code      string
		retryable bool
	}{
		{payment.ErrCheckoutExpiryPending, "checkout_expiry_pending", true},
		{payment.ErrAmountPrecision, "amount_unrepresentable", false},
		{payment.ErrCheckoutFailed, "checkout_failed", true},
		{payment.ErrConflict, "conflict", false},
		{payment.ErrNotFound, "not_found", true},
	} {
		public, ok := pluginv1.ParseActionError(hostedError(test.err))
		if !ok || public.Code != test.code || public.Retryable != test.retryable || public.Message == "" {
			t.Fatalf("invalid public rejection: %+v", public)
		}
	}
	for _, err := range []error{errors.New("private provider secret"), &pluginv1.HostCallError{Code: "forbidden"}} {
		if _, ok := pluginv1.ParseActionError(hostedError(err)); ok {
			t.Fatal("host/infrastructure failure became public business rejection")
		}
	}
}
