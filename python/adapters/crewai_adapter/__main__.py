import argparse
import asyncio
import logging
import os

from runkite_runner.generic_worker import run_worker

from .adapter import CrewAIAdapter


def main():
    parser = argparse.ArgumentParser(description="Runkite CrewAI runner")
    parser.add_argument("--config", default="langgraph.json", help="Path to langgraph.json")
    parser.add_argument(
        "--grpc-address",
        default=os.environ.get("RUNKITE_GRPC_URL", "localhost:50051"),
        help="Control plane gRPC address (env: RUNKITE_GRPC_URL)",
    )
    parser.add_argument("--runner-kind", default="python-crewai", help="Runner kind identifier")
    parser.add_argument(
        "--concurrency",
        type=int,
        default=int(os.environ.get("RUNKITE_CONCURRENCY", "1")),
        help="Max concurrent in-flight jobs per runner process (env: RUNKITE_CONCURRENCY, default: 1)",
    )
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")

    asyncio.run(run_worker(CrewAIAdapter(), args.config, args.grpc_address, args.runner_kind, args.concurrency))


if __name__ == "__main__":
    main()
