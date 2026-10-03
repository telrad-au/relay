"""Timing regressions for the C-STORE end-to-end proof."""

import threading
from concurrent.futures import ThreadPoolExecutor
from io import BytesIO
from types import SimpleNamespace

from pynetdicom import evt

from . import relay_image_integrity as runner


def test_store_response_is_observed_even_when_association_release_stalls(monkeypatch):
    """Waiting for send() alone must not conceal an early C-STORE response."""
    release_entered = threading.Event()
    release_allowed = threading.Event()
    response_received = threading.Event()
    payload = b"synthetic dataset bytes"

    class Association:
        is_established = True
        is_released = False

        def send_c_store(self, dataset):
            for event, handler in self.handlers:
                if event == evt.EVT_DIMSE_SENT:
                    handler(
                        SimpleNamespace(
                            message=SimpleNamespace(
                                command_set=SimpleNamespace(CommandField=0x0001),
                                data_set=BytesIO(payload),
                            )
                        )
                    )
            return SimpleNamespace(Status=0x0000)

        def release(self):
            release_entered.set()
            assert release_allowed.wait(5)
            self.is_established = False
            self.is_released = True

    class ApplicationEntity:
        def __init__(self, **kwargs):
            pass

        def add_requested_context(self, *args):
            pass

        def associate(self, *args, evt_handlers, **kwargs):
            association = Association()
            association.handlers = evt_handlers
            return association

    monkeypatch.setattr(runner, "AE", ApplicationEntity)
    case = SimpleNamespace(
        name="early-response",
        dataset=SimpleNamespace(
            SOPClassUID="1.2.840.10008.5.1.4.1.1.7",
            file_meta=SimpleNamespace(TransferSyntaxUID="1.2.840.10008.1.2.1"),
        ),
    )
    with ThreadPoolExecutor(max_workers=1) as executor:
        future = executor.submit(runner.send, case, 12345, response_received)
        try:
            assert release_entered.wait(5)
            assert not future.done()
            assert response_received.is_set()
        finally:
            release_allowed.set()
        status, sent, _ = future.result(timeout=5)
    assert status == 0x0000
    assert sent == payload
