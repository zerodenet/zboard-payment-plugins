package tdcloud

import "errors"

var errCashierEmpty = errors.New("TDCloud cashier has no available payment methods")

func (*Client) LookupProblem(err error) string {
	var remote *APIError
	if errors.As(err, &remote) {
		if remote.OAuth {
			return "provider_oauth_failed"
		}
		switch remote.StatusCode {
		case 401, 403:
			return "provider_access_denied"
		case 404:
			return "provider_order_not_found"
		}
	}
	return "provider_lookup_failed"
}

// ConnectionDiagnostic exposes fixed, non-secret categories to the common
// configuration view. Provider response bodies and arbitrary error text stay private.
func ConnectionDiagnostic(err error) (code string, connected bool) {
	if errors.Is(err, errCashierEmpty) {
		return "cashier_empty", true
	}
	var remote *APIError
	if errors.As(err, &remote) {
		if remote.OAuth {
			switch remote.Code {
			case "invalid_scope":
				return "oauth_scope_missing", false
			case "invalid_client":
				return "oauth_client_rejected", false
			case "unauthorized_client":
				return "oauth_client_ineligible", false
			}
			return "oauth_failed", false
		}
		if remote.StatusCode == 401 || remote.StatusCode == 403 {
			return "provider_access_denied", false
		}
		return "provider_unavailable", false
	}
	return "connection_failed", false
}
