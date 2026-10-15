# ZBoard Payment Channel Plugin

[简体中文](README.zh-CN.md)

ZBoard owns products, business orders, subscriptions, and entitlements. This plugin owns configurable payment channels, private payment attempts, provider checkout creation, server-side verification, and notification authentication. It submits a verified payment fact through the public native SDK; ZBoard performs the existing transactional fulfillment. A browser redirect is not payment evidence.

## Host integration

This plugin targets the commerce contract introduced by ZBoard commit `3e0ca9b` (or a later compatible build). A version number alone does not establish support. The host must admit the declared grants and slots; older hosts cannot run this integration.

- `account.checkout.payment`: supplies methods on the existing order confirmation page.
- `account.orders.payment`: supplies methods on the existing order/return page.
- The account entry requests `checkout.context`, discovers configured methods asynchronously, and publishes `checkout.options`. Each option declares `payments.prepare` and `payments.reconcile`.
- The host owns method selection, business-order creation, synchronous payment-window reservation, navigation, confirmation controls, and the final host-order receipt. The sandboxed plugin iframe does not open a window or create a business order.
- Native actions consume `PageActionRequest.Checkout()` and the host-provided order-bound principal. They accept only an option ID, never a browser-selected account, amount, currency, or return URL.
- Startup binds `HostClient.ListOrders`, `HostClient.RecordPayment`, and the trusted checkout return URL to the coordinator and plugin-owned `HostStorage`. The saved private authority supports notification reconciliation after the browser closes.
- Buyer confirmation, order return, and signed notifications share the same attempt and provider event. A local attempt becomes paid only after the host acknowledges payment recording. Restart and retry preserve the channel snapshot and payment identity.
- An owned-order lookup gates discovery of historical attempts. Removed or disabled methods may appear as disabled choices so the order can still reconcile with its original provider snapshot; they cannot create a new payment intent.
- There is no independent checkout menu. Consumers do not create or paste integration credentials.

See the sibling host's `backend/pkg/pluginapi/v1/commerce.md` for the public SDK contract. The [host requirement baseline](docs/zboard-plugin-payment-capability-requirement.md) is retained as design history; those contracts are now published.

## Channel configuration

Import the signed offline package in ZBoard, grant its declared UI/slot, config, storage, public-route, background-task, order-read, and payment-write capabilities, then configure and enable the plugin. The running ZBoard build must include the commerce contract above and have a valid deployment `SiteURL` for trusted HTTPS returns.

The plugin now declares an administrator configuration page (`settings`) that is available while disabled. It provides a shared “Add payment channel” action and a provider-type selector, with fields owned by registered provider adapters. TDCloud is the currently implemented adapter. Retained secrets, removal confirmation, and saved-configuration tests use the existing host configuration bridge. Advanced JSON configuration remains available.

The UI adapter registry is `ui/channel-config-providers.mjs`; TDCloud fields are isolated in `ui/tdcloud-config.mjs`. The host currently prevents configuration pages from registering a business menu and only authorizes configuration sessions to use `config.*`. See the [generic configuration navigation requirement](docs/zboard-plugin-configuration-navigation-requirement.md). Plugin lifecycle management and routine business settings should have distinct navigation contexts.

The host configuration editor accepts:

```json
{
  "channels": [
    {
      "id": "tdcloud-main",
      "name": "TDCloud",
      "type": "tdcloud",
      "enabled": true,
      "settings": {
        "api_url": "https://tdcloud.example.com",
        "client_id": "merchant-oauth-client-id",
        "client_secret": "merchant-oauth-client-secret"
      }
    }
  ]
}
```

Use the actual TDCloud API origin and merchant credentials. The merchant client needs `merchant_payments.create` and `merchant_payments.read`. New orders omit `payment_no`: TDCloud owns its cashier options and method selection. Run the host's configuration test to check OAuth access and channel visibility without creating an order. A successful probe alone is not an order settlement.

