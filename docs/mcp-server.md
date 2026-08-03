# MCP Server

> Deep dive moved from the root README. For a 60-second overview see the [root README](../README.md).

The reverse direction of Connectors: Connectors let a Runkite *agent* consume external MCP tool servers (MCP-client); this exposes Runkite's *own* configured agents *as* MCP tools, so any MCP-speaking client (IDEs, desktop apps, or your own tooling) can call them directly.

```
POST/GET/DELETE /mcp   Streamable HTTP MCP transport (session-based, per the 2025-03-26/2025-06-18/2025-11-25 spec revisions)
```

Every configured agent becomes exactly one MCP tool, named after its `agent_id`, using its `description` if set. Calling one dispatches a real run through the exact same `createRunCtx` + wait-for-result path every client-facing create-and-wait endpoint already uses (the same pattern A2A reuses for agent-to-agent calls) and blocks until it finishes -- MCP's `tools/call` is inherently request/response, there's no streaming variant to map Runkite's own SSE/WebSocket paths onto.

```json
// tools/call arguments
{
  "message": "What's the weather in Boston?",   // wrapped as a single user turn -- the common case
  "input": { "messages": [...] },                 // OR: raw input, for a caller that knows this agent's own shape. Wins if both given.
  "thread_id": "existing-thread-id"                // optional: continue a conversation instead of starting fresh
}
```

The result's text content is the agent's last message; a run that errors or gets interrupted comes back as a **tool-level** error (`isError: true` in the content, not a raw MCP protocol failure) so the calling LLM can actually see what went wrong and self-correct, per the MCP spec's own guidance on tool errors.

Mounted as a normal client-facing route (`/mcp`, not under `/internal/*`), so it goes through the exact same auth middleware (API key/JWT) as every other endpoint -- an MCP client is configured with a Runkite API key exactly the way any other Agent Protocol client would be. A session's tenant is fixed for its lifetime, resolved once at session establishment (the natural behavior for how MCP clients are actually configured in practice -- one static key per configured server entry) -- see `internal/api/mcpserver.go`'s doc comment for why the request context itself, not just the tenant/identity it carries, can't be reused across a session's later calls.

Uses the stateful (session-ID-based) Streamable HTTP transport rather than the newest stateless-only mode, since real-world MCP clients today predominantly still speak the older, session-based protocol revisions -- the official Go SDK (`github.com/modelcontextprotocol/go-sdk`) negotiates the right one automatically per client.

**Session security: bound to whoever created it.** The MCP Go SDK has its own session-hijack protection, but it keys off an OAuth-specific field none of Runkite's auth providers (API key, JWT) ever populate -- silently making that check a no-op here. Closed with Runkite's own middleware instead: the tenant + identity that established a session is recorded when it's created, and every later request against that same `Mcp-Session-Id` must match, or it's rejected with `403` -- confirmed live: a second, otherwise perfectly valid credential presenting a leaked or guessed session ID from a DIFFERENT tenant is rejected outright, not silently continued under the original tenant. This ownership record is also given an explicit TTL (30 minutes idle, swept every 5) so it doesn't grow unbounded over a long-running process -- and the SDK's own `StreamableHTTPOptions.SessionTimeout` is set to expire *before* that TTL can ever elapse (25 minutes), not left at the SDK's default of "never." A review caught this ordering as a real gap during verification: leaving the SDK's own session immortal while Runkite's ownership record still expired on schedule meant the hijack window reopened after 30 minutes of activity instead of never -- the ownership record would be swept, and the (still-alive) SDK session would then answer a mismatched caller's request instead of ever reaching the check. Fixing it just meant configuring the SDK's own idle timeout deliberately rather than leaving it unset.

**Multi-instance: `/mcp` needs sticky routing, unlike the rest of the API.** Session state (the SDK's own, and Runkite's session-ownership tracking above) lives in one replica's process memory -- a client's `initialize` landing on replica A, then a later call for that same session landing on replica B via plain round-robin, gets a genuine `404 session not found` from B. `docker-compose.multi.yml`'s `deploy/nginx-multi.conf` handles this with `hash $remote_addr consistent` on a dedicated `/mcp` upstream (open-source nginx, no nginx-plus or external session store needed) -- confirmed live, including a real pitfall found while testing it: hashing on the session ID itself (the more obvious-seeming choice) does NOT work, since that ID is an opaque, effectively random string a replica only assigns AFTER already being picked by some OTHER key, so its own hash value has no relationship to which replica actually holds it -- confirmed by reproducing the exact failure this way before switching to the client-address-based hash, which is consistent across a session's entire lifetime by construction (the key never changes), not by chance. If you're running a self-managed multi-instance deployment without this nginx config, you need the equivalent (or a single `/mcp`-serving replica) for MCP sessions to survive longer than one request.
