"""Optional Microsoft Agent Framework function middleware.

Install this last, after argument-repair middleware. Only JSON-native function
arguments and JSON defaults stable under native validation are supported. The
application retains its loop, execution idempotency and approval continuation.
Importing runkite_governance itself does not import this optional dependency.

Execution arguments remain ordinary native dict/list containers. The framework
may reuse prepared argument aliases, so the application, middleware and tool
must not mutate invocation data after authorization. This is a pre-execution
gate, not an immutable execution environment. Pending records are immutable.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import math
import threading
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass
from types import MappingProxyType
from typing import Any

from agent_framework import FunctionInvocationContext, FunctionMiddleware, FunctionTool, MiddlewareFailure

from . import GovernanceClient

__all__ = [
    "GovernedFunction",
    "GovernanceFunctionMiddleware",
    "GovernanceBlockedError",
    "PendingInvocation",
]


@dataclass(frozen=True)
class GovernedFunction:
    function: FunctionTool
    connector: str
    tool: str | None = None


@dataclass(frozen=True)
class PendingInvocation:
    action_id: str
    agent_id: str
    connector: str
    tool: str
    run_id: str
    arguments: Mapping[str, Any]


class GovernanceBlockedError(MiddlewareFailure):
    """A denied or pending operation. Arguments are never attached to this error."""

    def __init__(self, decision: Mapping[str, str]) -> None:
        copied = _decision(dict(decision))
        if copied["effect"] == "allow":
            raise MiddlewareFailure("A blocked governance decision cannot allow execution")
        super().__init__(
            "Runkite tool execution is pending approval"
            if copied["effect"] == "pending"
            else "Runkite tool execution was denied"
        )
        self.decision = copied


@dataclass(frozen=True)
class _Binding:
    function: FunctionTool
    connector: str
    tool: str
    name: str
    func: Callable[..., Any]
    model: Any
    signature: inspect.Signature
    properties: tuple[str, ...]
    schema: bytes


def _identifier(value: Any) -> str:
    if type(value) is not str or not value or value != value.strip():
        raise ValueError("Governance identifiers must be canonical nonempty strings")
    encoded = value.encode("utf-8", errors="strict")
    if len(encoded) > 256:
        raise ValueError("Governance identifiers must be at most 256 UTF-8 bytes")
    return value


def _snapshot(value: Any, *, root: bool = True) -> Any:
    """Copy exact JSON types without custom serialization or coercion."""
    ancestors: set[int] = set()

    def copy(item: Any) -> Any:
        kind = type(item)
        if item is None or kind is bool:
            return item
        if kind is str:
            item.encode("utf-8", errors="strict")
            return item
        if kind is int:
            if abs(item) > 2**53 - 1:
                raise ValueError("Tool integers must be in the safe JSON integer range")
            return item
        if kind is float:
            if not math.isfinite(item) or item == 0 and math.copysign(1, item) < 0:
                raise ValueError("Tool numbers must be lossless finite JSON values")
            return item
        if kind not in (dict, list) or id(item) in ancestors:
            raise ValueError("Tool arguments must contain exact JSON types without cycles")
        ancestors.add(id(item))
        try:
            if kind is list:
                return [copy(part) for part in item]
            result = {}
            for key, part in item.items():
                if type(key) is not str:
                    raise ValueError("Tool argument keys must be strings")
                key.encode("utf-8", errors="strict")
                result[key] = copy(part)
            return result
        finally:
            ancestors.remove(id(item))

    if root and type(value) is not dict:
        raise ValueError("Tool arguments must be a JSON object")
    return copy(value)


def _encoded(value: Any) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False).encode("utf-8")


def _freeze(value: Any) -> Any:
    if type(value) is dict:
        return MappingProxyType({key: _freeze(part) for key, part in value.items()})
    if type(value) is list:
        return tuple(_freeze(part) for part in value)
    return value


def _decision(raw: Any) -> Mapping[str, str]:
    if type(raw) is not dict:
        raise ValueError("Governance decision must be an object")
    copied = raw.copy()
    effect = copied.get("effect")
    if type(effect) is not str or effect not in ("allow", "deny", "pending"):
        raise ValueError("Governance decision is missing a valid effect")
    result = {"effect": effect}
    for key in ("reason", "reason_code", "action_id"):
        if key in copied:
            if type(copied[key]) is not str:
                raise ValueError("Governance decision metadata must be strings")
            result[key] = copied[key]
    if effect == "pending":
        _identifier(result.get("action_id"))
    return MappingProxyType(result)


def _binding(config: GovernedFunction) -> _Binding:
    function = config.function
    if not isinstance(function, FunctionTool) or not (
        inspect.isfunction(function.func) or inspect.ismethod(function.func)
    ):
        raise ValueError("Governed functions must wrap ordinary Python callables")
    signature = inspect.signature(function.func)
    if any(
        parameter.kind
        in (inspect.Parameter.VAR_POSITIONAL, inspect.Parameter.VAR_KEYWORD, inspect.Parameter.POSITIONAL_ONLY)
        for parameter in signature.parameters.values()
    ):
        raise ValueError("Variadic and positional-only governed functions are unsupported")
    schema = _snapshot(function.parameters())
    properties = schema.get("properties", {})
    if type(properties) is not dict or any(name not in signature.parameters for name in properties):
        raise ValueError("Governed function schemas must name declared Python parameters")
    return _Binding(
        function,
        _identifier(config.connector),
        _identifier(function.name if config.tool is None else config.tool),
        _identifier(function.name),
        function.func,
        function.input_model,
        signature,
        tuple(properties),
        _encoded(schema),
    )


def _projection(binding: _Binding, source: dict[str, Any]) -> dict[str, Any]:
    if any(name not in binding.properties for name in source):
        raise ValueError("Tool arguments must name declared tool parameters")
    bound = binding.signature.bind_partial(**source)
    bound.apply_defaults()
    projected = _snapshot({name: bound.arguments[name] for name in binding.properties if name in bound.arguments})
    if any(
        name not in projected
        for name in binding.properties
        if binding.signature.parameters[name].default is inspect.Parameter.empty
    ):
        raise ValueError("Tool arguments are missing a required parameter")
    if binding.model is not None:
        validated = binding.model.model_validate(_snapshot(projected)).model_dump(mode="python", exclude_unset=True)
        if _encoded(_snapshot(validated)) != _encoded(projected):
            raise ValueError("Tool normalization must preserve the governed JSON arguments")
    return projected


def _unchanged(binding: _Binding, context: FunctionInvocationContext, original: Any, before: bytes) -> None:
    if (
        context.function is not binding.function
        or binding.function.name != binding.name
        or binding.function.func is not binding.func
        or binding.function.input_model is not binding.model
        or context.arguments is not original
        or _encoded(_snapshot(original)) != before
        or _encoded(_snapshot(context.arguments)) != before
        or _encoded(_snapshot(binding.function.parameters())) != binding.schema
    ):
        raise ValueError("The governed function or its arguments changed during the decision")


def _check_cancellation() -> None:
    task = asyncio.current_task()
    if task is not None and task.cancelling():
        raise asyncio.CancelledError


class GovernanceFunctionMiddleware(FunctionMiddleware):
    """Decide once before native execution. Install last, after argument repair.

    No polling, approval replay or model-loop ownership is provided. Pending
    persistence and exactly-once application side effects remain app concerns.
    """

    def __init__(
        self,
        client: GovernanceClient,
        *,
        agent_id: str,
        functions: Sequence[GovernedFunction],
        operation_id: Callable[[FunctionInvocationContext], str],
        on_pending: Callable[[PendingInvocation], Awaitable[None]] | None = None,
    ) -> None:
        try:
            self._agent_id = _identifier(agent_id)
            self._decide = client.decide
            if not callable(self._decide) or not callable(operation_id):
                raise ValueError("Governance decision and operation ID callbacks must be callable")
            if on_pending is not None and not callable(on_pending):
                raise ValueError("The pending callback must be callable")
            self._bindings = tuple(_binding(config) for config in functions)
            if len({id(binding.function) for binding in self._bindings}) != len(self._bindings):
                raise ValueError("Governed function objects must be unique")
            self._operation_id = operation_id
            self._on_pending = on_pending
            self._bridges = asyncio.Semaphore(32)
        except Exception as exc:
            raise MiddlewareFailure("Invalid Runkite governance middleware configuration") from exc

    async def process(self, context: FunctionInvocationContext, call_next: Callable[[], Awaitable[None]]) -> None:
        cancelled = threading.Event()
        try:
            _check_cancellation()
            binding = next((item for item in self._bindings if context.function is item.function), None)
            if binding is None:
                raise ValueError("This function object is not registered for governance")
            original = context.arguments
            source = _snapshot(original)
            before = _encoded(source)
            projected = _projection(binding, source)
            run_id = _identifier(self._operation_id(context))
            _unchanged(binding, context, original, before)
            request_args = _snapshot(projected)
            async with self._bridges:
                raw = await asyncio.to_thread(
                    self._decide,
                    agent_id=self._agent_id,
                    connector=binding.connector,
                    tool=binding.tool,
                    args=request_args,
                    run_id=run_id,
                    cancelled=cancelled,
                )
            _check_cancellation()
            decision = _decision(raw)
            _unchanged(binding, context, original, before)
            if _encoded(_snapshot(request_args)) != _encoded(projected):
                raise ValueError("Governance request arguments changed during the decision")
            if decision["effect"] == "pending" and self._on_pending is not None:
                await self._on_pending(
                    PendingInvocation(
                        decision["action_id"],
                        self._agent_id,
                        binding.connector,
                        binding.tool,
                        run_id,
                        _freeze(projected),
                    )
                )
            if decision["effect"] != "allow":
                raise GovernanceBlockedError(decision)
            _check_cancellation()
            # Preserve ordinary dict/list semantics, including nested Any values.
            # Native prepared-argument caches may reuse checked original aliases.
            context.arguments = _snapshot(projected)
        except asyncio.CancelledError:
            cancelled.set()
            raise
        except MiddlewareFailure:
            raise
        except Exception as exc:
            raise MiddlewareFailure("Runkite governance check failed") from exc
        finally:
            cancelled.set()
        # Native tool failures retain the framework's handling. Never retry them.
        await call_next()
