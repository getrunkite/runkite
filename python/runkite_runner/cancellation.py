"""Keep attempt supersession separate from run-wide cancellation signals."""

import asyncio
from asyncio import get_running_loop

CANCEL_SIGNAL_TTL_S = 60


class RunCancellation(set[asyncio.Event]):
    """Run-wide user cancellation, separate from each attempt's stop Event."""

    def __init__(self, pre_cancelled: set[str]):
        super().__init__()
        self.cancelled = False
        self.pre_cancelled = pre_cancelled
        self.expiry: asyncio.TimerHandle | None = None


PendingCancels = dict[str, RunCancellation]


def _entry(pending_cancels: PendingCancels, pre_cancelled: set[str], run_id: str) -> RunCancellation:
    entry = pending_cancels.get(run_id)
    if entry is None:
        entry = RunCancellation(pre_cancelled)
        pending_cancels[run_id] = entry
    return entry


def _expire_orphan(pending_cancels: PendingCancels, run_id: str, entry: RunCancellation) -> None:
    if entry.expiry is not None:
        entry.expiry.cancel()

    def expire():
        # All registry mutations run without an await on the same event loop.
        # An old callback must not clear a refreshed or re-registered bucket.
        if entry.expiry is not timer:
            return
        entry.expiry = None
        if pending_cancels.get(run_id) is entry and not entry:
            pending_cancels.pop(run_id)
            entry.pre_cancelled.discard(run_id)

    timer = get_running_loop().call_later(CANCEL_SIGNAL_TTL_S, expire)
    entry.expiry = timer


async def register_run(
    pending_cancels: PendingCancels,
    pre_cancelled: set[str],
    lock: asyncio.Lock,
    run_id: str,
) -> asyncio.Event:
    """Allocate an attempt-local Event and observe any recent run cancel."""
    event = asyncio.Event()
    async with lock:
        entry = _entry(pending_cancels, pre_cancelled, run_id)
        if entry.expiry is not None:
            entry.expiry.cancel()
            entry.expiry = None
        if run_id in pre_cancelled:
            entry.cancelled = True
        if entry.cancelled:
            event.set()
        entry.add(event)
    return event


async def unregister_run(
    pending_cancels: PendingCancels,
    lock: asyncio.Lock,
    run_id: str,
    event: asyncio.Event,
) -> None:
    """Remove only this attempt; a replacement can still be executing."""
    async with lock:
        entry = pending_cancels.get(run_id)
        if entry is None or event not in entry:
            return
        entry.remove(event)
        if not entry:
            if entry.cancelled:
                _expire_orphan(pending_cancels, run_id, entry)
            else:
                pending_cancels.pop(run_id)


async def record_cancel_signal(
    pending_cancels: PendingCancels,
    pre_cancelled: set[str],
    lock: asyncio.Lock,
    run_id: str,
) -> bool:
    """Cancel every attempt and retain the signal for racing registrations.

    Return whether the signal was newly remembered. Orphan duplicates refresh
    their bounded expiry; active canceled buckets stay latched until cleanup.
    """
    async with lock:
        new_signal = run_id not in pre_cancelled
        pre_cancelled.add(run_id)
        entry = _entry(pending_cancels, pre_cancelled, run_id)
        entry.cancelled = True
        for event in entry:
            event.set()
        if not entry:
            _expire_orphan(pending_cancels, run_id, entry)
        return new_signal
