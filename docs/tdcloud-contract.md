# TDCloud adapter contract

Aligned with TDCloud merchant collection implementations `8b5b80e` (decimal amounts) and `9bbc8a1` (optional input normalization), and the published merchant collection contract. ZBoard owns business orders and fulfillment; this adapter owns the provider request, private retry snapshot and verified payment result.

## Creation

Use merchant OAuth client credentials with `merchant_payments.create` and `merchant_payments.read`. Submit `POST /openapi/v1/merchant-payments`:

```json
{
  "external_reference": "zboard-order-reference",
  "idempotency_key": "stable-plugin-attempt-key",
  "amount": "15.00",
  "currency": "CNY",
  "description": "Plan purchase",
  "return_url": "https://board.example/account/orders?payment_order=42",
  "notify_enabled": false
}
```

- `amount` is a string, never a JSON number or exponent. ZBoard cents `1500` become `"15.00"` without consulting TDCloud precision. Never send both representations.
- New requests omit `amount_minor` and `payment_no`. TDCloud interprets invoice units, validates currency availability, precision and bounds, and supplies the cashier's payment choices.
- The method directory checks merchant access and cashier availability only. Its internal method IDs, currencies and login requirements are not exposed as plugin payment options.
- Validate the returned order number, external reference, decimal amount, currency and exact trusted `return_url`. Capture the provider order and cashier URL. A response or browser return does not prove fulfillment.
- New snapshots store `_amount.version: 2` and the invoice currency, without a precision field. The attempt retains the original host amount, idempotency key, return URL and notification choice for ambiguous retries.

## Optional parameters

`description` and `return_url` may be omitted, empty or null at the platform boundary. The adapter trims optional presentation text before transmission, omits empty descriptions/returns and compares the response with the normalized return URL. It never makes description mandatory or builds Stripe parameters. Host-generated return URLs are already validated and saved on the attempt. It rejects surrounding whitespace in business references and idempotency keys instead of silently changing their identity.

`expires_in` is optional private channel configuration. Omission/null delegates to TDCloud's 1800-second default; explicit integer values must be 60–7200. The advanced configuration can set it, and the private channel snapshot retains it across retries, process restarts or active configuration changes. A value of zero is invalid. Existing snapshots lacking the field retain the default request.

New normalized plugin settings explicitly disable notifications when no receiver is configured, even if the platform has defaults enabled. Platform raw requests with omitted/null `notify_enabled` inherit applicable defaults; explicit `false` does not. Explicit `true` requires a configured platform default or URL/key pair. The plugin stores a verification key before enabling notifications. It never includes URL/key overrides when sending `false` or using `merchant_default`. Optional fields are not required merely to create a payment.

## Lookup and notifications

Authenticated `GET /openapi/v1/merchant-payments/:order_no` returns the authoritative decimal `amount`. Compare its value exactly to the saved host amount and validate currency, provider order and external reference. Accept equivalent trailing zeros; reject missing/invalid text, values not exactly representable in host cents, overflow and mismatches. New attempts never fall back to derived `amount_minor` or `currency_decimals`.

Notifications use the same decimal field in `data`, authenticated by TDCloud's event ID, timestamp and raw-body HMAC headers. Match the saved attempt, then perform authenticated lookup before recording the payment fact. Duplicate notification, return and confirmation paths use the same idempotent fact and host fulfillment transaction.

Merchant-default notifications send `notify_enabled: true` without a per-order URL or secret override. The plugin privately retains TDCloud's platform-issued signing key. Disabled notifications explicitly send `false`; confirmation and browser return still trigger lookup.

## Rejections and historical attempts

Saved checkout deadlines are enforced on reopen. An overdue attempt is looked up before its redirect can be reused. Only authenticated checkout expiry permits a new attempt/key on the same host order; a late `paid` result settles the original order. Pending/review results keep the old identity and return `checkout_expiry_pending` to explain why a stale checkout cannot reopen.

HTTP 400 `invalid_amount` / `invalid_amount_precision` and HTTP 422 `currency_unavailable` are explicit create rejections. They release the attempt so another configured option can be chosen. Network errors, HTTP 5xx and idempotency conflicts retain the original attempt for reconciliation or an identical retry. HTTP 202 `provider_result_pending`, and cashier-specific 403/422 failures, cannot settle an order or release a plugin attempt. A provider `review` result stays unconfirmed; verified `expired` maps to an ended checkout window (`failed` locally), which remains eligible for later paid reconciliation. The plugin never calls the underlying Stripe/session API.

Previously issued version-1 or unversioned snapshots retain their exact integer request and original internal method binding. `tdcloud/legacy_amount.go` contains only that compatibility path. They retain original precision/result checks and historical notification parameters. Active configuration strips retired manual `currencies` and `payment_no`; new payments always use version 2.

## Development acceptance

- Configuration containing only the merchant API origin and OAuth credentials discovers one TDCloud cashier without currency metadata.
- CNY/USD/TDC/JPY invoices submit decimal text without a provider precision lookup or chosen internal method.
- Creation, lookup and signed notifications reject changed amount/currency/reference, missing decimal text and nonzero sub-cent fractions.
- Timeout and restart retries preserve amount, key, URLs, expiry and notification mode, including historical integer snapshots.
- Optional omission/null/empty text, default 1800-second expiry, explicit bounds, notification dependencies and normalized idempotency are exercised against a rejecting simulator, not unconditional success responses.
- Against an isolated committed ZBoard checkout, real plugin processes use the public SDK and demonstrate one payment event and one fulfillment when confirmation races a signed notification.

These are local development checks against an HTTPS simulator. Deployment and a real TDCloud transaction are separate evidence.

## Payment after checkout expiry

The published TDCloud contract allows an authenticated collection to advance from `expired` to `paid`. Lookup `status_version` orders snapshots of the same provider order; it is not a cross-order deduplication key. A signed notification is a lookup hint and must not overwrite a newer verified state. Stable paid event IDs are replayed to the host, while different provider-order receipts are preserved independently.

Historical provider orders retain frozen credentials, notification keys, amount/reference and verified facts. Renewal does not delete their callback index. An idempotent create returning an expired collection retains that collection ID before allowing another attempt. Extra receipts and late payments on closed business orders are persisted for explicit manual/refund review, even when the host refuses an additional trade number. Host order fulfillment remains atomic and at most once. The periodic registered task provides interval-based recovery when notifications are unavailable.