Configuration reads omit `client_secret` and `notify_secret`. Saving those fields omitted preserves the existing credentials for the same merchant/endpoint; supply new values when rotating them. Credentials and native order authorities remain on the backend, never in checkout options.

### Hosted cashier boundary

TDCloud is one plugin payment option. The plugin never asks the administrator or consumer to choose a TDCloud internal payment-channel ID, and new collection requests omit `payment_no`. The cashier can initially return no selected method, then report its selected method in later order results. Lookup and notifications identify the TDCloud order by its merchant-authorized order number, external reference, invoice amount and currency; a changing internal method ID is not an outer-order conflict.

The formal TDCloud merchant contract accepts `amount` as an unsigned decimal string. New checkouts send `"amount": "15.00"` and the host's invoice currency, omitting both `amount_minor` and `payment_no`. TDCloud validates the currency, precision and limits and lets its hosted cashier choose the internal method. The plugin never queries or configures provider currency precision. The method directory checks merchant access and cashier availability only; internal method currencies and login requirements do not filter the outer business order. See [the adapter contract](docs/tdcloud-contract.md).

Configuration testing checks merchant OAuth and cashier-directory availability without creating an order. The page displays the running process's last test result through the existing configuration bridge. Restarting the process or changing settings clears that result. Configuration loads perform no network probes and diagnostic fields never persist as merchant settings. A successful connection check is not evidence of a completed payment.

### Decimal amounts and verification

ZBoard supplies amounts in its known two-decimal units. The plugin formats `1500` as `"15.00"` using integer arithmetic; it does not convert this amount into TDCloud minor units. Responses and signed notifications use the authoritative decimal `amount`. Equivalent trailing zeros are accepted (`"10.50"` and `"10.50000000"`); missing text, invalid syntax, sub-cent fractions, overflow and mismatched amounts or currencies are rejected without rounding. TDCloud's derived `amount_minor` and `currency_decimals` do not drive the current protocol. A signature still triggers authenticated lookup before recording a payment fact.

New private `_amount` snapshots pin wire version 2 and invoice currency. The attempt already pins the host amount, idempotency key and return/notification parameters. Retries after timeout, process restart, configuration changes or channel removal keep those values. Provider `invalid_amount`, `invalid_amount_precision` and `currency_unavailable` rejections release the attempt so another option can be selected; ambiguous failures and idempotency conflicts keep the original attempt.

Existing version-1 or unversioned snapshots retain the exact historical integer request, original method binding and result-unit checks. The former conversion code is isolated in `tdcloud/legacy_amount.go` and is never selected for a new payment. Retired `currencies` and `payment_no` settings are stripped from active configuration and hidden from the editor. The former invoice-metadata requirement is [superseded by the decimal contract](docs/tdcloud-invoice-currency-capability-requirement.md).

### Optional checkout inputs

Description is optional and omitted when blank; it is never used to work around provider-specific Stripe behavior. Optional description and return URL text is trimmed before transmission and response comparison, while business reference, amount, currency and idempotency identity remain strict. `expires_in` can be set in advanced channel settings: omit/null for TDCloud's 1800-second default, or use an integer from 60 to 7200. The attempt's private snapshot retains the original setting across restart and configuration edits. Existing snapshots with no expiry field remain compatible. HTTP 202 or a cashier-specific 403/422 does not prove payment or justify a replacement order; continue lookup of the original TDCloud order.

### Return and notifications

The host supplies an HTTPS order-page `return_url`; the plugin pins it at first creation and reuses it with the same idempotency key. It checks the returned address in the provider response. Returning still requires provider lookup and the host receipt.

For background notifications, enable notifications in the channel form and enter the signing key issued by TDCloud's merchant notification settings. This is distinct from the OAuth Client Secret. The plugin stores that key privately for verification and does not generate a replacement. TDCloud only displays its generated key once and ordinary configuration reads do not return it, so the merchant copies it during setup. Blank edits preserve an already configured platform key for the same merchant identity; transitioning from legacy per-order overrides requires the actual platform key.

