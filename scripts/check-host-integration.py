#!/usr/bin/env python3
"""Run the real payment plugin against ZBoard transactions in an isolated checkout.

No files in the ZBoard repository are changed. The simulated HTTPS provider's
certificate is trusted only in the temporary test binary. No deliverable offline
package or signing key is retained.
"""
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

plugin_root = Path(__file__).resolve().parent.parent
board_root = Path(os.environ.get("ZBOARD_DIR", str(plugin_root.parent / "zboard"))).resolve()
revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=board_root, text=True).strip()
print(f"Checking plugin against committed ZBoard {revision}", flush=True)
archive = subprocess.check_output(["git", "archive", revision], cwd=board_root)
with tempfile.TemporaryDirectory(prefix="zbpay-host-check-", dir="/tmp") as directory:
    checkout = Path(directory)
    with tarfile.open(fileobj=io.BytesIO(archive)) as source:
        source.extractall(checkout, filter="data")
    test = checkout / "backend/internal/handler/actual_payment_plugin_test.go"
    test.write_bytes((plugin_root / "integration/zboard_acceptance.gotest").read_bytes())
    environment = dict(os.environ, ZBOARD_PAYMENT_PLUGIN_SOURCE=str(plugin_root), GOMAXPROCS="2")
    subprocess.run(["go", "test", "-p", "1", "./internal/handler", "-run",
                    "^TestActualPaymentsPluginClosesHostOrder$", "-count=1", "-v"],
                   cwd=checkout / "backend", env=environment, check=True)
