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

Import the signed offline package in ZBoard, grant its declared UI/slot, config, storage, public-route, order-read, and payment-write capabilities, then configure and enable the plugin. The running ZBoard build must include the commerce contract above and have a valid deployment `SiteURL` for trusted HTTPS returns.

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

TDCloud is one plugin payment option. The plugin never asks the administrator or consumer to choose a TDCloud internal payment-channel ID, and new collection requests omit `payment_no`. The cashier can initially return no selected method, then report its selected method in later order results. Lookup and notifications identify the TDCloud order by its merchant-authorized order number, external reference, invoice amount, currency and precision; a changing internal method ID is not an outer-order conflict.

TDCloud owns invoice currencies and their precision; merchants must not define or copy those rules into this plugin. The configuration form does not contain invoice-unit fields. The intended source is TDCloud's authoritative merchant API, with the result pinned privately before first creation. A cashier method's currency is not the invoice currency catalogue.

Configuration testing checks merchant OAuth and cashier-directory access independently of payment readiness, without creating an order. The configuration page displays the last test result through the existing host configuration bridge while the plugin process remains running. Restarting the process or changing settings clears that result. Valid merchant access does not establish invoice metadata availability. Configuration loads never perform network probes, and diagnostic fields never persist as merchant configuration. Missing invoice metadata has a distinct setup error rather than a misleading transient retry.

**Current limitation:** the inspected TDCloud merchant API exposes no pre-creation invoice currency catalogue. Its method directory does not return invoice precision, and its currency catalogue requires administrator access. Automatic metadata discovery is therefore not connected yet. A new connection can save credentials, but cannot publish a payment option or create an order until the authoritative source is available. Missing metadata never defaults to two decimals. See [the capability requirement](docs/tdcloud-invoice-currency-capability-requirement.md). Do not build a replacement delivery package until this dependency is resolved and the configuration-only path passes integration.

Earlier private test settings may contain `currencies`; they are decoded for compatibility but removed during active configuration normalization and hidden from the configuration editor. Existing attempts continue using their pinned `_amount` snapshot. These legacy fixture values do not verify automatic discovery. The retired `payment_no` is removed from new configuration; original attempts retain their idempotent request parameters.

### Amount units and verification

ZBoard amounts use two decimal places. TDCloud's authoritative invoice currency precision (0–8), verified against TDCloud's order response `currency_decimals`, determines its smallest unit; conversion uses checked integer arithmetic, never floating point or rounding. For example, a host amount of `1050` means 10.50 TDC and becomes `1050000000` at TDCloud's eight-decimal precision. JPY `1000` becomes `10` at zero-decimal precision. Fractions that cannot be represented or amounts exceeding TDCloud's `9007199254740991` smallest-unit limit are rejected before provider creation; the buyer can select another method.

Currency and precision are saved privately before the first provider request. Ambiguous retries retain those units, the return/notification parameters, and the idempotency key, even after restart or a directory/configuration change. Creation responses must match the requested amount and precision. Lookup and signed notifications require explicit matching precision, normalize to host units, and match the saved amount, currency, channel, and order reference before recording a payment fact. A signed notification triggers authenticated lookup; it cannot settle an order by itself. Older attempts without saved precision only accept two-decimal results; other precisions fail closed to avoid reinterpreting previously sent amounts.

### Return and notifications

The host supplies an HTTPS order-page `return_url`; the plugin pins it at first creation and reuses it with the same idempotency key. It checks the returned address in the provider response. Returning still requires provider lookup and the host receipt.

For background notifications, enable notifications in the channel form and enter the signing key issued by TDCloud's merchant notification settings. This is distinct from the OAuth Client Secret. The plugin stores that key privately for verification and does not generate a replacement. TDCloud only displays its generated key once and ordinary configuration reads do not return it, so the merchant copies it during setup. Blank edits preserve an already configured platform key for the same merchant identity; transitioning from legacy per-order overrides requires the actual platform key.

The receiving HTTPS origin defaults to the authenticated admin page origin and can be edited; the registered path `/.well-known/zboard-payments/v1/tdcloud-notify` is read-only. Set this full URL in TDCloud's default notification configuration as well. New form saves use `notify_mode: "merchant_default"`; creation sends `notify_enabled: true` and omits `notify_url` and `notify_secret` to inherit TDCloud's configuration. The local URL is the receiving endpoint reference, not an instruction that changes TDCloud's settings. TDCloud key rotations must also be reflected in the plugin for new attempts. Original attempts retain their saved key and request mode. Legacy snapshots with no mode preserve their original per-order notification override parameters for idempotent retries.

The receiver verifies TDCloud's event ID, timestamp and HMAC headers with the saved channel snapshot, looks up the provider order, then records the same payment fact. No notification body can select an arbitrary host principal.

If no notification endpoint is configured, new checkouts explicitly send `notify_enabled: false`; buyer confirmation and order returns still work. Pending attempts retain their original notification parameters across configuration changes. Notifications with temporary storage/host failures return a retryable response. Refunds or chargebacks observed before settlement enter review; reversal of a settled business order is outside this implementation.

## Loading and retry behavior

Method discovery runs concurrently with a four-second deadline. Complete results are cached for 60 seconds by configuration and currency; partial failures are not cached and do not hide healthy methods. The iframe displays concise loading/empty/failure states and supports local retry. The host owns the payment wait and confirm controls.

Private storage contains attempts, provider-order references, and a bounded-key authority index. It uses host compare-and-swap revisions and retries brief lifecycle contention. Existing attempts restore the saved provider configuration after restart or removal from current configuration.

## Development checks

Keep `zboard` and this repository in sibling directories; `go.mod` currently replaces the public SDK with `../zboard/backend/pkg/pluginapi`.

```sh
go test ./...
go vet ./...
go test -race ./internal/runtime ./payment ./tdcloud
node --test ui/*.test.mjs
python3 scripts/check-host-integration.py
```

The host integration check creates an isolated copy of committed ZBoard HEAD. It imports the actual signed plugin, configures a simulated TDCloud HTTPS service, exercises the public SDK, covers USD (2), TDC (8), and JPY (0), rejects conflicting lookup and signed notification fields, restarts the process, races confirmation against signed notification, and checks real host payment/subscription/credential records. Only the temporary test binary trusts the simulator certificate. It does not modify the ZBoard checkout, retain signing keys, or produce a deliverable offline package.

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

The repository has not published a first code release. Its manifest, runtime and deliverable packages use **0.0.1**. Earlier local packages labelled 0.1.0/0.1.1 were temporary testing artifacts, not releases; replace them with the corrected 0.0.1 package.

ZBoard's lifecycle request mismatch is fixed in commit `abd9687`: its management page no longer sends the retired `version_id` field. Use a compatible running build containing that correction. The administrator configuration page works while the plugin is disabled. The [management requirement](docs/zboard-plugin-management-contract-requirement.md) remains as resolved issue history.
