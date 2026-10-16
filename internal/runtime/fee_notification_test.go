package runtime

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
	"github.com/zerodenet/zboard-pay-plugins/tdcloud"
)

func TestFeeSnapshotsDoNotReplaceInvoiceInNotificationsOrLookup(t *testing.T) {
	for _, tc := range []struct {
		name, noticeAmount, noticeCurrency, lookupAmount, lookupCurrency, payerCurrency string
		valid, legacy                                                                   bool
	}{
		{"buyer surcharge", "10.50", "USD", "10.50", "USD", "USD", true, false},
		{"TDC conversion", "10.50", "USD", "10.50", "USD", "TDC", true, false},
		{"legacy without fee fields", "10.50", "USD", "10.50", "USD", "", true, true},
		{"wrong notification amount", "10.51", "USD", "10.50", "USD", "USD", false, false},
		{"wrong notification currency", "10.50", "CNY", "10.50", "USD", "USD", false, false},
		{"wrong lookup amount", "10.50", "USD", "10.51", "USD", "USD", false, false},
		{"wrong lookup currency", "10.50", "USD", "10.50", "CNY", "USD", false, false},
		{"missing lookup amount", "10.50", "USD", "", "USD", "USD", false, false},
		{"missing notification amount", "", "USD", "10.50", "USD", "USD", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			invoice := func(amount, currency string) map[string]any {
				view := map[string]any{"order_no": "mp-fees", "external_reference": "zb-42", "amount": amount, "currency": currency,
					"status": "paid", "status_version": 3, "event_id": "paid-fees", "provider_trade_no": "trade-fees", "occurred_at": now}
				if !tc.legacy {
					// Even invalid invoices retain a payer total matching the business
					// order, so it cannot conceal a mismatched original amount/currency.
					view["payment_amount"], view["payment_currency"] = "10.50", tc.payerCurrency
					if tc.valid {
						view["payment_amount"] = "10.83"
						if tc.payerCurrency == "TDC" {
							view["payment_amount"] = "21.66000000"
						}
					}
					view["buyer_fee"], view["fee_status"], view["fee_version"] = "0.33", "quoted", 1
					view["gross_settlement_tdc"], view["merchant_fee_tdc"], view["settlement_tdc"] = "2100000000", "42000000", "2058000000"
				}
				return view
			}
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/token":
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 300})
				case "/openapi/v1/merchant-payments/mp-fees":
					_ = json.NewEncoder(w).Encode(invoice(tc.lookupAmount, tc.lookupCurrency))
				default:
					t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer provider.Close()
			const secret = "fees-notification-secret-123456789012345"
			settings, _ := json.Marshal(tdcloud.Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "secret",
				AmountMetadata: &tdcloud.AmountMetadata{Version: 2, Currency: "USD"},
				NotifyURL:      "https://board.example.test" + tdcloud.NotificationPath, NotifySecret: secret})
			snapshot, _ := snapshotChannel(channelConfig{Type: "tdcloud", Settings: settings})
			attempts := &checkoutAttempts{attempt: payment.Attempt{AccountID: "buyer", OrderID: 42, OrderTradeNo: "zb-42",
				AmountMinor: 1050, Currency: "USD", ChannelID: "tdcloud", ProviderOrderID: "mp-fees", ChannelSnapshot: snapshot,
				State: "failed", ProviderVersion: 2, Revision: 1}}
			facts := &checkoutFacts{events: map[string]bool{}}
			server := &Server{httpClient: provider.Client()}
			checkout, err := server.BindCheckout(CheckoutPorts{Orders: checkoutOrder{}, Facts: facts, Attempts: attempts, OrderReturnURL: checkoutReturnURL})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(map[string]any{"id": "fee-notice", "type": "merchant_payment.status_changed", "api_version": 1,
				"created_at": now, "data": invoice(tc.noticeAmount, tc.noticeCurrency)})
			timestamp := strconv.FormatInt(now.Unix(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			_, _ = mac.Write([]byte(timestamp + "."))
			_, _ = mac.Write(body)
			headers := http.Header{"X-Tdcloud-Event-Id": {"fee-notice"}, "X-Tdcloud-Timestamp": {timestamp},
				"X-Tdcloud-Signature": {"sha256=" + hex.EncodeToString(mac.Sum(nil))}}
			view, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, body, headers, now)
			if !tc.valid {
				if err == nil || len(facts.events) != 0 {
					t.Fatalf("payer or net amount concealed an invalid invoice: %+v %v", view, err)
				}
				return
			}
			if err != nil || view.State != "paid" || len(facts.events) != 1 {
				t.Fatalf("late payment with fees lost: %+v %v", view, err)
			}
			if _, err := checkout.ConfirmNotification(t.Context(), "buyer", 42, body, headers, now); err != nil {
				t.Fatal(err)
			}
			if _, err := checkout.Confirm(t.Context(), "buyer", 42); err != nil || len(facts.events) != 1 {
				t.Fatalf("replayed payment fulfilled twice: %v %+v", err, facts.events)
			}
		})
	}
}
