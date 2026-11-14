/** Select the operator's queue identity without switching the SDK adapter. */
export function poolRunnerKind(runnerKind: string, executionPool?: string): string {
  if (!executionPool) {
    validatePoolEnvironment(runnerKind);
    return runnerKind;
  }
  if (!/^[a-z][a-z0-9-]{0,30}$/.test(executionPool)) {
    throw new Error("execution pool must be a lowercase name of at most 31 characters");
  }
  const kind = `pool-${executionPool}`;
  if (runnerKind.startsWith("pool-") && runnerKind !== kind) {
    throw new Error("runner kind and execution pool disagree");
  }
  validatePoolEnvironment(kind);
  return kind;
}

export function validatePoolEnvironment(runnerKind: string): void {
  if (
    runnerKind.startsWith("pool-") &&
    ["POSTGRES_DSN", "MYSQL_DSN", "MONGO_URI", "REDIS_URL", "NATS_URL", "KAFKA_URL", "DATABASE_PATH"].some((key) =>
      Boolean(process.env[key]),
    )
  ) {
    throw new Error("execution pool workers require proxy mode without control-plane backend configuration");
  }
}
