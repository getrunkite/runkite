export class CheckpointerManager {
    checkpointer = null;
    mode = "memory";
    closeFn = null;
    async start(postgresDsn) {
        if (postgresDsn) {
            const { PostgresSaver } = await import("@langchain/langgraph-checkpoint-postgres");
            const saver = PostgresSaver.fromConnString(postgresDsn);
            await saver.setup();
            this.checkpointer = saver;
            this.mode = "direct-postgres";
            this.closeFn = async () => {
                await saver.end();
            };
            console.log("checkpoint mode: direct (postgres) -- state survives runner restarts");
        }
        else {
            const { MemorySaver } = await import("@langchain/langgraph");
            this.checkpointer = new MemorySaver();
            this.mode = "memory";
            console.warn("checkpoint mode: in-memory (no POSTGRES_DSN set) -- " +
                "thread state will NOT survive a runner restart. " +
                "Set POSTGRES_DSN for production persistence.");
        }
    }
    async stop() {
        if (this.closeFn)
            await this.closeFn();
    }
    /** Overrides a compiled graph's checkpointer with the shared one -- checkpoint
     * mode is a runner concern, not something agent authors configure in graph.ts. */
    attach(graph) {
        graph.checkpointer = this.checkpointer;
    }
}