The receiving HTTPS origin defaults to the authenticated admin page origin and can be edited; the registered path `/.well-known/zboard-payments/v1/tdcloud-notify` is read-only. Set this full URL in TDCloud's default notification configuration as well. New form saves use `notify_mode: "merchant_default"`; creation sends `notify_enabled: true` and omits `notify_url` and `notify_secret` to inherit TDCloud's configuration. The local URL is the receiving endpoint reference, not an instruction that changes TDCloud's settings. TDCloud key rotations must also be reflected in the plugin for new attempts. Original attempts retain their saved key and request mode. Legacy snapshots with no mode preserve their original per-order notification override parameters for idempotent retries.

The receiver verifies TDCloud's event ID, timestamp and HMAC headers with the saved channel snapshot, looks up the provider order, then records the same payment fact. No notification body can select an arbitrary host principal.

If no notification endpoint is configured, new checkouts explicitly send `notify_enabled: false`; buyer confirmation and order returns still work. Pending attempts retain their original notification parameters across configuration changes. Notifications with temporary storage/host failures return a retryable response. Refunds or chargebacks observed before settlement enter review; reversal of a settled business order is outside this implementation.

## Loading and retry behavior

Before reopening a saved checkout past its provider-issued deadline, the plugin queries its authenticated payment status. A verified checkout expiry permits renewing the payment attempt and idempotency key on the same ZBoard order. A late verified payment completes that order instead. Unresolved results or lookup failures retain the original attempt and stop reopening the stale URL; the UI explains that the original payment is still being checked. Local time alone never releases an attempt.

Expired order slots offer a one-shot original-payment inspection. The host verifies order ownership before the plugin reads its saved attempt and performs authenticated provider lookup. This action never creates an order, changes an attempt or records payment; it distinguishes the original provider state from the normalized payment state and classifies authorization/lookup failures without exposing credentials or cashier tokens. Business rejections use the public SDK `NewActionError` contract documented in ZBoard's `pluginapi/v1/actions.md`; older hosts safely degrade to generic errors.

Method discovery runs concurrently with a four-second deadline. Complete results are cached for 60 seconds by configuration and currency; partial failures are not cached and do not hide healthy methods. The iframe displays concise loading/empty/failure states and supports local retry. The host owns the payment wait and confirm controls.

Private storage contains attempts, provider-order references, and a bounded-key authority index. It uses host compare-and-swap revisions and retries brief lifecycle contention. Existing attempts restore the saved provider configuration after restart or removal from current configuration.

## Development checks

The public SDK is pinned to ZBoard commit `505d0b728ea6d3fd78730326718b2446bdef9a7b` in `go.mod` and `go.sum`; unit tests need no sibling checkout. Packaging and the isolated host integration check use a separate ZBoard checkout. Set `ZBOARD_DIR` to its absolute path, or keep it in `../zboard`. CI checks out that exact host revision.

```sh
go test ./...
go vet ./...
go test -race ./internal/runtime ./payment ./tdcloud
node --test ui/*.test.mjs
python3 scripts/check-host-integration.py
```

The host integration check creates an isolated copy of committed ZBoard HEAD. It imports the actual signed plugin, configures a simulated TDCloud HTTPS service, exercises the public SDK, submits decimal amounts for USD, TDC and JPY with no configured provider precision, rejects conflicting lookup and signed notification fields, restarts the process, races confirmation against signed notification, and checks real host payment/subscription/credential records. Only the temporary test binary trusts the simulator certificate. It does not modify the ZBoard checkout, retain signing keys, or produce a deliverable offline package.

## Offline package

After the checks pass, build a package for Linux and macOS on amd64/arm64:

```sh
scripts/build-package.sh "$PWD/dist/zboard-payments-0.0.1.zbplugin"
```

