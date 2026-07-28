#!/usr/bin/env node
/**
 * CLI entrypoint, mirroring the Python runner's `python -m runkite_runner.worker`.
 *
 *   npx runkite-runner --config langgraph.json --grpc-address localhost:50051
 */
import { runWorker } from "./worker.js";

function parseArgs(argv: string[]): Record<string, string> {
  const args: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (arg.startsWith("--")) {
      const key = arg.slice(2);
      const value = argv[i + 1] && !argv[i + 1].startsWith("--") ? argv[++i] : "true";
      args[key] = value;
    }
  }
  return args;
}

async function main(): Promise<void> {
  const args = parseArgs(process.argv.slice(2));

  const concurrencyRaw = args["concurrency"] ?? process.env.RUNKITE_CONCURRENCY ?? "1";
  const concurrency = Number.parseInt(concurrencyRaw, 10);
  if (!Number.isInteger(concurrency) || concurrency < 1) {
    throw new Error(`--concurrency must be a positive integer, got ${JSON.stringify(concurrencyRaw)}`);
  }

  const opts = {
    configPath: args.config ?? "langgraph.json",
    grpcAddress: args["grpc-address"] ?? process.env.RUNKITE_GRPC_URL ?? "localhost:50051",
    httpAddress: args["http-address"] ?? process.env.RUNKITE_HTTP_URL ?? "http://localhost:2026",
    runnerKind: args["runner-kind"] ?? "typescript-langgraphjs",
    concurrency,
  };

  await runWorker(opts);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
