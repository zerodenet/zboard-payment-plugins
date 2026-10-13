package tdcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zerodenet/zboard-pay-plugins/payment"
)

type Client struct {
	id              string
	baseURL         string
	clientID        string
	secret          string
	legacyPaymentNo string
	notifyURL       string
	notifySecret    string
	notifyMode      string
	notifyEnabled   *bool
	http            *http.Client
	auth            *merchantToken
	amount          *AmountMetadata
	legacyAmount    bool
	currencies      map[string]int
}

type merchantToken struct {
	mu    sync.Mutex
	token string
	end   time.Time
}

type merchantPayment struct {
	OrderNo          string    `json:"order_no"`
	ExternalRef      string    `json:"external_reference"`
	ReturnURL        string    `json:"return_url"`
	PaymentNo        string    `json:"payment_no"`
	AmountMinor      int64     `json:"amount_minor"`
	Currency         string    `json:"currency"`
	CurrencyDecimals *int      `json:"currency_decimals"`
	Status           string    `json:"status"`
	CheckoutURL      string    `json:"checkout_url"`
	ExpiresAt        time.Time `json:"expires_at"`
	ProviderTradeNo  string    `json:"provider_trade_no"`
	EventID          string    `json:"event_id"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type MerchantChannel struct {
	PaymentNo        string `json:"payment_no"`
	Name             string `json:"name"`
	Currency         string `json:"currency"`
	CurrencyDecimals *int   `json:"currency_decimals"`
	LoginRequired    bool   `json:"login_required"`
	TestMode         bool   `json:"test_mode"`
}

// APIError retains the stable TDCloud error code without exposing response bodies
// or merchant credentials to plugin logs.
type APIError struct {
	StatusCode int
	Code       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("TDCloud API returned HTTP %d (%s)", e.StatusCode, e.Code)
}

func New(id, baseURL, clientID, secret string, currencies map[string]int, httpClient *http.Client) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" ||
		id == "" || clientID == "" || secret == "" {
		return nil, errors.New("invalid TDCloud channel configuration")
	}
	if len(currencies) > 0 {
		if err := validateCurrencies(currencies); err != nil {
			return nil, err
		}
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	disabled := false
	return &Client{id: id, baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID, secret: secret, currencies: cloneCurrencies(currencies), notifyEnabled: &disabled, http: httpClient, auth: &merchantToken{}}, nil
}

func (c *Client) ID() string { return c.id }

func (c *Client) Probe(ctx context.Context) error {
	_, err := c.cashierDirectorySize(ctx)
	return err
}

func (c *Client) cashierDirectorySize(ctx context.Context) (int, error) {
	var directory struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := c.request(ctx, http.MethodGet, "/openapi/v1/merchant-payment-channels", nil, http.StatusOK, &directory); err != nil {
		return 0, err
	}
	return len(directory.Items), nil
}

// Describe checks merchant access without choosing or projecting cashier methods.
// Invoice units require authoritative metadata, never merchant-entered overrides.
func (c *Client) Describe(ctx context.Context) (payment.ChannelInfo, error) {
	if len(c.currencies) == 0 {
		return payment.ChannelInfo{}, ErrCurrencyMetadataUnavailable
	}
	count, err := c.cashierDirectorySize(ctx)
	if err != nil {
		return payment.ChannelInfo{}, err
	}
	if count == 0 {
		return payment.ChannelInfo{}, errors.New("TDCloud cashier has no available payment methods")
	}
	codes := make([]string, 0, len(c.currencies))
	for code := range c.currencies {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	info := payment.ChannelInfo{Currencies: codes}
	if len(codes) == 1 {
		info.Currency = codes[0]
	}
	return info, nil
}

func cloneCurrencies(source map[string]int) map[string]int {
	copy := make(map[string]int, len(source))
	for code, decimals := range source {
		copy[code] = decimals
	}
	return copy
}

var _ payment.Channel = (*Client)(nil)

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.auth.mu.Lock()
	defer c.auth.mu.Unlock()
	if c.auth.token != "" && time.Until(c.auth.end) > 30*time.Second {
		return c.auth.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.clientID}, "client_secret": {c.secret},
		"scope": {"merchant_payments.create merchant_payments.read"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TDCloud OAuth returned HTTP %d", response.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := decode(response.Body, &token); err != nil || token.AccessToken == "" || token.ExpiresIn <= 0 {
		return "", errors.New("invalid TDCloud OAuth response")
	}
	c.auth.token, c.auth.end = token.AccessToken, time.Now().Add(time.Duration(token.ExpiresIn)*time.Second)
	return c.auth.token, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, success int, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != success {
		var remote struct {
			Code string `json:"error"`
		}
		_ = decode(response.Body, &remote)
		return &APIError{StatusCode: response.StatusCode, Code: remote.Code}
	}
	return decode(response.Body, out)
}

func decode(reader io.Reader, out any) error {
	return json.NewDecoder(io.LimitReader(reader, 64<<10)).Decode(out)
}

func (c *Client) CreateCheckout(ctx context.Context, request payment.CheckoutRequest) (payment.Checkout, error) {
	if request.Reference == "" || request.IdempotencyKey == "" || request.AmountMinor <= 0 || request.AmountMinor > 9007199254740991 || len(request.Currency) != 3 {
		return payment.Checkout{}, errors.New("invalid TDCloud checkout request")
	}
	var view merchantPayment
	metadata, err := c.resolveAmount(ctx, request.Currency)
	if err != nil {
		return payment.Checkout{}, err
	}
	if c.legacyAmount && metadata.Decimals != hostDecimals {
		return payment.Checkout{}, payment.ErrAmountPrecision
	}
	providerAmount, err := toProviderMinor(request.AmountMinor, metadata.Decimals)
	if err != nil {
		return payment.Checkout{}, err
	}
	body := map[string]any{
		"external_reference": request.Reference, "idempotency_key": request.IdempotencyKey,
		"description":  request.Description,
		"amount_minor": providerAmount, "currency": request.Currency,
	}
	if c.legacyPaymentNo != "" {
		body["payment_no"] = c.legacyPaymentNo
	}
	if request.ReturnURL != "" {
		body["return_url"] = request.ReturnURL
	}
	if c.notifyEnabled != nil {
		body["notify_enabled"] = *c.notifyEnabled
	}
	if c.notifyURL != "" && c.notifyMode != "merchant_default" {
		body["notify_url"], body["notify_secret"] = c.notifyURL, c.notifySecret
	}
	err = c.request(ctx, http.MethodPost, "/openapi/v1/merchant-payments", body, http.StatusCreated, &view)
	if err != nil {
		return payment.Checkout{}, err
	}
	if view.OrderNo == "" || view.ExternalRef != request.Reference || (c.legacyPaymentNo != "" && view.PaymentNo != c.legacyPaymentNo) ||
		view.AmountMinor != providerAmount || view.CurrencyDecimals == nil || *view.CurrencyDecimals != metadata.Decimals ||
		!strings.EqualFold(view.Currency, request.Currency) || view.ReturnURL != request.ReturnURL {
		return payment.Checkout{}, errors.New("TDCloud checkout does not match requested order")
	}
	if view.Status == "expired" {
		return payment.Checkout{}, payment.ErrCheckoutFailed
	}
	if view.Status != "pending" && view.Status != "review" && view.Status != "paid" {
		return payment.Checkout{}, fmt.Errorf("TDCloud checkout cannot be started in state %q", view.Status)
	}
	// review and paid may be returned on an idempotent retry. Preserve the
	// provider order so the service can reconcile it by server-side lookup.
	return payment.Checkout{ProviderOrderID: view.OrderNo, RedirectURL: view.CheckoutURL, ExpiresAt: view.ExpiresAt, Status: view.Status}, nil
}

func (c *Client) Lookup(ctx context.Context, orderNo string) (payment.PaymentResult, error) {
	if orderNo == "" || strings.ContainsAny(orderNo, "/?&#") {
		return payment.PaymentResult{}, errors.New("invalid TDCloud order number")
	}
	var view merchantPayment
	err := c.request(ctx, http.MethodGet, "/openapi/v1/merchant-payments/"+url.PathEscape(orderNo), nil, http.StatusOK, &view)
	if err != nil {
		return payment.PaymentResult{}, err
	}
	if view.OrderNo != orderNo || (c.legacyPaymentNo != "" && view.PaymentNo != c.legacyPaymentNo) || view.ExternalRef == "" {
		return payment.PaymentResult{}, errors.New("TDCloud payment identity mismatch")
	}
	amount, err := c.normalizeAmount(view.AmountMinor, view.Currency, view.CurrencyDecimals)
	if err != nil {
		return payment.PaymentResult{}, err
	}
	var state string
	switch view.Status {
	case "pending", "paid":
		state = view.Status
	case "expired":
		state = "failed"
	case "review":
		state = "unknown"
	case "refunded", "chargeback":
		state = "reversed"
	default:
		return payment.PaymentResult{}, fmt.Errorf("unknown TDCloud payment state %q", view.Status)
	}
	return payment.PaymentResult{ProviderOrderID: view.OrderNo, Reference: view.ExternalRef, ProviderTradeNo: view.ProviderTradeNo,
		EventID: view.EventID, Status: state, AmountMinor: amount,
		Currency: view.Currency, OccurredAt: view.OccurredAt}, nil
}
