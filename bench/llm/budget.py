"""Soft USD spend guard for live LLM matrix runs."""

from __future__ import annotations

import json
import os
import time
from dataclasses import asdict, dataclass, field
from pathlib import Path


@dataclass
class Budget:
    limit_usd: float
    cost_per_1k_in: float
    cost_per_1k_out: float
    spent_usd: float = 0.0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    calls: int = 0
    stopped: bool = False
    stop_reason: str = ""
    events: list[dict] = field(default_factory=list)

    @classmethod
    def from_env(cls) -> "Budget":
        return cls(
            limit_usd=float(os.environ.get("LLM_BUDGET_USD", "150")),
            cost_per_1k_in=float(os.environ.get("LLM_COST_PER_1K_INPUT_USD", "0.0001")),
            cost_per_1k_out=float(os.environ.get("LLM_COST_PER_1K_OUTPUT_USD", "0.0004")),
        )

    def charge(self, prompt_tokens: int, completion_tokens: int, label: str) -> None:
        cost = (prompt_tokens / 1000.0) * self.cost_per_1k_in + (completion_tokens / 1000.0) * self.cost_per_1k_out
        # Floor: every live cell charges at least a tiny amount so a
        # missing usage metadata path still trends toward the cap.
        if prompt_tokens == 0 and completion_tokens == 0:
            cost = max(cost, 0.0005)
        self.spent_usd += cost
        self.prompt_tokens += prompt_tokens
        self.completion_tokens += completion_tokens
        self.calls += 1
        self.events.append(
            {
                "ts": time.time(),
                "label": label,
                "prompt_tokens": prompt_tokens,
                "completion_tokens": completion_tokens,
                "cost_usd": cost,
                "spent_usd": self.spent_usd,
            }
        )
        if self.spent_usd >= self.limit_usd:
            self.stopped = True
            self.stop_reason = f"estimated spend ${self.spent_usd:.4f} >= limit ${self.limit_usd:.2f}"

    def allow(self) -> bool:
        return not self.stopped

    def dump(self, path: Path) -> None:
        path.write_text(json.dumps(asdict(self), indent=2) + "\n")