The official ZBoard packager validates the signed package and retains a stable local publisher key in the OS user configuration directory, outside this repository. For server import, prefer a platform package to reduce transfer size: append `linux-amd64` (or the server's actual platform) to build only that target. A Linux amd64 package is about 5.4 MiB; the four-platform package is about 21 MiB. To use an existing publisher key:

```sh
scripts/build-package.sh /absolute/output.zbplugin /absolute/private-key-file publisher-id all
```

Keep the signing key for updates: replacing the publisher identity invalidates saved host order authorities. The package contains runtime binaries, UI assets, a manifest, and a public signature; no merchant credentials or private signing key are included.

## Version and host compatibility

The first formal release is **v0.0.1**. Its manifest, runtime and deliverable packages use **0.0.1**. Earlier local packages labelled 0.1.0/0.1.1 were temporary testing artifacts, not releases; replace them with the corrected 0.0.1 package.

ZBoard's lifecycle request mismatch is fixed in commit `abd9687`: its management page no longer sends the retired `version_id` field. Use a compatible running build containing that correction. The administrator configuration page works while the plugin is disabled. The [management requirement](docs/zboard-plugin-management-contract-requirement.md) remains as resolved issue history.

## Late payments and financial review

TDCloud may confirm `expired → paid` after a checkout deadline. Expiry ends the payment window; it does not permanently exclude a verified payment. Both signed notification hints and authenticated lookups reconcile the exact provider order, reference, currency and amount. Versioned lookups cannot roll back a newer status, and paid receipts are not erased by late expiry notifications.

Every renewed attempt retains its complete private channel snapshot and provider-order index. Authenticated paid facts are persisted before the host call; an interrupted host response can be replayed with the same event identity after restart. Different paid attempts remain separate receipts. The first accepted fact fulfills the business order through ZBoard; additional payments or payments for closed orders enter the administrator **Payment review** page for refund/manual resolution. The plugin never automatically refunds or reopens a business order. ZBoard currently rejects a second different trade number on an already settled order; the additional receipt remains in plugin-owned storage and is visible for review rather than being dropped.

The declared `reconcile-payments` task uses ZBoard's existing task scheduler every five minutes, scans at most 20 orders per run and checks at most 10 due provider attempts per order. Pending attempts are spaced by five minutes; expired attempts remain in daily historical reconciliation. Notifications and explicit confirmations bypass that interval. There is no private browser/background polling loop. The host must admit `zboard.task.v1` to run the fallback task.

The package version remains `0.0.1`. Private data schema 2 reads existing schema-1 attempts and keeps complete new history; its empty host migration does not rewrite channel settings. The host prevents downgrading to an older reader after schema preparation. Pre-fix records that contain only historical IDs without frozen credentials are shown as **historical credentials missing**; the plugin cannot reconstruct discarded authority from current settings. Operators must reconcile those records through the owning provider.

## GitHub Actions and releases

`Checks` runs on pull requests and pushes to `master`: Go race tests, vet, browser-module tests, release validation, dependency-lock verification, the isolated host integration check and four signed development packages. Development artifacts use the disposable `payments-ci-dev` identity, expire after 14 days, and are for isolated testing only.

`Release` runs for a `v*` tag, or manually with the matching tag. It reruns checks, requires the source commit to belong to `master`, and signs four platform packages in the `release` environment. Configure these environment values before publishing:

- Secret `PLUGIN_SIGNING_KEY`: the existing base64 Ed25519 private key, supplied securely; never commit it.
- Variable `PLUGIN_PUBLISHER_ID`: the existing publisher identity.
- Variable `PLUGIN_PUBLIC_KEY`: the corresponding base64 public key.

The workflow validates version/tag agreement and key identity, then publishes a draft only after all packages verify. Published assets are never overwritten. Releases include Linux/macOS amd64/arm64 packages, `SHA256SUMS`, `publisher.pub` and `release-metadata.json` recording the full source commit, compatibility requirements, publisher and artifact hashes. Verify publisher trust independently before import. No merchant configuration, host order authority or private signing key is packaged. Publishing a release does not update a running ZBoard installation.
