import argparse
import asyncio
import os

from runkite_runner.generic_worker import run_worker
from runkite_runner.logging_config import setup_logging

from .adapter import AgentFrameworkAdapter


def main() -> None:
    parser = argparse.ArgumentParser(description="Runkite managed Microsoft Agent Framework runner")
    parser.add_argument("--config", default="langgraph.json", help="Path to langgraph.json")
    parser.add_argument(
        "--grpc-address",
        default=os.environ.get("RUNKITE_GRPC_URL", "localhost:50051"),
        help="Control plane gRPC address (env: RUNKITE_GRPC_URL)",
    )
    parser.add_argument(
        "--runner-kind",
        default="python-agent-framework",
        help="Runner kind identifier",
    )
    parser.add_argument(
        "--concurrency",
        type=int,
        default=int(os.environ.get("RUNKITE_CONCURRENCY", "1")),
        help="Max concurrent in-flight jobs per runner process (env: RUNKITE_CONCURRENCY, default: 1)",
    )
    parser.add_argument(
        "--http-address",
        default=os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026"),
        help="Control plane HTTP address for opaque checkpoints (env: RUNKITE_HTTP_URL)",
    )
    args = parser.parse_args()
    setup_logging()
    adapter = AgentFrameworkAdapter()
    adapter.configure(
        http_address=args.http_address,
        runner_kind=args.runner_kind,
        runner_token=os.environ.get("RUNNER_TOKEN"),
    )
    asyncio.run(
        run_worker(
            adapter,
            args.config,
            args.grpc_address,
            args.runner_kind,
            args.concurrency,
            http_address=args.http_address,
        )
    )


if __name__ == "__main__":
    main()
