package tdcloud

import (
	"fmt"
	"testing"
)

func TestConnectionDiagnosticsDistinguishScopeCredentialsAndEmptyCashier(t *testing.T) {
	for _, item := range []struct {
		err       error
		code      string
		connected bool
	}{
		{&APIError{StatusCode: 400, Code: "invalid_scope", OAuth: true}, "oauth_scope_missing", false},
		{&APIError{StatusCode: 401, Code: "invalid_client", OAuth: true}, "oauth_client_rejected", false},
		{&APIError{StatusCode: 400, Code: "unauthorized_client", OAuth: true}, "oauth_client_ineligible", false},
		{&APIError{StatusCode: 503, Code: "private-provider-text", OAuth: true}, "oauth_failed", false},
		{errCashierEmpty, "cashier_empty", true},
		{&APIError{StatusCode: 403, Code: "private-provider-text"}, "provider_access_denied", false},
		{&APIError{StatusCode: 503}, "provider_unavailable", false},
	} {
		code, connected := ConnectionDiagnostic(fmt.Errorf("wrapped: %w", item.err))
		if code != item.code || connected != item.connected {
			t.Fatalf("wrong diagnostic: %q %v", code, connected)
		}
	}
}
