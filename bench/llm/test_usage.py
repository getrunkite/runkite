"""Offline self-check for usage extraction (no API key)."""

from __future__ import annotations

from usage import sum_usage_from_events


def main() -> None:
    events = [
        {
            "event": "values",
            "data": {
                "messages": [
                    {
                        "role": "ai",
                        "content": "hi",
                        "usage_metadata": {"input_tokens": 12, "output_tokens": 3},
                    }
                ]
            },
        }
    ]
    p, c, measured = sum_usage_from_events(events)
    assert measured and p == 12 and c == 3, (p, c, measured)
    p2, c2, m2 = sum_usage_from_events([{"event": "end", "data": {"status": "success"}}])
    assert not m2 and p2 == 0 and c2 == 0
    print("[PASS] usage extraction")


if __name__ == "__main__":
    main()
