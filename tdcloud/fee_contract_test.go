package tdcloud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestCheckoutKeepsInvoiceAmountWhenCashierAddsFeesOrConvertsCurrency(t *testing.T) {
	for _, tc := range []struct {
		name, amount, currency, payerAmount, payerCurrency string
		valid                                              bool
	}{
		{"buyer surcharge", "10.50", "USD", "10.83", "USD", true},
		{"TDC conversion", "10.50", "USD", "21.66000000", "TDC", true},
		{"payer total cannot repair wrong invoice", "10.51", "USD", "10.50", "USD", false},
		{"payer currency cannot repair wrong invoice", "10.50", "CNY", "10.50", "USD", false},
		{"payer total cannot replace missing invoice", "", "USD", "10.50", "USD", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/oauth/token" {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "merchant", "expires_in": 300})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/openapi/v1/merchant-payments" {
					t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if input["amount"] != "10.50" || input["currency"] != "USD" {
					t.Errorf("invoice request changed: %+v", input)
				}
				for _, field := range []string{"payment_no", "payment_amount", "payment_currency", "buyer_fee", "settlement_tdc"} {
					if _, present := input[field]; present {
						t.Errorf("plugin selected cashier terms: %s", field)
					}
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"order_no": "mp-fees", "external_reference": "zb-fees", "amount": tc.amount, "currency": tc.currency,
					"status": "pending", "checkout_url": "https://cashier.example.test/mp-fees",
					"fee_status": "quoted", "fee_version": 1, "buyer_fee": "0.33",
					"payment_amount": tc.payerAmount, "payment_currency": tc.payerCurrency,
					"gross_settlement_tdc": "2100000000", "merchant_fee_tdc": "42000000", "settlement_tdc": "2058000000",
				})
			}))
			defer provider.Close()
			raw, _ := json.Marshal(Settings{APIURL: provider.URL, ClientID: "merchant", ClientSecret: "secret",
				AmountMetadata: &AmountMetadata{Version: 2, Currency: "USD"}})
			client, err := NewFromSnapshot("tdcloud", raw, provider.Client())
			if err != nil {
				t.Fatal(err)
			}
			checkout, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{
				Reference: "zb-fees", IdempotencyKey: "fees-attempt", AmountMinor: 1050, Currency: "USD",
			})
			if tc.valid && (err != nil || checkout.ProviderOrderID != "mp-fees") {
				t.Fatalf("fee quote replaced the invoice: %+v %v", checkout, err)
			}
			if !tc.valid && err == nil {
				t.Fatal("cashier total or net settlement accepted as invoice evidence")
			}
		})
	}
}
