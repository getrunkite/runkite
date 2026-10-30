import argparse
import asyncio
import os

from runkite_runner.generic_worker import run_worker
from runkite_runner.logging_config import setup_logging

from .adapter import PydanticAIAdapter


def main() -> None:
    parser = argparse.ArgumentParser(description="Runkite managed Pydantic AI runner")
    parser.add_argument("--config", default="langgraph.json")
    parser.add_argument("--grpc-address", default=os.environ.get("RUNKITE_GRPC_URL", "localhost:50051"))
    parser.add_argument("--http-address", default=os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026"))
    parser.add_argument("--runner-kind", default="python-pydantic-ai")
    parser.add_argument("--concurrency", type=int, default=int(os.environ.get("RUNKITE_CONCURRENCY", "1")))
    args = parser.parse_args()
    setup_logging()
    adapter = PydanticAIAdapter()
    adapter.configure(
        http_address=args.http_address, runner_kind=args.runner_kind, runner_token=os.environ.get("RUNNER_TOKEN")
    )
    asyncio.run(
        run_worker(
            adapter, args.config, args.grpc_address, args.runner_kind, args.concurrency, http_address=args.http_address
        )
    )


if __name__ == "__main__":
    main()
