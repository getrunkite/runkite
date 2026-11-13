"""Select a queue identity without changing the chosen SDK adapter."""

import os
import re

_POOL_NAME = re.compile(r"[a-z][a-z0-9-]{0,30}\Z")


def pool_runner_kind(runner_kind: str, execution_pool: str | None) -> str:
    if not execution_pool:
        validate_pool_environment(runner_kind)
        return runner_kind
    if not _POOL_NAME.fullmatch(execution_pool):
        raise ValueError("execution pool must be a lowercase name of at most 31 characters")
    if runner_kind.startswith("pool-") and runner_kind != f"pool-{execution_pool}":
        raise ValueError("runner kind and execution pool disagree")
    kind = f"pool-{execution_pool}"
    validate_pool_environment(kind)
    return kind


def validate_pool_environment(runner_kind: str) -> None:
    if runner_kind.startswith("pool-") and any(
        os.environ.get(key)
        for key in ("POSTGRES_DSN", "MYSQL_DSN", "MONGO_URI", "REDIS_URL", "NATS_URL", "KAFKA_URL", "DATABASE_PATH")
    ):
        raise ValueError("execution pool workers require proxy mode without control-plane backend configuration")
