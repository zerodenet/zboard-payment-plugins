package tdcloud

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

func TestOptionalDescriptionReturnAndExpiryFollowPlatformNormalization(t *testing.T) {
	for _, options := range []struct {
		description, returned string
		seconds               *int64
	}{
		{}, {description: " \t\n ", returned: " \t "},
		{description: " Monthly plan ", returned: " https://board.example/orders/7?x=A%20B#payment ", seconds: expiryPointer(60)},
		{seconds: expiryPointer(1800)}, {seconds: expiryPointer(7200)},
	} {
		p := newContractFixture(t, false, 0)
		raw, _ := json.Marshal(Settings{APIURL: p.server.URL, ClientID: "merchant", ClientSecret: "secret", ExpiresIn: options.seconds})
		client, err := NewFromSettings("tdcloud", raw, p.server.Client())
		if err != nil {
			t.Fatal(err)
		}
		view, err := client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "USD", Description: options.description, ReturnURL: options.returned})
		if err != nil {
			t.Fatalf("optional checkout: %v", err)
		}
		body := p.lastBody()
		if options.description == "" || options.description == " \t\n " {
			if body["description"] != nil {
				t.Fatal("empty description became mandatory")
			}
		}
		if body["amount_minor"] != nil || body["payment_no"] != nil {
			t.Fatal("new order selected provider units or method")
		}
		seconds := int64(1800)
		if options.seconds != nil {
			seconds = *options.seconds
		}
		if view.ExpiresAt.Before(time.Now().Add(time.Duration(seconds-3) * time.Second)) {
			t.Fatal("wrong platform expiry")
		}
		if options.seconds == nil && body["expires_in"] != nil {
			t.Fatal("default expiry must remain omitted")
		}
	}
}

func TestOptionalNullDefaultsAndNotificationChoices(t *testing.T) {
	for _, item := range []struct {
		options  string
		enabled  string
		defaults bool
	}{
		{``, `false`, true},
		{`,"expires_in":null,"notify_enabled":null,"notify_url":null,"notify_secret":null`, `false`, true},
		{`,"notify_enabled":false`, `false`, true},
		{`,"notify_enabled":true,"notify_mode":"merchant_default","notify_url":"https://board.example/.well-known/zboard-payments/v1/tdcloud-notify","notify_secret":"platform-issued-notification-key-123456789"`, `true`, true},
		{`,"notify_enabled":true,"notify_url":"https://board.example/.well-known/zboard-payments/v1/tdcloud-notify","notify_secret":"explicit-notification-key-123456789"`, `true`, false},
	} {
		p := newContractFixture(t, item.defaults, 0)
		raw := []byte(`{"api_url":"` + p.server.URL + `","client_id":"merchant","client_secret":"secret"` + item.options + `}`)
		normalized, err := NormalizeSettings(raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewFromSettings("tdcloud", normalized, p.server.Client())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = client.CreateCheckout(t.Context(), payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "USD"}); err != nil {
			t.Fatal(err)
		}
		body := p.lastBody()
		if string(body["notify_enabled"]) != item.enabled {
			t.Fatal("notification choice not pinned")
		}
		if item.enabled == "false" && (body["notify_url"] != nil || body["notify_secret"] != nil) {
			t.Fatal("disabled notification leaked override")
		}
	}
	for _, options := range []string{`,"expires_in":0`, `,"expires_in":59`, `,"expires_in":7201`, `,"notify_enabled":true`, `,"notify_enabled":false,"notify_url":"https://board.example/.well-known/zboard-payments/v1/tdcloud-notify","notify_secret":"explicit-notification-key-123456789"`} {
		if _, err := NormalizeSettings([]byte(`{"api_url":"https://cloud.example","client_id":"merchant","client_secret":"secret"`+options+`}`), nil); err == nil {
			t.Fatalf("invalid optional configuration accepted: %s", options)
		}
	}
}

func TestPlatformOptionalNullAndOmissionShareDefaultIdempotency(t *testing.T) {
	p := newContractFixture(t, false, 0)
	client, _ := New("tdcloud", p.server.URL, "merchant", "secret", p.server.Client())
	base := map[string]any{"external_reference": "zb", "idempotency_key": "same-key", "amount": "10.50", "currency": "USD"}
	for _, fields := range []map[string]any{{"description": nil, "return_url": nil, "expires_in": nil, "notify_enabled": nil}, {"description": " \t ", "return_url": " "}, {"expires_in": int64(1800)}} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		for k, v := range fields {
			body[k] = v
		}
		var view merchantPayment
		if err := client.request(t.Context(), "POST", "/openapi/v1/merchant-payments", body, 201, &view); err != nil || view.OrderNo != "mp" {
			t.Fatalf("equivalent defaults conflicted: %v", err)
		}
	}
	base["expires_in"] = 0
	var view merchantPayment
	err := client.request(t.Context(), "POST", "/openapi/v1/merchant-payments", base, 201, &view)
	var api *APIError
	if !errors.As(err, &api) || api.Code != "invalid_expires_in" {
		t.Fatal("zero expiry was defaulted instead of rejected")
	}
}

func TestExpiryAndNotificationChoiceSurviveSnapshotAndAmbiguousRetry(t *testing.T) {
	p := newContractFixture(t, false, 503)
	seconds := int64(60)
	raw, _ := json.Marshal(Settings{APIURL: p.server.URL, ClientID: "merchant", ClientSecret: "secret", ExpiresIn: &seconds})
	normalized, _ := NormalizeSettings(raw, nil)
	client, _ := NewFromSettings("tdcloud", normalized, p.server.Client())
	prepared, snapshot, err := client.PrepareSnapshot(t.Context(), normalized, "USD")
	if err != nil {
		t.Fatal(err)
	}
	r := payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "USD"}
	if _, err = prepared.CreateCheckout(t.Context(), r); err == nil {
		t.Fatal("ambiguous response accepted")
	}
	restored, err := NewFromSnapshot("tdcloud", snapshot, p.server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restored.CreateCheckout(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if string(p.lastBody()["expires_in"]) != "60" || string(p.lastBody()["notify_enabled"]) != "false" {
		t.Fatal("restart changed optional request")
	}
	client.expiresIn = expiryPointer(7200)
	if _, err = client.CreateCheckout(t.Context(), r); err == nil {
		t.Fatal("fixture ignored changed expiry under original key")
	}
}

func TestOptionalNormalizationDoesNotRewriteIdentityOrAcceptInvalidURLs(t *testing.T) {
	client, _ := New("tdcloud", "https://cloud.example", "merchant", "secret", nil)
	base := payment.CheckoutRequest{Reference: "zb", IdempotencyKey: "key", AmountMinor: 1050, Currency: "USD"}
	for _, raw := range []string{"http://board.example/receipt", "https://user:secret@board.example/receipt", "https://board.example:0/receipt", "https://board.example:65536/receipt", "https://board.example/path with spaces"} {
		r := base
		r.ReturnURL = raw
		if _, err := client.CreateCheckout(t.Context(), r); !errors.Is(err, payment.ErrConflict) {
			t.Fatalf("invalid URL reached network: %v", err)
		}
	}
	for _, field := range []string{"reference", "key"} {
		r := base
		if field == "reference" {
			r.Reference = " zb "
		} else {
			r.IdempotencyKey = " key "
		}
		if _, err := client.CreateCheckout(t.Context(), r); err == nil {
			t.Fatal("business identity was silently normalized")
		}
	}
}

func expiryPointer(value int64) *int64 { return &value }
