"""Select an explicit external ingest checkout, or the public test receiver."""

import os
from pathlib import Path
import sys


INGEST_SOURCE = None
if os.environ.get("RELAY_INTEGRITY_INGEST_SOURCE"):
    INGEST_SOURCE = Path(os.environ["RELAY_INTEGRITY_INGEST_SOURCE"]).resolve(
        strict=True
    )
    if not (INGEST_SOURCE / "ingest_service" / "relay_ingest.py").is_file():
        raise RuntimeError(
            "RELAY_INTEGRITY_INGEST_SOURCE must contain ingest_service/relay_ingest.py"
        )
    sys.path.insert(0, str(INGEST_SOURCE))
    from ingest_service.config import (
        AppConfig,
        S3LandingConfig,
        ScratchConfig,
        StorageConfig,
    )
    from ingest_service.landing_storage import LandingStorage, ReceiptContext
    from ingest_service.receipt_writer import ReceiptWriter, ReceiptWriteError
    from ingest_service.relay_ingest import (
        RelayIngestRuntime,
        build_relay_ingest_router,
    )
else:
    from .reference_ingest import (
        AppConfig,
        S3LandingConfig,
        ScratchConfig,
        StorageConfig,
        LandingStorage,
        ReceiptContext,
        ReceiptWriter,
        ReceiptWriteError,
        RelayIngestRuntime,
        build_relay_ingest_router,
    )
