"""Soft USD spend guard for live LLM matrix runs.

Prefer measured token usage from the event stream when present. When
missing, charge a *conservative* (high) estimate so the cap trips early
on long agentic loops rather than under-counting.
"""

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
    # Conservative fallbacks when the stream carries no usage_metadata.
    estimate_happy_prompt: int
    estimate_happy_completion: int
    estimate_other_prompt: int
    estimate_other_completion: int
    spent_usd: float = 0.0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    calls: int = 0
    measured_calls: int = 0
    estimated_calls: int = 0
    stopped: bool = False
    stop_reason: str = ""
    events: list[dict] = field(default_factory=list)

    @classmethod
    def from_env(cls) -> "Budget":
        return cls(
            limit_usd=float(os.environ.get("LLM_BUDGET_USD", "150")),
            cost_per_1k_in=float(os.environ.get("LLM_COST_PER_1K_INPUT_USD", "0.0001")),
            cost_per_1k_out=float(os.environ.get("LLM_COST_PER_1K_OUTPUT_USD", "0.0004")),
            # High enough that a tool-loop ReAct turn isn't under-counted
            # when usage_metadata never reaches the SSE stream.
            estimate_happy_prompt=int(os.environ.get("LLM_ESTIMATE_HAPPY_PROMPT", "2500")),
            estimate_happy_completion=int(os.environ.get("LLM_ESTIMATE_HAPPY_COMPLETION", "800")),
            estimate_other_prompt=int(os.environ.get("LLM_ESTIMATE_OTHER_PROMPT", "200")),
            estimate_other_completion=int(os.environ.get("LLM_ESTIMATE_OTHER_COMPLETION", "50")),
        )

    def charge(
        self,
        prompt_tokens: int,
        completion_tokens: int,
        label: str,
        *,
        measured: bool = False,
    ) -> None:
        cost = (prompt_tokens / 1000.0) * self.cost_per_1k_in + (completion_tokens / 1000.0) * self.cost_per_1k_out
        if prompt_tokens == 0 and completion_tokens == 0:
            cost = max(cost, 0.0005)
        self.spent_usd += cost
        self.prompt_tokens += prompt_tokens
        self.completion_tokens += completion_tokens
        self.calls += 1
        if measured:
            self.measured_calls += 1
        else:
            self.estimated_calls += 1
        self.events.append(
            {
                "ts": time.time(),
                "label": label,
                "prompt_tokens": prompt_tokens,
                "completion_tokens": completion_tokens,
                "cost_usd": cost,
                "spent_usd": self.spent_usd,
                "measured": measured,
            }
        )
        if self.spent_usd >= self.limit_usd:
            self.stopped = True
            kind = "measured+estimated" if self.measured_calls else "estimated"
            self.stop_reason = f"{kind} spend ${self.spent_usd:.4f} >= limit ${self.limit_usd:.2f}"

    def charge_cell(
        self,
        label: str,
        *,
        scenario: str,
        events: list[dict] | None = None,
    ) -> None:
        from usage import sum_usage_from_events  # local package import

        if events:
            prompt, completion, measured = sum_usage_from_events(events)
            if measured:
                self.charge(prompt, completion, label, measured=True)
                return
        if scenario == "happy_path":
            self.charge(self.estimate_happy_prompt, self.estimate_happy_completion, label, measured=False)
        else:
            self.charge(self.estimate_other_prompt, self.estimate_other_completion, label, measured=False)

    def allow(self) -> bool:
        return not self.stopped

    def dump(self, path: Path) -> None:
        path.write_text(json.dumps(asdict(self), indent=2) + "\n")
