# Changelog

## v0.0.1

First formal release of `zboard.payments`.

- Configurable provider-neutral payment channels and native checkout/order slots.
- TDCloud hosted cashier integration with decimal amounts, merchant OAuth, trusted returns and signed notification hints verified by authenticated lookup.
- Idempotent payment facts and renewed provider attempts on the original ZBoard business order.
- Frozen historical credentials, restart recovery, scheduled reconciliation and administrator review of late or additional receipts.
- Administrator configuration while disabled, retained secrets and asynchronous channel loading.
- Signed Linux/macOS amd64/arm64 offline packages and reproducible GitHub release metadata.

Compatibility: the public SDK and CI host are pinned to ZBoard commit `505d0b728ea6d3fd78730326718b2446bdef9a7b`. Hosts must support all eight manifest capabilities, including commerce and task scheduling. Data schema 2 reads schema-1 records and preserves channel configuration. Older records whose historical credentials were already discarded require provider-side manual reconciliation. Additional receipts are retained for review; refunds are not automatic.

Build checks and isolated provider simulations do not establish acceptance on every target platform or deploy this release to a running host.
