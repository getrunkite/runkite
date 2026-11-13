#!/usr/bin/env node
/**
 * CLI entrypoint, mirroring the Python runner's `python -m runkite_runner.worker`.
 *
 *   npx runkite-runner --config langgraph.json --grpc-address localhost:50051
 */
import { logger } from "./logger.js";
import { poolRunnerKind } from "./executionPool.js";
const help = `Usage: runkite-runner [options]

Run a LangGraph.js or AI SDK agent through a Runkite control plane.

Options:
  -h, --help                 Show this help without loading agent configuration
  --config PATH              Agent configuration (default: langgraph.json)
  --framework NAME           langgraphjs (default) or ai-sdk
  --grpc-address HOST:PORT   Control plane gRPC address
                             (RUNKITE_GRPC_URL; default: localhost:50051)
  --http-address URL         Control plane HTTP address
                             (RUNKITE_HTTP_URL; default: http://localhost:2026)
  --runner-kind KIND         Override the selected framework's runner kind
  --execution-pool NAME      Use the registered pool-NAME queue
                             (RUNKITE_EXECUTION_POOL)
  --concurrency COUNT        Positive safe integer (RUNKITE_CONCURRENCY; default: 1)

Authentication: set RUNNER_TOKEN to the worker's control plane credential.
For a pool, use its dedicated token and configure the matching framework.
Proxy persistence: set RUNKITE_HTTP_URL and leave POSTGRES_DSN unset.
Pool workers also reject other control plane database and queue settings.
Use RUNKITE_GRPC_TLS=1 for gRPC TLS with system trust, or configure
RUNKITE_TLS_CA_FILE and optional RUNKITE_TLS_CLIENT_CERT_FILE plus
RUNKITE_TLS_CLIENT_KEY_FILE.
`;
function parseArgs(argv) {
    const args = {};
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
async function main() {
    const argv = process.argv.slice(2);
    if (argv.includes("--help") || argv.includes("-h")) {
        process.stdout.write(help);
        return;
    }
    const args = parseArgs(argv);
    const concurrencyRaw = args["concurrency"] ?? process.env.RUNKITE_CONCURRENCY ?? "1";
    const concurrency = Number(concurrencyRaw);
    if (!/^\d+$/.test(concurrencyRaw) || !Number.isSafeInteger(concurrency) || concurrency < 1) {
        throw new Error(`--concurrency must be a positive integer, got ${JSON.stringify(concurrencyRaw)}`);
    }
    const opts = {
        configPath: args.config ?? "langgraph.json",
        grpcAddress: args["grpc-address"] ?? process.env.RUNKITE_GRPC_URL ?? "localhost:50051",
        httpAddress: args["http-address"] ?? process.env.RUNKITE_HTTP_URL ?? "http://localhost:2026",
        runnerKind: poolRunnerKind(args["runner-kind"] ?? (args.framework === "ai-sdk" ? "typescript-ai-sdk" : "typescript-langgraphjs"), args["execution-pool"] ?? process.env.RUNKITE_EXECUTION_POOL),
        concurrency,
    };
    const { runWorker } = await import("./worker.js");
    if (args.framework === "ai-sdk") {
        const { register } = await import("tsx/esm/api");
        register();
        const { AiSdkAdapter } = await import("./aiSdkAdapter.js");
        await runWorker({
            ...opts,
            adapter: new AiSdkAdapter(opts.configPath, {
                httpAddress: opts.httpAddress,
                runnerKind: opts.runnerKind,
                runnerToken: process.env.RUNNER_TOKEN,
            }),
        });
    }
    else if (!args.framework || args.framework === "langgraphjs") {
        await runWorker(opts);
    }
    else {
        throw new Error(`unsupported framework ${args.framework}; use langgraphjs or ai-sdk`);
    }
}
main().catch((err) => {
    logger.error("fatal error", err);
    process.exit(1);
});
