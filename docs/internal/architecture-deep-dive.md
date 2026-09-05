# Runkite — Complete Technical Explanation

> **What this document is.** These are study notes, written so the founder (or anyone who inherits this codebase) can understand every part of the product without reading raw code first. It was written after the v0.2 cut and updated after v0.3 shipped — chapters 1-93 are the original v0.2-era walkthrough; chapters 94 onward cover what changed since (FinOps, the run manifest, universal checkpoints, RLS, and the hardening pass before the v0.3.0 tag). If you found this file by browsing the repo: welcome, and I hope it's actually useful rather than just long.

**Author: Opus 4.6 (Anthropic), updated by Claude Sonnet 5 (Anthropic) for the v0.3.0 pass**

---

## What This Document Is

This document explains Runkite from absolute zero. If you have never seen the codebase, never heard of Agent Protocol, and only vaguely know what gRPC means, you should still be able to read this top-to-bottom and come away understanding every component, how they connect, why they exist, and where the code lives.

I will explain things the way I would if we were sitting together at a whiteboard. When I use a technical term for the first time, I will explain it right there. I will not assume you remember something from a previous paragraph — I will reconnect ideas explicitly.

The document is long on purpose. Brevity creates confusion. I would rather over-explain one concept than have you re-read something three times trying to figure out what I meant.

---



## Table of Contents

**Core Architecture (Chapters 1-22)**

1. [The Big Picture — What Is Runkite?](#1-the-big-picture)
2. [The Three Processes — Control Plane, Runner, Client](#2-the-three-processes)
3. [The Two Protocols — How They Talk](#3-the-two-protocols)
4. [A Complete Run From Start to Finish](#4-a-complete-run-from-start-to-finish)
5. [Multi-Replica — Running Multiple Control Planes](#5-multi-replica)
6. [Thread and Run Lifecycles — The Status Machines](#6-thread-and-run-lifecycles)
7. [Cancel — How to Stop a Running Agent](#7-cancel)
8. [Crash Recovery — What Happens When Things Die](#8-crash-recovery)
9. [Agent-to-Agent Communication](#9-agent-to-agent)
10. [Human In The Loop — Two Completely Different Kinds](#10-human-in-the-loop)
11. [Governance — Who Can Do What](#11-governance)
12. [Connectors — How Agents Use External Tools Safely](#12-connectors)
13. [Cron — Scheduled Runs](#13-cron)
14. [The Admin UI — What Operators See](#14-the-admin-ui)
15. [Rate Limiting](#15-rate-limiting)
16. [Fail-Closed Serve — Why Production Refuses to Start Insecurely](#16-fail-closed-serve)
17. [Conformance — How We Test Multiple Backends](#17-conformance)
18. [Gaps and Honest Limitations](#18-gaps-and-honest-limitations)
19. [Observability — OpenTelemetry and Prometheus](#19-observability)
20. [Boot Path — What Happens When the Server Starts](#20-boot-path)
21. [Security Findings — What We Fixed and What Remains](#21-security-findings)
22. [File Map — Where Everything Lives in the Code](#22-file-map)

**Deep Dives (Chapters 23-93)** — detailed explorations of specific mechanisms, extended narratives, failure scenarios, analogies, and the topics added in the second pass:

- 81: Factory Graphs and ServerRuntime
- 82: LLM Response Cache
- 83: mTLS (Mutual TLS)
- 84: Agent Versioning and Rollback
- 85: MCP Server Mode (Exposing Agents AS MCP Tools)
- 86: Deployment: Helm, Docker Compose, and kind
- 87: Agent Registry (The Marketplace)
- 88: How the Admin UI Is Embedded in the Go Binary
- 89: How Rate Limiting Actually Works (Token Bucket)
- 90: Why Governance Requires SQL (FOR UPDATE) — includes Postgres RLS, shipped in v0.3
- 91: The Two (Now Three) Checkpoint Systems — includes universal opaque checkpoints, shipped in v0.3
- 92: The LLM Response Cache Hash Mechanism
- 93: Practice Questions (32 questions covering Chapters 1-92)

**Part Two — What v0.3 Added (Chapters 94-99):**

- 94: FinOps — Cost Governance, Not a Billing Platform
- 95: The FinOps Admin UX, and Why "100% Accurate Cost" Isn't a Real Target
- 96: The Run Manifest ("Blueprint-lite") — Freezing Intent at Dispatch Time
- 97: Admin UI Additions Since v0.2 — Try-Agent and the Catalog Editor
- 98: Cutting a Release — What Actually Happens for a Version Bump
- 99: Practice Questions — Part Two (v0.3 Additions)

---



# 1. The Big Picture



## What problem does Runkite solve?

You want to run AI agents in production. These agents use frameworks like LangGraph, CrewAI, LlamaIndex, or AutoGen. Each framework has its own way of running agents, storing state, and managing conversations.

The problem: if you run agents directly, you have no central control. Each framework manages its own execution. You cannot see what all your agents are doing. You cannot enforce security policies across them. You cannot kill a runaway agent. You cannot require human approval before a dangerous action. You have no single place to look at logs, manage credentials, or enforce rate limits.

Runkite solves this by sitting in the middle. It is a **control plane** — a single Go binary that coordinates everything. Think of it like air traffic control for your AI agents. The agents (planes) do the actual flying (LLM calls, tool use), but the control plane decides who can fly, where, when, and with what fuel (credentials). Every agent, regardless of framework, must go through this one control plane.

## What Runkite is NOT

Runkite does not run your agent's code. It does not import LangGraph or CrewAI. It does not call OpenAI or Anthropic. It does not scan prompts for injection attacks or PII. Those are different products solving different problems.

Runkite coordinates: it receives requests from your application, dispatches work to agent runners, streams results back to your application, manages credentials, enforces policies, and keeps audit trails. The actual AI execution happens in separate worker processes called "runners" that DO import the frameworks and DO call LLMs.

## The one-sentence version

Runkite is a self-hosted control plane that sits between your application and your AI agent workers, giving you dispatch, streaming, multi-tenancy, authentication, policy enforcement, credential management, and observability — regardless of which agent framework your workers use.

## Where the code lives

The entire project lives in this one repository. The control plane is written in Go. The runners are in Python and TypeScript. The Admin UI is React. It ships as one binary with the UI embedded.

---



# 2. The Three Processes

When Runkite is running, there are three types of processes involved. Understanding what each one does and does NOT do is the foundation for everything else.

## Process 1: The Client (your application)

This is the application your users interact with. It could be a web app, a mobile backend, a CLI tool, or anything that wants to use an AI agent. The client speaks to Runkite over standard HTTP, Server-Sent Events (SSE), or WebSocket. It uses the "Agent Protocol" — a public, standardized API for managing AI agent conversations.

What the client can do:

- Create a conversation thread (like opening a new chat)
- Start a run on that thread (like sending a message and waiting for a response)
- Stream the agent's response in real-time (tokens appearing one by one)
- Cancel a running agent
- Resume an interrupted conversation

What the client never sees:

- How the agent is implemented (LangGraph vs CrewAI vs anything else)
- Which runner process is executing the agent
- gRPC (the internal protocol between control plane and runners)
- Redis keys, generation tokens, or any internal coordination details

The client only knows: "I send HTTP requests to this API and get responses/streams back."

**Code:** The client is YOUR code — it uses Runkite's public API. Example SDKs exist for Python and TypeScript (the same SDKs that LangGraph Platform uses, because Runkite implements the same Agent Protocol API shape).

## Process 2: The Control Plane (Runkite itself)

This is the Go binary. You start it with `runkite serve` (production) or `runkite dev` (local development). It is the brain of the operation.

What the control plane does:

- Receives HTTP requests from clients (Agent Protocol)
- Authenticates and authorizes those requests
- Stores run/thread/agent metadata in a database (Postgres, MySQL, SQLite, or MongoDB)
- Dispatches work to runners via a job queue (Redis, NATS, or Kafka)
- Streams events from runners back to clients (via Redis fan-out)
- Manages connector credentials (API keys, OAuth tokens for external services)
- Enforces policy decisions (can this agent call this tool?)
- Provides the Admin UI for operators
- Handles cancellation, timeouts, crash recovery
- Exposes gRPC for runners to connect to

What the control plane does NOT do:

- Import or run any agent framework code
- Call OpenAI, Anthropic, or any LLM
- Execute any user-defined Python or TypeScript
- Parse or inspect prompt content

The control plane is framework-agnostic. It does not know or care whether the runner uses LangGraph, CrewAI, or a hand-written Python script. All it knows is the Runner Protocol (five gRPC calls).

**Code:** `cmd/serve.go` is the main entry point. `internal/api/` has all the HTTP handlers. `internal/bridge/` has the gRPC server that runners connect to.

## Process 3: The Runner (the worker that actually runs agent code)

This is where your agent framework lives. A runner is a Python or TypeScript process that connects to the control plane via gRPC. It polls for work, executes agent graphs, streams events back, and reports completion.

What the runner does:

- Connects to the control plane over gRPC
- Long-polls for work assignments (GetJob)
- Loads and executes agent framework code (LangGraph graphs, CrewAI crews, etc.)
- Calls LLMs (OpenAI, Anthropic, etc.) as part of agent execution
- Streams progress events back to the control plane (StreamEvents)
- Reports final status when done (ReportStatus)
- Sends periodic heartbeats to prove it is still alive (Heartbeat)
- Listens for cancel signals (WatchCancels)
- Calls back to the control plane for connector access, storage, and vector operations

What the runner does NOT do:

- Serve HTTP to end users
- Manage authentication or tenancy (the control plane handles that)
- Decide security policy (the control plane decides)
- Store durable run metadata (that is in Postgres via the control plane)

**Code:** Python runner at `python/runkite_runner/`. TypeScript runner at `typescript/runkite-runner/`. Framework adapters at `python/adapters/`.

## How the three connect (the relay race)

```
Your App (Client)                Control Plane                 Runner
     |                                |                          |
     |-- HTTP "create run" ---------> |                          |
     |                                |-- write Postgres         |
     |                                |-- push to Redis queue    |
     |                                |                          |
     |                                | <--- gRPC GetJob --------|
     |                                |--- assignment ---------->|
     |                                |                          |
     |                                |                          |-- calls LLM
     |                                |                          |-- gets tokens
     |                                |                          |
     |                                | <--- StreamEvents -------|
     |                                |-- publish to Redis       |
     | <-- SSE events --------------- |                          |
     |                                |                          |
     |                                | <--- ReportStatus -------|
     |                                |-- update Postgres        |
     | <-- final response ----------- |                          |
```

The client never talks to the runner. The runner never talks to the client. The control plane is always in the middle. This is what makes governance possible — because everything passes through one point, that one point can enforce rules.

## Why "publish to Redis" is in the middle of that diagram

Looking at the diagram above, you might wonder: why does the CP publish to Redis between receiving StreamEvents and sending SSE to the client? Why not just send tokens directly from the CP to the client?

**In single-replica mode (dev),** that is exactly what happens. The CP receives tokens from the runner via gRPC and pushes them directly to the client's SSE connection. No Redis needed. It is all one process.

**In production (multi-replica),** you have 2-3 CP replicas behind a load balancer. The problem: the client's SSE connection lands on CP-1, but the runner's gRPC connection lands on CP-2. CP-2 receives the tokens from the runner but has no idea a client is waiting on CP-1. CP-1 has no idea tokens are arriving on CP-2.

Redis solves this. When CP-2 receives tokens from the runner, it writes them to a Redis Stream keyed by run_id (XADD). CP-1, which has the client's SSE connection, has an XREAD goroutine tailing that same Redis Stream. CP-1 reads the events and delivers them to the client. Redis is the shared mailbox between them.

```
Runner ──gRPC StreamEvents──→ CP-2 ──XADD──→ Redis Stream "events:run_9f"
                                                           │
CP-1 ──XREAD (subscribed)── reads from Redis ──────────────┘
  │
  └──SSE──→ Client
```



## Critical mental model: normal operation uses exactly 2 CPs per run

In the normal (stable connection) case, a run involves exactly TWO CPs:

- **CP-1 (client-side):** receives the HTTP request, creates the run, subscribes to Redis Stream, holds SSE open, delivers events to client
- **CP-2 (runner-side):** dispatches the job via GetJob, receives ALL gRPC from the runner (StreamEvents, Heartbeat, WatchCancels), writes events to Redis Stream, handles ReportStatus, owns the cancel subscription

This is because the runner opens ONE gRPC channel (one HTTP/2 TCP connection) to the load balancer. The LB routes that connection to ONE CP (say CP-2). All RPCs over that channel — GetJob, StreamEvents, Heartbeat, WatchCancels — go to CP-2. The runner does NOT open separate connections per RPC.

**When could a third CP get involved?** Only in edge cases:
- The runner's gRPC connection drops and it reconnects (LB might route to CP-3). But this triggers heartbeat loss → reclaim → new generation, so the old CP's state cleans up.
- An admin cancel request arrives at CP-3 (a different CP than CP-1 or CP-2). CP-3 just publishes to Redis pub/sub — CP-2 (the dispatcher) picks it up. CP-3's involvement is one PUBLISH command and done.

For understanding the core data flow, think 2 CPs: one for the client, one for the runner, with Redis Stream as the bridge between them.

## The runner never touches Redis

The runner has no Redis URL, no Redis client, no Redis connection. It speaks ONLY gRPC to whatever CP the load balancer gave it. The CP is the one that reads and writes Redis on the runner's behalf. Redis is a CP-to-CP coordination layer, completely invisible to runners and clients.

Think of it as: runners talk to CPs, clients talk to CPs, and CPs talk to EACH OTHER through Redis. Postgres is where they record permanent history. Redis is how they coordinate in real-time.

## What happens if the CP holding the client's SSE connection dies?

The client's SSE connection drops (network error). What happens next:

1. **Client detects the drop.** The SSE EventSource fires an error event.
2. **Client reconnects automatically.** The new HTTP request goes through the load balancer and lands on a DIFFERENT, alive CP (say CP-2).
3. **CP-2 subscribes to the Redis Stream for that run.** It has never seen this run before in its own memory, but it does not need to — it just subscribes to `events:run_9f` on Redis.
4. **Replay from where the client left off.** SSE supports a `Last-Event-ID` header. The client sends the sequence number of the last event it received. CP-2 replays all events AFTER that sequence from Redis. Then continues streaming new ones live.

Result: the client experiences a brief hiccup (milliseconds to seconds), receives any missed events via replay, and the stream continues as if nothing happened. Zero data loss. This is why events are published to Redis (durable streams with replay) rather than kept in CP memory (lost if that CP dies).

---



# 3. The Two Protocols

There are exactly two communication protocols in Runkite. Understanding what each one carries and why they are different is the key mental model.

## Protocol 1: Agent Protocol (Client to Control Plane)

This is a public HTTP-based protocol. It is what your application speaks. It was originally designed for LangGraph Platform and Runkite implements the same API shape so existing SDKs work.

The protocol uses three transport mechanisms depending on what you need:

**Regular HTTP (request-response):** For creating threads, creating runs, getting run status, searching runs, managing store data. You send a request, you get a JSON response. Simple.

**Server-Sent Events (SSE):** For streaming agent output in real-time. SSE is a one-way stream from server to client over a regular HTTP connection. The server keeps the connection open and pushes events as they happen. Think of it like a radio broadcast — the server talks, you listen. When the agent produces tokens, they appear on your SSE stream in real-time.

How SSE works technically: the client makes a GET request with `Accept: text/event-stream`. The server responds with `Content-Type: text/event-stream` and keeps the connection open. It sends lines like `data: {"event": "values", "data": {...}}` followed by blank lines. The client receives these as they arrive. When the run finishes, the server sends a terminal event and closes.

**WebSocket:** For bidirectional real-time communication. Unlike SSE (one-way server-to-client), WebSocket lets both sides send messages at any time. In Runkite, WebSocket is used for the same streaming but also allows sending commands (like cancel, resume) without opening a separate HTTP request. The WebSocket connection starts as an HTTP upgrade request, then becomes a persistent bidirectional channel.

All three carry the same logical operations: create thread, create run, stream events, cancel, resume, store access, etc. They are different transport choices for the same API.

**What Agent Protocol does NOT carry:** It never carries gRPC. It never exposes internal concepts like generation tokens, runner_kind identifiers, inflight lease details, or Redis keys. From the client's perspective, there are threads, runs, agents, events, and store — that is the entire vocabulary.

## Protocol 2: Runner Protocol (Control Plane to Runner)

This is a private gRPC protocol. It is what runners speak to the control plane. It is defined in `proto/runner.proto` and documented in `runner-protocol/PROTOCOL.md`.

**What is gRPC?** gRPC is a framework for making remote procedure calls (function calls across a network). Unlike REST where you send HTTP requests to URL paths, gRPC defines exact function signatures with typed parameters and returns. It runs over HTTP/2, which allows multiplexing (multiple requests on one connection), streaming (long-lived data flows), and efficient binary serialization (smaller messages than JSON).

**Why gRPC instead of HTTP for runners?** Three reasons: (1) Long-polling — runners sit waiting for work; gRPC handles this natively with server-streaming. (2) Bidirectional streaming — events flow continuously from runner to control plane during execution. (3) Keepalive — gRPC has built-in TCP-level health checks that detect dead connections in seconds, which is critical for crash recovery.

The Runner Protocol has exactly **five RPCs** (remote procedure calls). That is it. Five functions cover everything a runner needs:

### RPC 1: GetJob

The runner calls this to say "I am ready for work, give me something to do." The control plane checks the Redis queue for work matching this runner's type (called `runner_kind` — for example, `python-langgraph` or `typescript-langgraph`). If there is work available, it returns an assignment with all the details the runner needs: what agent to run, what input to process, what thread this belongs to, what generation this attempt is (more on generation later), and context about the user who requested it.

If there is no work available, the call blocks (waits) until work arrives or a timeout happens. This is called "long-polling" — the runner holds the connection open, and the server responds only when there is something to deliver. This is more efficient than the runner asking every second "any work? any work? any work?"

**How GetJob works in multi-replica:** The runner's gRPC connection goes to ONE CP (say CP-1). CP-1 does a Redis blocking pop (`BRPOP`) on the queue for this runner_kind. This blocks — CP-1 is waiting on Redis. Meanwhile, a client creates a run via CP-2. CP-2 pushes the job to the same Redis queue. Redis instantly unblocks CP-1's blocking pop. CP-1 now has the assignment and responds to the runner. The runner never knew that the job was created on a different CP. It just waited, and CP-1 eventually handed it work.

If multiple runners are long-polling (each on a different CP), all those CPs have blocking pops on the same Redis queue. When a job appears, Redis gives it to exactly ONE of them (whichever pop was registered first). The others keep waiting. Redis is the single arbiter of who gets the next job.

### RPC 2: StreamEvents

While executing, the runner calls this to send progress events back to the control plane. These events include tokens being generated, intermediate state updates, tool call results, and anything else the client should see in real-time. The control plane receives each event and publishes it to Redis so that any client SSE connection (on any control plane replica) can deliver it to the browser.

This is a client-streaming RPC: the runner opens one call and sends many messages over time, then closes when done. The control plane acknowledges.

### RPC 3: ReportStatus

When the agent finishes (successfully, with an error, or interrupted), the runner calls this to report the final outcome. The control plane updates the database (Postgres) with the final status, releases the thread for new work, and cleans up.

### RPC 4: Heartbeat

Every two seconds while working on a job, the runner calls this to say "I am still alive and working." This is how the control plane detects crashes. If heartbeats stop arriving for about six seconds, the control plane assumes the runner is dead and reassigns the work to another runner. The heartbeat response also tells the runner if it has been "superseded" (replaced by another runner due to a crash recovery event) — in which case the runner should stop immediately.

### RPC 5: WatchCancels

The runner opens this streaming connection to listen for cancel signals. When a client or admin cancels a run, the signal flows through Redis Pub/Sub to the control plane, and then to the runner through this stream. The runner then cooperatively stops its execution and reports status as "interrupted."

### Why only five RPCs are enough

Everything else a runner might need (connector access, storage, vector queries, agent-to-agent delegation) happens over regular HTTP calls from the runner back to the control plane's `/internal/*` paths. These are simple request-response operations that do not need the streaming or long-polling capabilities of gRPC. Using HTTP for these keeps the gRPC protocol minimal and makes it easier to implement runners in new languages.

### The `/internal/*` HTTP APIs — what runners call DURING execution

While the five gRPC RPCs handle job dispatch and lifecycle, runners also make plain HTTP calls to the control plane when they need resources during execution. Think of it as: **gRPC is how the runner gets its JOB. HTTP is how the runner gets RESOURCES to do that job.**

Here is every `/internal/*` endpoint available today:

**Connectors (tool access via MCP proxy):**

- `POST /internal/connectors/{name}/session` — mint a short-lived session token
- `POST /internal/connectors/{name}/mcp` — proxy an MCP tool call to the upstream (policy Decide runs here)
- `GET /internal/connectors` — list available connectors
- `GET /internal/connectors/{name}` — get info about a specific connector

**Store (key-value persistence for agents):**

- `PUT /internal/store/items` — save a key-value item
- `GET /internal/store/items` — get an item by key
- `DELETE /internal/store/items` — delete an item
- `POST /internal/store/items/search` — search items by prefix/namespace
- `POST /internal/store/namespaces` — list namespaces

**Vectors (semantic search for RAG):**

- `PUT /internal/vectors/items` — insert/update a vector document with embedding
- `DELETE /internal/vectors/items` — delete a vector document
- `POST /internal/vectors/search` — similarity search by embedding vector

**Agent-to-Agent delegation:**

- `POST /internal/a2a/runs` — create a child run (one agent delegates to another)

**Other:**

- `GET /internal/runs/{runID}/status` — check a run's current status
- `PUT /internal/agents/{agentID}/schema` — report the agent's input/output JSON schema

All of these use run-binding: the CP validates the runner token, looks up the inflight assignment in Redis, and derives the tenant from the assignment rather than trusting any header the runner sends.

### Why these are NOT part of the Runner Protocol

This is an important architectural distinction. The Runner Protocol (`proto/runner.proto`) is the **universal standard** — any control plane could implement it, and any runner could speak it. It covers the fundamental mechanics of distributed execution: get work, stream results, report status, prove liveness, receive cancellation.

The `/internal/*` HTTP APIs are **Runkite platform services** — features specific to this control plane implementation. They are convenience services that Runkite provides, not something every control plane must offer.

If you released the Runner Protocol as a public standard for the industry:

- The five gRPC RPCs would be the spec. Any runtime (Runkite, LangGraph Platform, a custom system) could implement them.
- The `/internal/*` APIs would NOT be in that spec. They are Runkite's "extras." A different control plane might offer vector search differently, or not at all, or through a completely different mechanism.

A runner built only against the five RPCs (no `/internal/*` calls) works perfectly fine — it just cannot use connectors, store, or vectors through the CP proxy. It would access those directly (direct mode) or not at all. The Runner Protocol does not require any HTTP calls.

This separation means: the PROTOCOL is portable. The PLATFORM SERVICES are Runkite-specific value-adds.

### The mental model

Think of it this way:

- **Agent Protocol** is the "customer-facing counter" — polite, standardized, speaks in terms of conversations and messages
- **Runner Protocol** is the "back-of-house radio" — efficient, focused on work dispatch and liveness, speaks in terms of jobs, generations, and heartbeats

The control plane is the translator between these two languages. It receives a customer request ("start a conversation with my research agent") and translates it into a work dispatch ("here is a job assignment with generation 1 for runner_kind python-langgraph"). It receives worker events ("here are some tokens") and translates them into customer events ("here is your agent's response streaming").

---



# 4. A Complete Run From Start to Finish

Let me walk through exactly what happens when someone creates a run, step by step, naming every process, every database write, and every Redis operation. This is the core mental model for the entire system.

## Setup

Assume we have:

- One control plane process (we will add multiple later)
- Postgres as the database
- Redis as the queue and event system
- One Python runner connected via gRPC
- A client application that wants to use an agent called `research_bot`



## Step 1: Client sends the request

The client application sends:

```
POST /threads/thr_42/runs
Authorization: Bearer <token>
Content-Type: application/json

{"agent_id": "research_bot", "input": {"messages": [{"role": "human", "content": "What is quantum computing?"}]}}
```

This says: "On thread thr_42, start a new run of agent research_bot with this input."

## Step 2: Authentication

The control plane's HTTP middleware intercepts the request before it reaches the handler. It validates the bearer token (could be a JWT, an API key, or a custom auth webhook). From the token, it extracts: who is making the request (the "principal"), which tenant they belong to (for multi-tenancy), and what permissions they have (read, write, admin). If auth fails, the client gets a 401 and nothing else happens.

**Code:** `internal/auth/` handles all authentication.

## Step 3: The createRunCtx funnel

Every single run creation in the system — whether from HTTP, WebSocket, cron, or agent-to-agent — goes through ONE function called `createRunCtx` in `internal/api/runs.go`. This is the central funnel. Understanding this function means understanding run creation.

Here is what it does, in order:

**3a. Alias resolution.** If `research_bot` is actually an alias that routes 90% to `research_bot_v1` and 10% to `research_bot_v2` (for A/B testing), resolve it now. Store the original alias name in metadata for analytics. After this step, we have the real agent ID.

**3b. Load the agent.** Query the database: does an agent with this ID exist for this tenant? If not, return a 404 error. If yes, load its configuration (what runner_kind it needs, what connectors it uses, what stream modes it supports). Note: "query the database" does NOT mean hitting the database on every single request. The CP keeps an in-memory cache of recently-loaded agent configs with a short TTL (seconds). If the same agent was loaded 2 seconds ago, the cached version is returned instantly. If the cache entry expired or doesn't exist, it queries the DB once and caches the result. This is important because agent configs change rarely (maybe a few times per day by an admin), but requests arrive thousands of times per second. The short TTL ensures config changes propagate within seconds while eliminating redundant DB queries.

**3c. Rate limiting.** Check if this tenant/agent/user has exceeded their request rate limit. If using Redis-backed rate limits (production), this decrements a shared counter in Redis. If the limit is exceeded, return 429 "Too Many Requests."

**3d. Admission gates.** Run through several checks:

- Is there a kill switch active for this tenant or agent? If yes, refuse with 403.
- Does the principal have permission to run this specific agent? (The `agents:<id>:run` permission check.) If not, refuse with 403.
- If policy is configured, does the policy allow creating a run for this agent? (Stage: `run.create`.) If the policy webhook returns deny, refuse with 403.

**3e. Ensure the thread exists.** If the thread ID provided does not exist in Postgres, either create it (if the request allows implicit creation) or return a 404.

**3f. Cache check.** If the exact same request (same input, same agent, same config) was recently completed and caching is enabled, return the cached result immediately. No runner needed. The assignment returned will be nil, signaling "cache hit, no dispatch needed."

**3g. Claim the thread.** This is critical. A thread can only have ONE active run at a time. To enforce this, the control plane does an atomic conditional database update: "SET thread status to 'busy' WHERE thread status is currently 'idle'." If this succeeds, we own the thread. If it fails (because the thread is already busy with another run), return a 409 Conflict error. This prevents two runs from trampling each other on the same conversation.

**3h. A2A checks (if this is agent-to-agent).** This step only fires when one agent is calling another agent (not when a human client creates a run). How does the CP know the difference? It's structural — there are two separate endpoints:

- Human clients hit `POST /threads/{id}/runs` (public Agent Protocol). The `ParentRunID` field is NEVER populated from a client request body.
- Agents call `POST /internal/a2a/runs` (internal-only, runner-authenticated). This endpoint REQUIRES `parent_run_id` in the body.

So the check is simply: `if ParentRunID != nil` → this is agent-to-agent. A human cannot fake this because the public endpoint ignores that field entirely.

When it IS agent-to-agent, two safety checks run:

**Depth limit (prevents infinite recursion):** Every run has a `Depth` integer stored in the database. Human-initiated runs have `Depth = 0`. When an A2A run is created, the CP loads the parent run, reads its depth, and sets `child.Depth = parent.Depth + 1`. If this exceeds `a2a.max_depth` (default: 10), reject with 400. So even if Agent A calls B calls C calls A in a cycle, it can only go 10 levels before the CP refuses — killing the chain at creation time, not after consuming resources.

**Breadth limit (prevents fork bombs):** Before creating the child, the CP queries "how many children does this parent already have?" If the count is at or above `a2a.max_breadth` (default: 20), reject with 400. So one parent cannot spawn hundreds of children simultaneously.

Combined: the tree is bounded. Max 10 deep, max 20 wide per node. And if you cancel any run, the cancel cascades to ALL its descendants (children, grandchildren, etc.) via a single DB query for the whole tree — so cancelled parents cannot leave orphaned children still running.

**Code:** `internal/api/a2a.go` (the `/internal/a2a/runs` handler), depth/breadth enforcement in `internal/api/runs.go` (inside `createRunCtx`), defaults in `internal/api/server.go` (`defaultA2AMaxDepth = 10`, `defaultA2AMaxBreadth = 20`).

**3i. Create the run row.** Insert a new row into Postgres: `run_id`, `thread_id`, `agent_id`, `tenant_id`, `status = 'pending'`, `input`, timestamps. For admission-limited tenants (concurrent/daily caps), this insert happens inside a database lock to prevent the count-then-insert race condition where two simultaneous requests both see "9 out of 10 slots used" and both insert, ending up at 11.

**3j. Build the assignment.** Construct a JSON object that contains everything the runner will need: the run_id, thread_id, agent_id (after alias resolution), the input, configuration, what connectors the agent needs, the user context (who requested this), stream modes, and critically: `generation: 1`. Generation is a counter that starts at 1 for the first attempt. If the runner crashes and the job is reassigned, generation becomes 2, then 3, etc. This is the fencing token that prevents stale runners from corrupting results (explained fully in the Crash Recovery chapter).

**3k. Return.** createRunCtx returns the created run object and the assignment to the caller.

**Code:** `internal/api/runs.go`, function `createRunCtx`.

## Step 4: Enqueue to Redis (and the three handler styles)

After createRunCtx returns, the HTTP handler takes the assignment and pushes it into the Redis job queue. The queue is partitioned by `runner_kind` — so the assignment goes into the queue for `python-langgraph` runners specifically.

**Only one handler fires per request.** The client chooses which handler by choosing which endpoint to call:

- `POST /threads/{id}/runs` → background handler
- `POST /threads/{id}/runs/stream` → stream handler
- `POST /threads/{id}/runs/wait` → wait handler

You never use stream and wait simultaneously for the same run. They are two different delivery styles for the same operation. The runner does identical work regardless — the difference is purely in how the CP delivers results back to the client.

**What each handler does after createRunCtx returns:**

**Background create:** Enqueue assignment to Redis → immediately return the run object as JSON → close the HTTP connection. Client gets `{"run_id": "...", "status": "pending"}` and is done. If they want results later, they poll `GET /runs/{id}` or open a separate SSE stream.

**Stream create (ChatGPT-style, tokens appear word by word):**
1. Subscribe to the internal event broker FIRST (before any runner starts)
2. Enqueue assignment to Redis (now a runner can pick it up)
3. Keep the HTTP connection open as SSE — forward every event to the client as it arrives
4. Close the connection when the run terminates

**Wait create (one-shot API-style, complete answer in one response):**
1. Subscribe to the internal event broker FIRST
2. Enqueue assignment to Redis
3. Block silently — consume events internally, discarding intermediate tokens
4. When the terminal event arrives (success/error), write ONE JSON response with the final result
5. Close the connection

**Why subscribe BEFORE enqueuing?** Order matters. If you enqueue first, the runner could start producing events before the CP subscribes — you'd miss the first few tokens. By subscribing first, you guarantee that no event can arrive before you're listening.

**What is "subscribe" subscribing to?** It is the CP subscribing to its OWN internal event broker (not a client-side thing, not Redis directly). The broker is an in-memory pub/sub system inside the CP that receives events from runners and distributes them to waiting handlers. The full data flow:

```
Runner (produces tokens) —gRPC StreamEvents→ CP (publisher side) —broker→ CP (subscriber side) —SSE→ Client
```

In single-replica mode, the publisher and subscriber are the same CP process. In multi-replica mode, the runner's gRPC might be connected to CP2, but the client's SSE is on CP1. Redis pub/sub bridges them: CP2 publishes to Redis, CP1's subscriber picks it up from Redis and forwards to the client. The handler code is identical either way — the broker abstraction hides whether events came locally or via Redis.

If enqueue FAILS (Redis is down), the handler calls `rollbackCreatedRun` which marks the run as errored in Postgres and releases the thread back to idle. Without this rollback, a Redis failure would leave a thread permanently stuck in "busy" with a run that will never execute.

**Code:** `internal/transport/redis/` (queue), `internal/api/runs.go` (all three handler functions), `internal/broker/` (the internal event broker).

## Step 4b: How events flow from runner back to client (the broker explained)

The "broker" is not Kafka. It is not a durable message store. It is a real-time broadcast system — like a loudspeaker. If you are listening, you hear the message. If you are not listening yet, the message is gone forever.

**Single replica (everything in one process):**

```
┌─────────────────────────────── CP1 ──────────────────────────────┐
│                                                                   │
│  gRPC handler ───► IN-MEMORY BROKER (Go channels) ───► SSE handler│
│  (receives from       (routes by run_id)             (sends to    │
│   runner)                                             client)     │
└───────────────────────────────────────────────────────────────────┘
       ▲                                                    │
       │ gRPC                                               │ SSE
   [RUNNER]                                             [CLIENT]
```

Events flow: Runner → gRPC → CP memory → SSE → Client. No Redis involved for events. The broker is just an in-memory map of `run_id → list of subscribers (Go channels)`.

**Multi-replica (Redis pub/sub bridges the CPs):**

```
[RUNNER]                                                [CLIENT]
   │ gRPC                                               ▲ SSE
   ▼                                                    │
┌────────────┐         ┌───────────┐         ┌────────────┐
│    CP2     │         │   REDIS   │         │    CP1     │
│            │         │  pub/sub  │         │            │
│ gRPC recv  │──PUBLISH─►channel   │──DELIVER─►subscriber │
│            │         │  "run:X"  │         │ → SSE send │
└────────────┘         └───────────┘         └────────────┘
```

Timeline:
1. Client calls `/runs/stream` → load balancer routes to CP1
2. CP1 subscribes to Redis pub/sub channel `run:X` (now listening)
3. CP1 enqueues assignment to Redis job queue
4. Runner calls `GetJob` → load balancer routes gRPC to CP2
5. Runner produces token "Hello" → sends via gRPC → arrives at CP2
6. CP2 publishes "Hello" to Redis pub/sub channel `run:X`
7. Redis broadcasts to all subscribers → CP1 receives it
8. CP1 forwards "Hello" to client via SSE

**Who decides which CP does what?** The load balancer, at connection time. Not the CPs negotiating. The rule is deterministic:
- Whichever CP the client's HTTP connection landed on → that CP subscribes (it needs to deliver events)
- Whichever CP the runner's gRPC connection landed on → that CP publishes (it receives events from the runner)

There is no conflict because Redis pub/sub is broadcast — if multiple CPs subscribe to channel `run:X`, ALL receive the message. But typically only one CP has a client waiting for that specific run, so only one subscribes.

**Correction: events use Redis Streams, NOT Redis pub/sub.**

Redis pub/sub (fire-and-forget broadcast) is used for cancel signals only. Event delivery uses Redis Streams (XADD/XREAD), which are a persistent, replayable log — more like a lightweight Kafka topic with a TTL.

**Where things live (summary):**
- Run record (metadata, status) → Postgres (permanent)
- Job queue (work waiting for runners) → Redis LIST (consumed once, then gone)
- Events during streaming → Redis Streams (persistent for 24h, replayable)
- Cancel signals → Redis pub/sub (ephemeral, fire and forget)

## Step 4b-extra: What exactly IS the "broker"? (Not Kafka — much simpler)

The word "broker" does NOT mean Kafka or any external messaging system. It is a Go INTERFACE defined inside the control plane:

```go
type EventBroker interface {
    Publish(ctx, runID, event)             // put an event for run_abc
    Subscribe(ctx, runID) → Go channel     // watch for events on run_abc
    Replay(ctx, runID, sinceSeq)           // read all past events
    Close(runID)                           // done, close the stream
}
```

It has TWO implementations. The code that calls `Publish` or `Subscribe` doesn't know which one is running — same interface, different backends:

**Implementation 1: In-process (single replica, dev mode, no Redis needed for events)**

Just a Go map in RAM:

```go
type Broker struct {
    subscribers map[string][]chan *RunEvent  // run_id → list of Go channels
}
```

`Publish("run_abc", event)` → loops through `subscribers["run_abc"]`, sends event to each Go channel.
`Subscribe("run_abc")` → creates a Go channel, adds it to `subscribers["run_abc"]`, returns the channel.

No network. No external system. The gRPC handler calls Publish inside the same process, the SSE handler calls Subscribe inside the same process, and Go channels connect them.

**Implementation 2: Redis Streams (multi-replica, production)**

```go
Publish("run_abc", event) → XADD rk:events:run_abc {data: event_json}
Subscribe("run_abc")      → spawns goroutine doing XREAD (blocking tail) on rk:events:run_abc
```

XADD appends the event to a Redis Stream (a persistent, ordered log). XREAD blocks until new entries appear, then delivers them. Unlike pub/sub, Streams store entries (with a 24h TTL), so Replay works (you can read from any offset).

**How subscribe-before-enqueue works in actual code:**

```go
// Step 1: Subscribe (creates a Go channel listening to Redis Stream)
eventCh, err := s.broker.Subscribe(ctx, run.RunID)
// → Redis: starts XREAD goroutine on rk:events:run_abc from current tail position

// Step 2: Enqueue (NOW runners can pick up the job)
s.enqueue(ctx, assignment)
// → Redis: LPUSH rk:queue:python-langgraph

// Step 3: Stream to client (read from the Go channel, write SSE)
for event := range eventCh {
    fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Method, event.Data)
}
```

The `eventCh` is a plain Go channel. Subscribe creates it and returns it. A background goroutine (started by Subscribe) reads from Redis via XREAD and pushes events into this channel. The SSE handler just reads from the channel — it has no idea Redis is involved.

**Code:** `internal/transport/types.go` (EventBroker interface), `internal/transport/inprocess/inprocess.go` (in-memory implementation), `internal/transport/redis/redis.go` (Redis Streams implementation).

## Step 4b-walkthrough: Complete end-to-end flow (multi-replica, one token from client to client)

This traces ONE complete request through a 3-CP multi-replica deployment, showing exactly which CP does what and where events flow.

**Step A: Client sends request → LB picks CP1**

```
Client: POST /threads/thr_42/runs/stream  ──HTTP──► LB ──► CP1
```

LB picks CP1 randomly. CP1 handles this for the entire request lifetime.

**Step B: CP1 creates the run**

CP1 runs `createRunCtx()`: validates agent, rate-limits, claims thread in Postgres, inserts run row (status=pending), builds the assignment JSON. Returns two objects: `run` (DB record) and `assignment` (job packet).

**Step C: CP1 subscribes to event broker THEN enqueues**

```
CP1: broker.Subscribe("run_abc")
  → Redis: starts XREAD goroutine polling rk:events:run_abc
  → Returns: eventCh (Go channel, empty, waiting)

CP1: enqueue(assignment)
  → Redis: LPUSH rk:queue:python-langgraph {assignment_json}

CP1: enters SSE loop:
  for event := range eventCh {   ← BLOCKS here, waiting
      write SSE to client
  }
```

State: CP1 has a goroutine doing XREAD on Redis, waiting. No other CP knows about this run yet.

**Step D: Runner asks for work → LB picks CP2**

```
Runner-1: GetJob("python-langgraph") ──gRPC──► LB ──► CP2
```

CP2 runs BLMOVE on Redis, claims the job atomically. CP2 also subscribes to `rk:cancel:run_abc` (Redis pub/sub, for cancel signals only). CP2 returns assignment to Runner-1.

Important: LB sends GetJob to ONE CP, not all. CP2 wins because it's the one that called BLMOVE first.

**Step E: Runner produces token "Hello"**

```
Runner-1: StreamEvents(event={run_id:"run_abc", data:"Hello"})
  ──gRPC──► CP2 (same channel as GetJob)
```

**Step F: CP2 writes to Redis Stream**

```
CP2: broker.Publish("run_abc", event)
  → Redis: XADD rk:events:run_abc * data:"Hello"
  
  CP2 is DONE. It doesn't know about CP1 or the client.
```

**Step G: CP1's XREAD goroutine picks up the event**

```
CP1: (XREAD goroutine from Step C)
  Redis returns new entry → goroutine sends to eventCh

CP1: (SSE loop from Step C)
  event := <-eventCh   ← receives "Hello"
  fmt.Fprintf(w, "event: values\ndata: Hello\n\n")
  flusher.Flush() → bytes flow to client
```

**Step H: Client receives the token**

```
Client: SSE frame arrives:
  event: values
  data: Hello
```

**The complete chain for one token:**

```
LLM → Runner-1 → gRPC → CP2 → XADD Redis Stream → XREAD CP1 goroutine → Go channel → SSE → Client
```

**Steps E-H repeat for every token.** When the run finishes, Runner-1 calls `ReportStatus("success")` to CP2. CP2 Acks the inflight entry, updates Postgres (status=success, release thread). The final "end" event flows through the same Redis Stream → CP1 → client path, and CP1 closes the SSE connection.

**Who does what (summary):**

| Component | Does |
|-----------|------|
| CP1 (client-side) | Creates run, starts XREAD goroutine, holds SSE open, forwards events to client |
| CP2 (runner-side) | Dequeues job, receives gRPC from runner, writes XADD to Redis Stream, handles ReportStatus |
| Redis Stream | The ONLY bridge between CP1 and CP2 — they never talk directly |
| Runner-1 | Executes agent, sends tokens via gRPC to CP2, heartbeats to CP2 |
| Postgres | Stores durable run record and thread ownership |

CP1 and CP2 never communicate with each other. The Redis Stream is the mailbox: CP2 drops letters in, CP1 picks them up.

## Step 4b-persistence: When does Postgres get updated? (The "before and after photo" vs "live video")

A common question: "If tokens flow through Redis Streams, when and how does Postgres know what's happening?"

The answer: **Postgres is NOT updated on every token.** It records the bookends (start and finish), not the live stream. Think of it as:

- **Redis Stream** = the live video feed (every token, real-time, ephemeral, expires in 24h)
- **Postgres** = the before-and-after photo (status changes, final output, permanent)

**The exact moments Postgres is written to during a run's lifetime:**

```
t=0s:    createRunCtx
         Postgres: INSERT INTO runs (run_id='run_abc', status='pending', ...)
         Postgres: UPDATE threads SET status='busy' WHERE thread_id='thr_42'
         → Admin UI can query: "show me all pending runs"

t=0.1s:  Runner picks up job via GetJob
         Postgres: nothing changes.
         (Redis inflight tracking is updated, but Postgres still says "pending")

t=0.3s:  First StreamEvents message arrives at CP2
         Postgres: UPDATE runs SET status='running' WHERE run_id='run_abc'
         → Admin UI can query: "show me all running runs"

t=0.5s:  Tokens "Hello", "world", "!" flow through
         Postgres: NOTHING CHANGES. Still just status='running'.
         Redis Stream: tokens written via XADD, read via XREAD by CP1, sent to client via SSE.
         These tokens are NOT in Postgres. They exist only in Redis (for 24h) and in the client's SSE stream.

t=5s:    Run completes. ReportStatus("success") arrives at CP2.
         StatusCallback fires — this is where the burst of Postgres writes happens:

         1. broker.Replay("run_abc", 0)
            → Read ALL events back from Redis Stream
            → Find the last "values" event (the final output)

         2. UPDATE threads SET values = '{"messages":[...final output...]}' WHERE thread_id='thr_42'
            → Thread now carries the final conversation state

         3. Save checkpoint (for multi-turn conversations)

         4. UPDATE runs SET status='success', updated_at=NOW() WHERE run_id='run_abc'
            → Run is now permanently marked as successful

         5. UPDATE threads SET status='idle' WHERE thread_id='thr_42'
            → Thread is free for the next run

         6. Cache result (if caching enabled, for identical future queries)

         → Admin UI can query: "show me completed runs" + see final output
```

**What the Admin UI can see at each stage:**

| Stage | Admin UI shows | Source |
|-------|---------------|--------|
| Run created | run_abc: pending | Postgres |
| Runner executing | run_abc: running | Postgres |
| Tokens streaming | run_abc: running (no token detail) | Postgres (unchanged) |
| Run complete | run_abc: success + final output | Postgres |

The Admin UI queries Postgres. It can see WHICH runs exist and their status, but it cannot see individual tokens mid-stream. The live token stream goes exclusively through: Redis Stream → CP1 → SSE → client. The Admin UI is a dashboard, not a live token viewer.

**Why not write every token to Postgres?**

Performance. An agent might produce 500 tokens in 5 seconds. Writing each one to Postgres would be 100 writes/second per run. With 100 concurrent runs, that's 10,000 writes/second just for streaming data that nobody queries from the database. Redis Streams handle this volume trivially (it's in-memory). Postgres is reserved for durable state that needs to survive Redis restarts.

**What happens if Redis dies mid-stream?**

The live SSE stream breaks (XREAD fails, no more events). The client's connection errors out. BUT: the run row in Postgres still says `status='running'`. The runner is still executing (it talks to the CP via gRPC, not Redis). When the runner finishes and calls ReportStatus, Postgres is updated to `success`. The run's OUTPUT is saved. The client missed the live stream but can poll `GET /runs/run_abc` later and get the complete result.

Redis is the fast path (live streaming). Postgres is the durable path (final state). Losing Redis degrades the UX (no live tokens) but doesn't lose data (final output still lands in Postgres).

**Code:** `internal/api/server.go:StatusCallback` (the burst of Postgres writes on completion), `internal/bridge/server.go:StreamEvents` (first-event → status=running), `internal/transport/redis/redis.go:Publish/Subscribe` (Redis Stream operations).

## Step 4b-scalability: The goroutine-per-run model — where it works and where it hits a wall

**How Subscribe works under the hood:**

Every `broker.Subscribe("run_abc")` spawns ONE goroutine that runs a blocking `XREAD` loop against Redis. If a CP is serving 500 concurrent SSE streams (500 clients watching 500 runs), that CP has 500 goroutines each holding a Redis connection, doing:

```
goroutine 1:  XREAD BLOCK 1000 rk:events:run_001 <last_id>
goroutine 2:  XREAD BLOCK 1000 rk:events:run_002 <last_id>
goroutine 3:  XREAD BLOCK 1000 rk:events:run_003 <last_id>
...
goroutine 500: XREAD BLOCK 1000 rk:events:run_500 <last_id>
```

**Why this is fine at moderate scale:**

Go goroutines are NOT OS threads. Each one costs ~2-4 KB of stack memory. 500 goroutines = ~1-2 MB of RAM. Even 10,000 goroutines = ~20-40 MB. Go's runtime multiplexes them onto a handful of real OS threads. This is Go's core design advantage — goroutines are cheap, spawn as many as you need.

For comparison: in Java or Python, each waiter would need an OS thread (~1-8 MB stack). 10,000 threads = 10-80 GB + scheduling overhead. That would be a real problem. In Go, it is not.

**Where it hits a wall (~5,000-10,000 concurrent SSE connections per CP):**

The goroutines themselves are cheap. The BOTTLENECK is Redis connections. Each `XREAD` blocks on a Redis connection from go-redis's connection pool. 10,000 concurrent XREAD calls need 10,000 pool connections. Redis's default `maxclients` is 10,000. At that scale, one CP would consume the entire Redis connection budget, leaving nothing for queues, heartbeats, cancels, or other CPs.

With 3 CPs behind a load balancer, the per-CP ceiling is ~3,333 concurrent SSE streams before connection pressure becomes real. Below that: no problem. Above that: optimization needed.

**The optimization (not implemented — demand-gated):**

Replace goroutine-per-run with a multiplexed reader. Redis's XREAD supports tailing MULTIPLE streams in ONE command:

```
Current (goroutine per run):
  10,000 goroutines × 1 XREAD each = 10,000 Redis connections

Optimized (multiplexed reader):
  1 goroutine: XREAD BLOCK 1000 STREAMS rk:events:run_001 rk:events:run_002 ... <id_1> <id_2> ...
  = 1 Redis connection for ALL streams
  + in-memory map[run_id] → Go channel for dispatching events to the right SSE handler
```

One XREAD call tails all active streams. When any stream has a new event, Redis returns it. The goroutine looks up the run_id in its map and sends the event to the correct Go channel. The SSE handler code doesn't change at all — it still reads from a Go channel.

**Why this isn't built today:**

1. The product targets hundreds to low thousands of concurrent runs — well within the current model
2. The multiplexed reader is significantly more complex (dynamic subscribe/unsubscribe as runs start/finish, managing the growing/shrinking XREAD argument list, handling connection failures for the single shared reader)
3. Premature optimization adds code complexity for a scale problem nobody has reported
4. When the scale IS needed, it's a drop-in replacement behind the same EventBroker interface — the API, SSE handlers, and bridge code don't change

**Known limitation (documented here for future reference):**

Per-CP ceiling of ~3,000-5,000 concurrent SSE connections before Redis connection pool pressure. Mitigation: add more CP replicas (horizontal scaling) or implement the multiplexed XREAD reader (vertical optimization). The multiplexed reader would raise the ceiling to ~50,000+ concurrent streams per CP (limited by memory and SSE write throughput, not Redis connections).

## Step 4c: The load balancer topology (both sides, not just client-side)

A common misconception: the load balancer only sits between clients and CPs. In reality, in multi-replica deployments, the load balancer sits between BOTH sides:

```
                    ┌──────────────────┐
  Client ──HTTP──► │  LOAD BALANCER   │ ◄──gRPC── Runner
                    │  (nginx)         │
                    │                  │
                    │  port 2026: HTTP │
                    │  port 50051: gRPC│
                    └────────┬─────────┘
                             │
              ┌──────────────┼──────────────┐
              ▼              ▼              ▼
           [CP1]          [CP2]          [CP3]
              │              │              │
              └──────────────┼──────────────┘
                             │
                    ┌────────▼────────┐
                    │  Postgres + Redis│
                    └─────────────────┘
```

Runners do NOT connect directly to a specific CP. They connect to `lb:50051`, and nginx round-robins their gRPC calls across CPs. The runner's config is simply `RUNKITE_GRPC_URL: lb:50051` — it has no idea which CP it's talking to.

**gRPC connection persistence:** gRPC uses HTTP/2, which multiplexes many RPCs over one persistent TCP connection. Once a runner opens a channel to the LB and gets routed to (say) CP2, subsequent RPCs over that same channel (`StreamEvents`, `Heartbeat`) go to CP2. The runner doesn't reconnect per-RPC — it holds the channel open. If the connection drops and the runner reconnects, the LB MAY route to a different CP next time.

**Why this works despite different CPs:** Because Redis bridges everything. The runner sends events to whichever CP its gRPC landed on. That CP publishes to Redis. The CP holding the client's SSE picks events up from Redis. Nobody needs to know who is on the other side.

**In single-instance mode (dev/testing):** No load balancer exists. The runner connects directly to `localhost:50051`. There's only one CP, so gRPC, HTTP, events — all go through the same process. No Redis pub/sub needed for events (in-memory broker suffices).

**In multi-replica mode (production):** Both `docker-compose.multi.yml` and the Helm chart deploy nginx (or an ingress controller) in front of all CPs, exposing both port 2026 (HTTP) and port 50051 (gRPC) through the same load balancer.

**Code:** `deploy/nginx-multi.conf` (the nginx config with `upstream runkite_grpc` and `grpc_pass`), `docker-compose.multi.yml` (runners pointing at `lb:50051`).

## Step 5: The runner picks up the work

The runner has been sitting in a `GetJob` call, waiting for work. The control plane's gRPC bridge receives the GetJob request and calls Redis `Dequeue`.

**How Dequeue is atomic (no two runners get the same job):**

Redis's `BLMOVE` command does this in one atomic step:

```
BLMOVE  rk:queue:python-langgraph  →  rk:inflight:pending  (RIGHT → LEFT)
```

It pops the job from the ready queue AND pushes it to the pending list as a SINGLE operation. Redis is single-threaded for command execution — while this runs, nothing else can touch the queue. If two CPs call `BLMOVE` simultaneously, Redis serializes them: one gets the job, the other waits for the next one.

After `BLMOVE`, a second step (`promoteScript`, a Lua script) moves the job from pending into durable inflight tracking (a hash + sorted set). If the CP crashes between `BLMOVE` and `promoteScript`, the job sits on `rk:inflight:pending` until the next reaper tick (~2s) picks it up and promotes it. No job is ever "lost in the void."

**Can a job be executed twice?** Not with both accepted. If a runner crashes, reclaim re-enqueues the job with `generation + 1`. A new runner gets it. If the original runner somehow revives and finishes, its `ReportStatus(generation: 1)` is rejected because the inflight record now shows `generation: 2`. This is fencing — stale runners cannot overwrite current results.

**Before returning the job, the CP subscribes to the cancel channel:**

This happens SYNCHRONOUSLY inside GetJob (before the response is sent). The CP calls `SubscribeCancel(run_id)` which subscribes to Redis pub/sub channel `rk:cancel:{run_id}`. This must happen before returning because Redis pub/sub has no replay — a cancel published before subscription is lost forever.

**Code:** `internal/bridge/server.go` (GetJob handler), `internal/transport/redis/redis.go` (Dequeue with BLMOVE + promoteScript).

## Step 5b: How cancel reaches a running agent (not polling — streaming)

Cancel is NOT periodic checking. The runner does NOT ask "am I cancelled?" on a timer. It uses a permanent streaming connection that delivers cancel signals instantly.

**The WatchCancels stream (established once at runner startup):**

When the runner boots, it opens a `WatchCancels` gRPC call — a server-streaming RPC that stays open for the entire lifetime of the runner:

```python
stream = stub.WatchCancels(WatchCancelsRequest(runner_kind="python-langgraph"))
async for signal in stream:
    # signal.run_id tells us which specific run to cancel
    cancel_events[signal.run_id].set()
```

This connection just sits there, permanently. The CP only sends a message on it when a cancel actually happens.

**The full cancel flow:**

```
1. Client: POST /runs/run_abc/cancel → arrives at CP1
2. CP1: publishes to Redis pub/sub channel "rk:cancel:run_abc"
3. CP2 (which subscribed to that channel during GetJob): receives signal
4. CP2: sends CancelSignal{run_id: "run_abc"} down the WatchCancels stream
5. Runner: receives signal → sets cancel_event for run_abc
6. execute_run: after its current chunk, checks cancel_event.is_set() → True → stops
7. Runner: calls ReportStatus(status="interrupted")
```

**How does the runner stop execution?** Cooperatively. The `cancel_event` is an `asyncio.Event` shared with `execute_run`. Inside the execution loop, after every streamed chunk (every token from the LLM), the code checks:

```python
if cancel_event is not None and cancel_event.is_set():
    await event_callback(make_event("end", {"status": "interrupted"}))
    return "interrupted"
```

This is NOT a periodic timer — it's checked at every natural yield point in the async loop (after each LLM token, after each tool result, after each graph step). So cancellation latency is: however long until the next chunk arrives (typically milliseconds during active streaming, longer if the LLM is thinking).

**The race condition that was fixed:** A cancel signal can arrive BEFORE the runner has registered `cancel_event` for that run_id (because `GetJob → parse → register` takes real time). The runner keeps a `pre_cancelled` set: if a signal arrives for a run_id not yet registered, it's remembered. When the run registers moments later, the event is created already-set. A test (`cancel_race_test.go`) explicitly verifies this.

**Code:** `internal/bridge/server.go` (WatchCancels handler + cancel subscription in GetJob), `python/runkite_runner/worker.py` (watch_cancels coroutine + cancel_event checking in execute_run).

## Step 5c: Why cancel works across CPs (the gRPC connection is implicitly sticky)

A common confusion: "The runner's WatchCancels is connected to CP2. The cancel request arrives at CP1. How does CP1 notify the runner?" The answer relies on two facts:

**Fact 1: All of a runner's RPCs go to the SAME CP.**

The runner opens ONE gRPC channel (one TCP connection to the load balancer). gRPC uses HTTP/2 multiplexing — many streams (GetJob, WatchCancels, StreamEvents, Heartbeat) share that one connection. Nginx maps one inbound connection to one backend. Result: if the LB routed this runner to CP2, then GetJob AND WatchCancels both live on CP2.

**Fact 2: The CP that dispatched the job subscribes to Redis pub/sub for it.**

When CP2 dispatches run_abc via GetJob, CP2 subscribes to `rk:cancel:run_abc`. No other CP subscribes to that channel. So only CP2 will receive a cancel signal for run_abc.

**Combined flow:**

```
Client → cancel request → (LB) → CP1
CP1: PUBLISH "rk:cancel:run_abc" to Redis pub/sub
Redis: delivers to CP2 (the only subscriber)
CP2: receives signal → notifyWatchers → sends over WatchCancels stream → Runner
```

The dispatching CP (CP2) is both the Redis subscriber AND the WatchCancels host. Redis pub/sub bridges the gap from "any CP can receive the cancel request" to "the specific CP that owns this run."

**Edge case: what if the runner reconnects to a different CP?**

If the runner's connection drops and reconnects (now on CP3): heartbeats stop → lease expires → run is reclaimed (re-enqueued with generation+1). Cancel becomes irrelevant — the original run is dead anyway. The system doesn't need cancel delivery to survive connection migration because disconnection = heartbeat loss = reclaim.

## Step 5d: The exact state transitions (what's in Postgres vs what's in Redis)

Two separate tracking systems exist, serving different purposes:

**Postgres (the run row — durable state visible to clients):**
- `pending` → set when createRunCtx inserts the run row
- `running` → set when the first StreamEvents message arrives (proves a runner is executing it)
- `success` / `error` / `interrupted` → set by ReportStatus (runner's final call)

**Redis inflight sorted set (ephemeral lease — only the reclaim system reads this):**
- Entry added during Dequeue (BLMOVE + promoteScript)
- Score = last heartbeat timestamp (refreshed every 2s by Heartbeat RPC → Renew)
- Entry removed by Ack (inside ReportStatus, after the run completes)

These are independent. Postgres tells the CLIENT what's happening. Redis tells the RECLAIM SYSTEM whether the runner is alive.

**The reclaim system does NOT know which runner has the job.** It doesn't track runner identities. It simply scans: "which inflight entries have a score (timestamp) older than the stale threshold?" If found → the runner is dead (or stuck) → re-enqueue with generation+1.

**The exact numbers:**
- Heartbeat interval: **2 seconds** (runner sends heartbeat every 2s)
- Reaper scan interval: **2 seconds** (CP checks for stale jobs every 2s)
- Stale threshold (maxAge): **6 seconds** (a job is dead if last heartbeat is >6s old)
- This means: **3 consecutive missed heartbeats** = declared dead (2s × 3 = 6s)

**Why long-running jobs (10+ minutes) are NOT killed:**

The heartbeat is a SEPARATE async task from execution. It runs in parallel, independently of whether the agent is producing output. Even if your agent calls an external MCP tool that takes 10 minutes and produces zero tokens, the heartbeat keeps firing every 2 seconds in the background:

```
t=0s:    Agent starts calling slow MCP tool...
t=2s:    Heartbeat fires → Renew → lease refreshed.  Agent still waiting for tool.
t=4s:    Heartbeat fires → Renew → lease refreshed.  Agent still waiting.
...
t=600s:  Heartbeat fires → Renew → lease refreshed.  Tool finally returns.
```

The reaper never sees a stale entry because the heartbeat resets the clock continuously. Output is irrelevant — only heartbeat matters for liveness. A job can run for hours if the runner is alive and heartbeating.

**What about the cancel subscriptions — isn't it millions of channels?**

No. Redis pub/sub channels are dynamic and ephemeral. A cancel subscription (`rk:cancel:{run_id}`) exists ONLY while the run is in-flight. When the run completes, the `watchCtx` is cancelled → `pubsub.Close()` releases the subscription → channel vanishes.

Active subscriptions at any moment = number of concurrent in-flight runs. If you have 500 runs executing simultaneously, you have 500 subscriptions. When they complete, the subscriptions disappear. Redis handles 100k+ pub/sub channels trivially.

Redis pub/sub channels are NOT like Kafka topics (permanent, requiring creation/deletion). They exist only while someone is subscribed. No cleanup needed — `Close()` makes them vanish.

**How we ensure cancel subscribe happens on the SAME CP as GetJob:** It's inside the same function call. In `bridge/server.go`'s GetJob handler, the subscribe happens BETWEEN dequeuing the job and returning it to the runner — there's no separate call that could route to a different CP. It's one atomic sequence on one CP.

**Corrected lifecycle:**

```
1. createRunCtx:   Postgres status='pending', enqueue to Redis ready queue
2. GetJob:         BLMOVE claims from ready → inflight (Redis only)
3. First event:    Postgres status='running' (proves delivery)
4. Heartbeats:     Every 2s, Renew refreshes the lease score in Redis
5a. Success:       ReportStatus → Postgres status='success', Ack removes from Redis inflight
5b. Runner crash:  Heartbeats stop → lease score ages → reclaim fires:
                   Remove from inflight, generation++ , re-enqueue to ready queue
                   (Any runner picks it up fresh via GetJob with new generation)
```

## Step 6: Runner executes the agent

The runner parses the assignment, loads the appropriate agent graph (e.g., a LangGraph StateGraph), and starts execution. This is where LLM calls happen, tools are invoked, and the agent "thinks."

During execution, two things happen in parallel:

**Heartbeat loop (per-run, NOT per-runner):** Every two seconds, the runner calls the Heartbeat RPC with the run_id and generation. Each job gets its OWN independent heartbeat task — if a runner handles 5 concurrent jobs, it has 5 heartbeat loops running in parallel, each reporting a different run_id.

Why per-run? Because the system has NO concept of "runner identity." There is no runner_id stored anywhere. The CP only cares "is run_abc alive?" not "is runner_1 alive?" This gives finer-grained liveness: even if the runner process is alive but one specific job is stuck, the per-run heartbeat for THAT job would stop while others continue — allowing the reaper to reclaim only the stuck job.

No mapping of "runner → runs" is stored anywhere. The sorted set contains ONLY `(run_id → last_heartbeat_timestamp)`. If runner_1 crashes with 3 jobs, those 3 timestamps stop getting refreshed. Runner_2 and runner_3's jobs continue heartbeating independently. After 6 seconds, only the dead runner's 3 stale entries are reclaimed.

The chain of responsibility for each heartbeat:
1. Runner sends `Heartbeat(run_id, generation)` via gRPC → arrives at CP
2. CP calls `queue.Renew(run_id, generation)` → executes a Lua script in Redis
3. Redis: `ZADD rk:inflight:zset CURRENT_TIMESTAMP run_id` (score updated)
4. Reaper (separate loop, every 2s): scans for scores older than 6s → reclaims

If the control plane responds with `superseded: true`, the runner immediately cancels its local execution (explained in Crash Recovery).

**Event streaming:** As the agent produces output (tokens, state updates, tool results), the runner sends them via the StreamEvents RPC. The control plane receives each event and publishes it to a Redis Stream keyed by the run_id. This is where fan-out happens.

## Step 7: Fan-out to the client

"Fan-out" means: one publisher, many possible readers. The control plane that received StreamEvents publishes each event to Redis once. Any control plane replica that has a client waiting for events on this run_id subscribes to that Redis Stream and forwards events to the client via SSE or WebSocket.

Think of Redis Streams like a bulletin board in a shared office. One person posts a notice (the CP that received events from the runner). Anyone walking by (any CP serving an SSE connection for this run) can read it. The poster does not need to know who is reading. The readers do not need to know who posted.

The client sees tokens appearing in their SSE stream in real-time, just as the agent generates them.

## Step 8: Run completes

When the agent finishes, the runner calls `ReportStatus` with `status: "success"` (or "error" if something went wrong) and the generation it was assigned.

The control plane receives this and does several things:

1. **Ack the inflight record** in Redis — this removes the lease tracking because the job is done.
2. **Update Postgres** — set the run status to "success" and the thread status back to "idle" (freeing it for new runs).
3. **Publish a terminal event** to the Redis Stream so SSE clients know the run is finished.
4. **Close the OTel span** for observability.
5. **Fire completion hooks** (if configured — like a webhook notification that the run finished).

The client receives the terminal SSE event, knows the run is complete, and can GET the final result.

## Step 9: Everything is cleaned up

The thread is now idle again, ready for the next run. The Redis inflight entry is gone. The run status is permanently stored in Postgres. The event stream remains in Redis for replay (if a client reconnects and needs to catch up) until it expires based on retention settings.

## What you should remember

The entire flow is: **HTTP in → Postgres write → Redis queue → gRPC out to runner → gRPC back from runner → Redis fan-out → SSE out to client → Postgres final write.**

Postgres is the source of truth for "what happened" (durable state). Redis is the coordination layer for "what is happening right now" (queues, events, leases, signals). The control plane bridges between the client world (HTTP) and the worker world (gRPC), never mixing them.

---



# 5. Multi-Replica — Running Multiple Control Planes



## Why you would run multiple control planes

In production, you do not want a single process handling all traffic. If it crashes, everything stops. You run two, three, or more control plane replicas behind a load balancer. Each replica is the same Go binary running `runkite serve`. They share one Postgres database and one Redis instance.

## How it works without breaking

The key insight: **control plane replicas are stateless workers that share a database.** They do not hold important state in memory. Any replica can handle any request for any thread or run because the truth lives in Postgres and the coordination happens in Redis.

Here is what this means concretely:

**Creating a run:** Client hits the load balancer, gets routed to CP-1. CP-1 writes to Postgres (the run) and Redis (the queue). Done. CP-2 and CP-3 never knew this happened, and they do not need to.

**Runner polling for work:** A runner's gRPC GetJob call might hit CP-2 (whichever the load balancer picks). CP-2 pops from the SAME Redis queue that CP-1 pushed to. The runner does not care which CP gave it work.

**Streaming events:** The runner's gRPC channel typically lands on the SAME CP that dispatched the job (CP-2), because it's one persistent HTTP/2 connection. CP-2 receives events via StreamEvents and writes each one to a Redis Stream via XADD (`rk:events:run_abc`). Meanwhile, the client's SSE connection is on CP-1. CP-1 has an XREAD goroutine tailing that same Redis Stream. Events flow: runner → gRPC → CP-2 → XADD Redis Stream → XREAD by CP-1 → SSE → client. Normal operation involves 2 CPs per run (one client-side, one runner-side), not 3.

**Heartbeats:** The runner sends heartbeats to whatever CP its gRPC connection goes to. That CP refreshes the lease in shared Redis. Any CP can check "is this lease stale?" when running the reclaim loop.

**Cancellation:** A client cancels via CP-1. CP-1 publishes a cancel signal to Redis Pub/Sub channel `rk:cancel:run_abc`. The CP that dispatched this run via GetJob (say CP-2) subscribed to that pub/sub channel during dispatch — it receives the signal and sends a CancelSignal over the runner's WatchCancels gRPC stream. Unlike events (where any CP can read from the Redis Stream), cancel delivery depends on the dispatching CP being alive (see Step 5c for the edge case).

## What DOES need sticky routing (memorize these)

Most things work without caring which replica you hit. But a few things require "sticky sessions" — meaning the same client must always reach the same replica:

1. **MCP sessions** (`/mcp` path): The Agent Protocol can expose agents as MCP servers. These MCP sessions store state in process memory (not Redis). If the load balancer sends the next MCP request to a different replica, the session is gone. Fix: configure your load balancer to use cookie-based sticky routing for `/mcp` paths.
2. **Admin UI sessions without Redis:** Admin login creates a cookie session. If sessions are stored in process memory (no Redis), switching replicas logs you out. Fix: either set `REDIS_URL` (sessions become shared) or sticky-route `/admin` paths.
3. **Runner gRPC connections:** A runner typically maintains one gRPC connection to one CP replica (HTTP/2 keeps the connection open). All its RPCs (GetJob, StreamEvents, Heartbeat, WatchCancels) go over that one connection to that one replica. This is natural — gRPC connections are long-lived. If the CP dies, the runner reconnects to whatever is available.



## What Redis shares across replicas

When `REDIS_URL` is set, all replicas share:

- The job queue (pending assignments waiting for runners)
- The event streams (so any replica can serve SSE for any run)
- The cancel pub/sub channels (so cancel signals reach the right place)
- Rate limit counters (so limits are cluster-wide, not per-pod)
- Inflight lease tracking (so any replica can check if a runner is still alive)
- Admin sessions (so login persists across replicas)
- The reclaim leader lock (so only one replica runs the crash recovery sweep)

Without Redis, you are running a single replica in development mode. Multi-replica without Redis is not supported for production.

## Policy overlay convergence

When an admin creates a new grant in the Admin UI, that write goes to Postgres via whichever CP handled the request. That CP's in-memory policy engine updates immediately. Other replicas learn about it by polling a fingerprint from Postgres every ~15 seconds (see `cmd/policy_overlay_poll.go`). During those 15 seconds, different replicas may give different policy answers for the same request. This is an accepted trade-off — strong consistency would require a distributed lock on every tool call, which is too expensive.

---



# 6. Thread and Run Lifecycles



## What a thread is

A thread is a container for a conversation. Think of it as a chat session. It has an ID, it belongs to a tenant, and it can hold multiple runs over time. But only ONE run can be active on a thread at a time — this is enforced by the thread's status.

## Thread statuses

A thread can be in one of these states:

- **idle** — No run is currently using this thread. It is available for a new run. This is the default state.
- **busy** — A run currently owns this thread. If someone tries to create another run on this thread, they get a 409 Conflict. The thread becomes busy when createRunCtx successfully claims it, and returns to idle when the run finishes (success, error, or interruption).
- **interrupted** — The thread is waiting for a framework-level resume. This happens when a LangGraph agent hits an `interrupt()` call and pauses, waiting for the user to provide input before continuing. The thread is not freely claimable in this state — you need to go through the resume path.

The transition is always: idle → busy (a run claims it) → idle or interrupted (the run finishes or pauses).

## What a run is

A run is a single execution of an agent on a thread. It has: an ID, a thread it belongs to, an agent it runs, an input, a status, and timing information. Over the lifetime of a conversation (thread), you might have many runs — each one is a turn in the conversation.

## Run statuses

- **pending** — The run has been created in Postgres and the job is in the Redis queue, but no runner has picked it up yet.
- **running** — A runner has taken the job and is actively executing it. (When exactly this transition fires depends on the implementation — often on first meaningful activity.)
- **success** — The agent completed normally. This is terminal.
- **error** — Something went wrong. This is terminal.
- **interrupted** — The run was cancelled, timed out, or stopped by the framework for a human-in-the-loop pause. This is terminal for this run (though a new run can resume the conversation).
- **timeout** — An optional wall-clock deadline was hit. Terminal.

Terminal means: no further status changes. Once a run is success/error/interrupted/timeout, it stays that way forever.

## Who writes these transitions

- **createRunCtx** writes: thread → busy, run → pending.
- **Runner executing** may write: run → running (when the status callback path fires).
- **Runner completing** writes: run → success or error, thread → idle. (Via ReportStatus → status callback in the control plane.)
- **Cancel** writes: run → interrupted, thread → idle.
- **Timeout** writes: run → timeout, thread → idle.
- **Rollback** (if enqueue fails): run → error, thread → idle.
- **Reclaim** does NOT write a new status — it reassigns the work. The winning runner's eventual ReportStatus writes the terminal status.



## The key invariant

At most one non-terminal run should own a thread claim at any time. If you ever see a thread stuck in "busy" with no active run, that is a bug (likely a crash during the status callback that should have released it). The fix involves checking Postgres for orphaned states.

---



# 7. Cancel — How to Stop a Running Agent



## Why cancel is hard

The client that wants to cancel sends an HTTP request to one control plane replica. The runner that needs to stop might be connected to a completely different replica via gRPC. The cancel signal must cross process boundaries without assuming the same replica handles both.

## How it works, step by step

1. **Client sends cancel.** `POST /threads/{id}/runs/{run_id}/cancel` hits some CP replica (call it CP-A). CP-A validates auth.
2. **CP-A publishes to Redis.** CP-A calls `PublishCancel(run_id)` on the CancelBroker, which writes a message to a Redis Pub/Sub channel for that run_id.
3. **CP-B receives the signal.** When the runner's GetJob was served (by CP-B), CP-B subscribed to that run's cancel channel. Now CP-B's subscription fires and it receives the cancel signal.
4. **CP-B notifies the runner.** The runner has a WatchCancels gRPC stream open to CP-B. CP-B pushes a cancel signal down that stream.
5. **Runner cooperatively stops.** The runner receives the cancel on its WatchCancels stream, cancels the local framework execution (e.g., cancels the asyncio task running the LangGraph graph), and calls ReportStatus with `status: "interrupted"`.
6. **Normal completion path.** The control plane Acks the inflight record, updates Postgres (run → interrupted, thread → idle), and publishes the terminal event to SSE.



## Critical detail: no replay

The CancelBroker is fire-and-forget with NO replay. If you publish a cancel signal and nobody is subscribed at that exact moment, the signal is lost forever. This is why the control plane subscribes to the cancel channel for a run BEFORE returning the assignment to the runner in GetJob. The subscription must exist before the runner starts, so that a cancel arriving at any moment will be caught.

## Cancel is cooperative

The runner is not forcibly killed. It receives a signal and must decide to stop. Well-behaved runners cancel their framework execution immediately. A crashed runner that never reads the cancel will eventually be caught by heartbeat timeout and reclaim. Cancel is the fast path; reclaim is the safety net.

---



# 8. Crash Recovery — What Happens When Things Die

This is one of the most important chapters because production systems crash, and how you handle crashes determines whether your system is reliable or just appears to work during demos.

## The three crash scenarios



### Scenario 1: Runner dies immediately after getting a job

A runner calls GetJob. The control plane dequeues a job from Redis into the "inflight" set (meaning "someone is working on this"). Then the runner process crashes before it even starts executing.

Without protection: the job sits in "inflight" forever. Nobody is working on it. Nobody will ever work on it. The thread is stuck busy permanently.

**How we handle it:** gRPC has built-in TCP-level keepalive pings (about every 4 seconds). When the runner's process dies, the TCP connection drops. The gRPC keepalive detects this within ~4 seconds. The GetJob handler code checks if the connection context was cancelled after Dequeue, and if so, calls `Nack` — which moves the job from "inflight" back to "ready" in Redis. Another runner can pick it up immediately.

Even if this fast-path Nack fails (race condition), the lease-based reclaim will catch it within ~6 seconds (see Scenario 2).

### Scenario 2: Runner dies mid-execution

The runner got the job, started executing, maybe even sent some events, then dies. Perhaps a container was killed (OOM), the host failed, or a network partition isolated it.

Without protection: same as above — the job sits inflight forever because nobody is refreshing the lease.

**How we handle it with heartbeats and leases:**

When a job is dequeued into the "inflight" set, Redis records the current timestamp as "last touched." Every 2 seconds, the runner calls the Heartbeat RPC, and the control plane refreshes that timestamp (this is called "Renew"). If the runner dies, heartbeats stop, and the timestamp becomes stale.

A background goroutine called the "reclaim loop" runs every 2 seconds on the control plane (specifically, on whichever replica holds the reclaim-leader lock in Redis). It scans for inflight jobs whose timestamp is older than 6 seconds (called `maxAge`). Any job that stale is assumed abandoned.

When reclaim finds a stale job, it:

1. Increments the "generation" counter for that job (from 1 to 2, or 2 to 3, etc.)
2. Puts the job back on the "ready" queue with the new generation
3. A new runner picks it up and starts fresh

The numbers are deliberate: heartbeat every 2 seconds means a healthy runner is always within 2 seconds of freshness. MaxAge of 6 seconds means you need to miss three heartbeats before being declared dead. This gives network blips a chance to recover without false alarms.

**Code:** `cmd/reclaim.go` for the reclaim loop.

### Scenario 3: The crashed runner comes back

This is the tricky one. Runner A got the job with generation 1. A network blip happens — heartbeats stop arriving. After 6 seconds, reclaim kicks in, bumps generation to 2, and Runner B picks up the job. Runner B completes successfully.

Then Runner A's network recovers. It is still running the same job with generation 1. It tries to report success, or it tries to send events, or it tries to access connectors.

Without protection: Runner A's late success report overwrites Runner B's correct success. The system has corrupted data.

**How we handle it with generation fencing:**

Every interaction between a runner and the control plane carries the generation number. When Runner A sends its next heartbeat with generation=1, the control plane checks Redis: "is generation 1 still current for this run?" It is not — generation 2 is current now. The control plane responds to Runner A's heartbeat with `superseded: true`.

When Runner A sees `superseded: true` in the heartbeat response, it immediately cancels its local execution. It knows it has been replaced. If it tries to call ReportStatus with generation=1, that will also be rejected (Ack fails for non-current generation). If it tries to send terminal events via StreamEvents, those are filtered out before being published (non-current generation terminal events are dropped).

This fencing is the critical safety mechanism: **a stale runner cannot corrupt the winner's results.**

Think of generation like a version number on a boarding pass. If your boarding pass says "version 1" but the airline reissued your seat as "version 2" to a standby passenger, you cannot board even if you show up. Your pass is invalidated. The new holder has the valid version.

## All the "heartbeat-like" timers (do not confuse them)

There are five different periodic timers in the system that people sometimes conflate. They solve different problems:

1. **Heartbeat RPC (every ~2 seconds):** The runner's application-level proof of life for a specific run. This is what reclaim watches. This carries generation and receives `superseded`. This is the most important one for reliability.
2. **Inflight lease renewal (the mechanism Heartbeat calls):** Under the hood, the Heartbeat RPC calls `queue.Renew(runID, generation)` on the transport layer. For Redis, this updates a timestamp in a hash or sorted set. It is the implementation detail of heartbeat, not a separate user-facing concept.
3. **gRPC keepalive (~4 seconds):** An HTTP/2-level TCP ping between the runner and the control plane. This does NOT check whether the runner is still working on the job — it only checks whether the network connection is alive. A runner can have a live connection but a wedged event loop that never calls Heartbeat. These are different failure modes.
4. **Connector session TTL (15 minutes):** When a runner gets a token to access an external service (like Salesforce), that token expires after 15 minutes regardless of heartbeat status. This is about credential lifetime, not run liveness.
5. **Admin session TTL (12 hours, sliding):** Browser cookies for the Admin UI. Completely unrelated to agent execution.



## The A/B sequence — memorize this

This scenario is the oral exam for crash recovery:

1. Job dispatched to Runner A, generation 1.
2. Runner A is docker-paused (simulating a crash/freeze).
3. Heartbeats stop. After ~6 seconds, reclaim fires.
4. Generation bumped to 2. Job requeued.
5. Runner B picks up generation 2. Executes. Reports success.
6. Runner A is unpaused. Its next heartbeat carries generation 1.
7. Control plane responds with `superseded: true`.
8. Runner A cancels its local work. Attempts ReportStatus with generation 1 — ignored.
9. Final status in Postgres: success (from Runner B). Thread: idle.

Runner A never corrupted anything. The system recovered automatically.

## The poison pill problem — what if reclaim loops forever?

A "poison pill" is a job that always kills its runner. Maybe the input triggers an OOM (out of memory), or the agent's code has a bug that segfaults on this specific input. No matter which runner picks it up, it dies.

What happens without protection:

```
t=0s:    Runner-A picks up job → OOM → dies
t=6s:    Reclaim fires. Generation 1→2. Job re-enqueued.
t=7s:    Runner-B picks up job → OOM → dies
t=13s:   Reclaim fires. Generation 2→3. Job re-enqueued.
...every ~7 seconds, a healthy runner dies
```

The generation counter increments (1→2→3→4→...) but has no ceiling today. The loop continues until something external stops it.

**Existing mitigations (partial):**

- **Run timeout** (`cmd/run_timeout.go`): If configured, kills the run after a wall-clock deadline (e.g., 5 minutes). But it takes that full duration before it kicks in.
- **Kubernetes CrashLoopBackOff:** Runner pods restart with increasing backoff (10s, 20s, 40s, ..., 5 min). This SLOWS the loop but doesn't stop it. Other runners still pick up the poison job.
- **Kill switch (manual):** An operator notices the alerts and kills the agent. Effective but requires human intervention.
- **Queue depth alerting:** Prometheus metrics show the queue growing. Detection only — doesn't fix the problem.

**What's missing (documented gap):** A `max_retries` generation ceiling. The reclaim code already tracks generation per run — it just needs a cap: "if generation exceeds N, mark the run as error instead of re-enqueuing." This is ~10 lines of code in the reclaim Lua script. The detailed requirement is at `plans/poison_pill_max_retries.md`.

**The bottom line:** Today, run timeout + operator alerting + kill switch cover this. A generation ceiling would make it fully automatic. Until then, always configure `run_timeout` for production agents as a safety net.

---



# 9. Agent-to-Agent Communication



## How agents talk to each other

This is simple but important: **agents never communicate directly.** There are no sockets between agent processes. There is no message bus they share. There is no peer-to-peer discovery.

When Agent A wants Agent B to do something, here is exactly what happens:

1. Agent A is running inside a runner process.
2. That runner makes an HTTP POST to the control plane: `POST /internal/a2a/runs` with body saying "create a run for agent B with this input, and my parent run is run_id X."
3. The control plane receives this request, authenticates the runner (runner token + run-binding headers), and creates a brand new run for Agent B through the same createRunCtx path that any client request would go through.
4. This child run goes into the Redis queue.
5. Some runner (maybe the same one, maybe a different one) picks up the child run via GetJob.
6. That runner executes Agent B.
7. Agent A can poll or wait for the child run to complete and use the result.

The control plane is ALWAYS the intermediary. This means:

- All governance applies to the child run (rate limits, policy, kill switches)
- The child run gets its own audit trail
- Tenancy is inherited from the parent (preventing cross-tenant forgery)
- Cost tracking can roll up the entire tree via root_run_id
- If the parent is cancelled, orphan children can be cascaded



## Depth and breadth limits

To prevent infinite recursion (Agent A calls Agent B calls Agent A calls Agent B...), there is a configurable `max_depth`. Each child run has a depth one greater than its parent. If depth exceeds the max, the create is rejected before anything is enqueued.

To prevent fork bombs (Agent A spawning 10,000 children), there is a configurable `max_breadth`. Before creating a child, the control plane counts how many existing children the parent has. If it exceeds the max, reject.

**Important honesty about breadth:** The breadth check has a race condition. Two concurrent requests from the same parent can both count "9 children" (limit 10), both pass the check, and both create — ending up at 11. This is documented in `docs/limitations.md`. The admission_limits system (tenant-wide caps) does NOT have this race because it uses a database lock around the count+insert. The A2A breadth check does not use the same lock because it operates at a different scope. This is a known, documented gap.

---



# 10. Human In The Loop — Two Completely Different Kinds

This chapter is critical. There are two types of HITL (Human In The Loop) in Runkite and they solve completely different problems with completely different mechanisms. Confusing them is the #1 mistake people make.

## Kind 1: Framework Interrupt/Resume

**What it is:** The agent's own code decides to pause and ask the user something. For example, a travel booking agent might say "I found three flights. Which one do you want?" and pause until the user responds.

**How it works:**

1. The LangGraph graph calls `interrupt("Which flight?")` (or equivalent in other frameworks).
2. The runner reports this as an interrupted state, possibly with the question/options as part of the event stream.
3. The client (your application) shows the question to the end user.
4. The end user responds ("Flight B").
5. The client creates a NEW run on the same thread with a `resume_command` containing the user's response.
6. This new run goes through the normal createRunCtx + enqueue + GetJob cycle.
7. The runner loads the checkpoint (the saved graph state from where it paused), applies the resume command, and continues execution.

**Key characteristics:**

- The AGENT decides to pause (it is in the code)
- The END USER resumes (via the client application)
- Uses Agent Protocol (HTTP/WS) for the resume
- Requires durable checkpoints in Postgres for reliable resume across crashes
- This is about the agent's conversational flow, not security



## Kind 2: Connector Policy Pending (Governance HITL)

**What it is:** The control plane's security policy blocks a dangerous tool call and requires an administrator to approve it before it can proceed.

**How it works:**

1. The agent is running and decides to call a tool (e.g., `transfer_funds` on a banking connector).
2. The runner sends a `tools/call` MCP request to the control plane's connector proxy.
3. The control plane's policy engine evaluates: "Is tenant X's agent Y allowed to call `transfer_funds` on connector `bank`?"
4. The policy (either from a webhook to your OPA/Cedar service, or from a mandatory HITL rule) returns `effect: "pending"` — meaning "this needs human approval."
5. The control plane does NOT pause the LangGraph graph. It simply returns an error to the MCP call: `{"error": {"code": -32000, "data": {"reason_code": "policy_pending", "action_id": "act_abc123"}}}`.
6. The control plane writes a row to the `pending_actions` table in Postgres.
7. The control plane optionally publishes a `tool_auth` event on the run's SSE stream so dashboards/UIs can show "waiting for approval."
8. An ADMINISTRATOR opens the Admin UI (`/admin/pending`), sees the pending action, reviews it, and clicks "Approve."
9. On approve: the control plane re-checks policy (in case a hard deny rule was added in the meantime), and if still OK, stores a one-shot capability for this specific (run_id, generation, connector, tool) combination.
10. The agent code must RETRY the same tools/call. On this retry, the control plane sees the one-shot capability, consumes it, and forwards the call to the actual upstream service. The tool executes once.
11. If the agent calls the same tool a third time, there is no capability remaining, and it goes back to pending/deny.

**Key characteristics:**

- The PLATFORM decides to block (based on policy rules)
- An ADMINISTRATOR approves (via Admin UI, not the end user)
- The agent framework does NOT pause or enter an interrupt state — it simply gets an error on the tool call
- If the agent framework swallows the error without retrying, nothing happens — the agent appears "stuck" but it is an agent bug, not a platform bug
- Requires SQL backends (Postgres/MySQL/SQLite) for the pending_actions table — MongoDB cannot store these and fails closed to deny



## Why this distinction matters

If a support engineer sees "agent is stuck" and tries to fix it by approving something in the Admin pending UI: that only helps if it is Kind 2 (connector pending). If the agent is actually stuck because of Kind 1 (framework interrupt waiting for user input), admin approval does nothing — the user needs to send a resume command via the application.

Conversely, if someone tries to resume a governance-blocked agent by sending a resume command through the SDK: that creates a new run entirely, which will hit the same policy block again. The fix is admin approval, not user resume.

**Never use the word "resume" for Kind 2. Say "approve" and "retry."**
**Never use the word "approve" for Kind 1. Say "resume."**

---



# 11. Governance — Who Can Do What



## What governance means in Runkite

Governance is the control plane deciding: who is allowed to do what, keeping a permanent record of every decision, and giving administrators emergency controls when things go wrong.

It is NOT:

- Scanning prompts for injection attacks (that is AI safety, a different field)
- Redacting PII from outputs (that is data loss prevention)
- Sandboxing arbitrary Python code inside agent graphs (that is compute isolation)
- Rate limiting (that is availability protection, covered separately)

It IS:

- "Can tenant acme's sales-bot use the Salesforce connector?" (access control)
- "Can this specific agent call the `delete_repo` tool?" (tool-level control)
- "Show me every policy decision made in the last 24 hours" (audit trail)
- "Stop all runs for tenant X immediately" (kill switch)
- "Allow this specific blocked action to proceed once" (pending HITL approval)
- "Bypass policy for 2 hours because our PDP is down" (break-glass emergency)



## How the policy engine works (Decide)

Every time something security-relevant happens — a run is created, a connector session is minted, a tool is called — the control plane calls `Engine.Decide()` internally. This function evaluates multiple layers and returns one of three answers: **allow**, **deny**, or **pending**.

The evaluation layers, in order:

1. **Break-glass check:** If there is an active break-glass window, skip everything and allow. But audit MUST still be written — if the audit write fails, break-glass fails closed (you cannot bypass silently).
2. **Static grants (from config file):** The `langgraph.json` configuration can contain grants like "tenant acme, agent sales-bot, can use connector salesforce, tools: allow [query, getRecord], deny [updateRecord]". These are immutable at runtime.
3. **SQL overlay grants (from Admin UI):** Administrators can create additional grants via the Admin UI that are stored in Postgres. These override/supplement the static grants. This is how you manage permissions without redeploying.
4. **Sync webhook (your PDP):** If configured, Runkite sends the decision context to YOUR external policy decision point — an HTTP endpoint you control. This could be OPA (Open Policy Agent), Cedar, your own ABAC system, or anything that speaks HTTP. The webhook receives: what is being requested (stage, tenant, agent, connector, tool) and returns allow/deny/pending. If the webhook times out or returns an error, the decision is **deny** (fail-closed).
5. **Mandatory HITL rules:** Even if everything above says "allow," a mandatory HITL rule can force it to "pending." This is defense-in-depth: even if your webhook PDP is compromised and says "allow everything," mandatory HITL still blocks dangerous operations.
6. **Default effect:** If nothing matches and no webhook is configured, the default_effect setting decides (usually deny in production = fail-closed).



## How the control plane knows which tool is being called

When an agent uses a connector tool, the runner sends an MCP `tools/call` JSON-RPC message to the control plane's connector proxy at `/internal/connectors/{connector_name}/mcp`. The JSON body contains the tool name (like `"params": {"name": "delete_repo", "arguments": {...}}`).

The control plane reads the tool name from this JSON body. Combined with:

- The connector name (from the URL path)
- The tenant_id and agent_id (derived from the inflight assignment via run-binding — NOT from headers the runner sends, because those could be forged)

...the control plane has the complete tuple `(tenant, agent, connector, tool)` to evaluate policy. It never needs to parse the agent's source code or understand the framework.

## Kill switches

A kill switch is an emergency stop for a tenant, an agent, or a tenant+agent combination. When activated:

- All NEW run creates for that scope are immediately refused (403).
- Optionally (non-pause mode): all EXISTING pending/running runs in that scope are drained (cancelled in batches with pagination — a bug fix from an earlier version that silently stopped after 200 runs).

Use case: you discover an agent is misbehaving, calling APIs it should not, spending money uncontrollably. You activate the kill switch. Everything stops. You investigate, fix the problem, then deactivate.

## Break-glass

Break-glass is a time-bounded emergency bypass of the policy engine. Maximum duration: 24 hours. While active, `Decide` is skipped — connector calls and run creates proceed without policy evaluation.

Use case: your policy webhook (OPA server) is down. All connector calls are being denied because webhook timeout = deny (fail-closed). Production is broken. You mint a break-glass window for 2 hours. During those 2 hours, policy is bypassed. You fix your OPA server. After the window expires (or you manually close it), policy enforcement resumes.

Critical constraints:

- Break-glass REQUIRES a successful audit write. If audit cannot be written (database down), break-glass itself fails. You cannot silently bypass policy.
- Break-glass does NOT bypass kill switches. If something is killed, break-glass cannot revive it.
- Break-glass does NOT bypass rate limits or admission limits. Those are separate systems.
- The audit record permanently notes that break-glass was used, when, by whom, and what was bypassed (including whether mandatory HITL rules would have applied).



## The product claim boundary (important honesty)

There is a sentence in the docs that confuses people: "Connector Decide can run on any state backend when policy is configured. The durable trail requires Postgres/MySQL/SQLite."

What this means:

- **The evaluation** (should this tool call be allowed?) works on ALL backends, including MongoDB. It is just CPU work: compare grants, call webhook, check mandatory rules. No database writes needed for the hot-path decision.
- **The durable record** of those decisions needs SQL tables. Audit events, admin-created grants, pending actions, kill switches, break-glass windows, mandatory HITL rules — these are all SQL tables implemented for Postgres, MySQL, and SQLite.
- **MongoDB cannot store governance data.** On Mongo: audit writes are skipped (no durable trail), Admin governance routes return 501 (not implemented), pending actions cannot be persisted (so pending decisions fail closed to deny).
- **Therefore:** when we publicly claim features like "audit search, connector HITL, kill switches, break-glass," those claims are honest only for SQL backends. Mongo can enforce allow/deny from config grants, but it is NOT a complete governance story.

This is a deliberate product design choice. We are honest about it in `docs/limitations.md` rather than quietly hoping nobody tries it on Mongo.

---



# 12. Connectors — How Agents Use External Tools Safely



## What a connector is

A connector is a named external service that agents can use through the control plane. Examples: `github`, `salesforce`, `slack`, `stripe`. Each connector has configuration (how to reach the upstream service) and credentials (API keys, OAuth tokens) stored in the control plane.

The critical design principle: **runners never hold real credentials.** Instead of baking API keys into every runner container and hoping policy lives in prompts, the control plane mints short-lived session tokens and proxies requests. The runner gets a temporary pass, not the master key.

## Two HTTP paths from runner to control plane

When a runner needs to use a connector during execution, it makes HTTP calls back to the control plane (not gRPC — these are simple request-response):

**1. Get a session:** `POST /internal/connectors/{name}/session`

The runner says "I need access to the `github` connector for my current run." The control plane checks: is this runner authenticated? Does its inflight assignment (run_id + generation) match? Does policy allow session creation for this tenant/agent/connector? If all checks pass, the control plane mints a session token bound to this specific (run_id, generation, connector) with a 15-minute absolute TTL. The runner uses this token for subsequent MCP calls.

Why not mint the token at dispatch time and include it in the assignment? Two reasons: (1) The job might sit in Redis queue for longer than 15 minutes — the token would expire before the runner even starts. (2) Policy should be evaluated at use time, not dispatch time — grants might change between dispatch and actual use.

**2. Proxy an MCP call:** `POST /internal/connectors/{name}/mcp`

The runner sends MCP JSON-RPC messages (like `tools/call`, `tools/list`) to this endpoint with the session token in a header. The control plane:

- Validates the session token (correct run, generation, connector, not expired)
- For `tools/call`: runs policy Decide (is this tool allowed?)
- If allowed: injects the REAL upstream credentials and forwards the request to the actual service (Salesforce, GitHub, etc.)
- Returns the response to the runner

The runner never sees the real credentials. The upstream service never sees the runner directly. The control plane is the MCP proxy.

## Run-binding: preventing forgery

When a runner makes requests to `/internal/*` paths, it sends headers like `X-Runkite-Run-Id` and `X-Runkite-Generation`. But what prevents a malicious runner from faking these headers and pretending to be a different run or tenant?

The answer is **run-binding** (`internal/auth/runbind.go`). On these sensitive paths, the control plane does NOT trust the headers. Instead, it:

1. Looks up the inflight assignment in Redis by the claimed run_id
2. Verifies the generation matches
3. Derives tenant_id, agent_id, and user context FROM the assignment (which was written by the trusted createRunCtx path)
4. Ignores any `X-Runkite-Tenant-Id` header the runner sends

This means: even if a runner process is compromised and sends forged tenant headers, the control plane uses the assignment truth, not the runner's claim. The runner cannot escalate to another tenant's privileges.

## The complete gate-by-gate walkthrough: one tool call through all security layers

All the security features described across Chapters 11, 12, and elsewhere converge on ONE code path. Here is every gate a single `tools/call` passes through, in order, with the exact code file.

**Example:** Agent `finance_bot` (tenant `acme`) calls `transfer_funds` on the `bank` connector.

**Gate 0 — Runner authentication** (`internal/bridge/interceptors.go`). Before any of this, the runner authenticated its gRPC connection using `RUNNER_TOKEN_PYTHON_LANGGRAPH` from env vars. If this failed, the runner can't even call GetJob. Configured via environment variables on the CP.

**Gate 1 — Run-binding** (`internal/auth/runbind.go`). HTTP middleware on all `/internal/*` routes. The runner sends `X-Runkite-Run-Id` and `X-Runkite-Generation` headers. The CP looks up the inflight assignment in Redis and derives `tenant_id=acme`, `agent_id=finance_bot`, `user=user_alice` from the ASSIGNMENT — ignoring any identity headers the runner sends. This is automatic, no configuration needed.

**Gate 2 — Connector session** (`internal/api/server.go`, `requireConnectorSession`). The runner must present a valid `X-Runkite-Connector-Session` token. The CP verifies: token exists, not expired (15min TTL), was minted for THIS connector AND this run_id AND this generation. Prevents stolen tokens from being reused across runs.

**Gate 3 — Extract tool name** (`internal/api/server.go`). The CP parses the JSON-RPC body: `{"method": "tools/call", "params": {"name": "transfer_funds"}}`. Now it has the complete tuple: `(acme, finance_bot, bank, transfer_funds)`. Policy only fires for `tools/call` — read-only methods like `tools/list` skip it.

**Gate 4 — One-shot capability** (`internal/api/policy.go`, `tryConsumePendingCapability`). Was this exact `(run_id, generation, connector, tool)` already approved by an admin? If YES: consume the capability (delete it from Postgres so it can't be reused), skip all remaining policy gates, forward directly to the upstream bank API. This is how "retry after admin approval" works.

**Gate 5 — Break-glass** (`internal/api/break_glass.go`, `tryBreakGlassBypass`). Is there an active break-glass window for tenant `acme`? If YES: attempt to write an audit record first. If the audit write succeeds, allow the call (skip grants, webhook, mandatory HITL). If the audit write fails, the bypass itself is REFUSED — you cannot use break-glass silently. Configured via Admin UI `/admin/break-glass`.

**Gate 6 — Static grants** (`internal/policy/static.go`, `decide`). Search the merged grant list for `(tenant=acme, agent=finance_bot, connector=bank)`. If found, check the tool filter: is `transfer_funds` in the deny list? (Blocked.) Is it in the allow list? (Allowed.) No filter? (All tools allowed.) No matching grant at all? (`policy_no_grant` → denied, unless `default_effect` is allow). Configured in `langgraph.json` `policy.grants` AND Admin UI `/admin/grants` (Postgres overlays that win over config on the same key).

**Gate 7 — Webhook PDP** (`internal/policy/webhook.go`, `Decide`). If configured, the CP POSTs a JSON payload to your external policy server (OPA, Cedar, custom Flask app — anything that speaks HTTP). Your server responds with `{"effect": "allow"}`, `{"effect": "deny"}`, or `{"effect": "pending"}`. If the webhook times out or errors: **deny** (fail-closed by default). Configured in `langgraph.json` `policy.webhook`.

**Gate 8 — Mandatory HITL** (`internal/policy/policy.go`, `applyMandatoryHITL`). Even if the webhook said allow, this gate can override it to pending. If a mandatory rule matches `(acme, bank, transfer_funds)`, the allow is forced to pending. This exists so a compromised PDP that allows everything is still blocked on critical tools. Configured in `langgraph.json` `policy.mandatory_hitl` AND Admin UI `/admin/mandatory-hitl`.

**Gate 9 — Audit** (`internal/policy/policy.go`). Every decision (allow, deny, pending) writes a permanent record to the `audit_events` table in Postgres. This is not a blocking gate for normal decisions, but it is for break-glass (break-glass fails if audit can't be written).

**Final outcome:** ALLOW → forward to the real bank API, inject real credentials, return result. DENY → return JSON-RPC error to the runner. PENDING → persist a `pending_actions` row in Postgres, emit a `tool_auth` SSE event so dashboards can show "waiting for approval", return JSON-RPC error to the runner.

**Important:** Kill switches are NOT in this tool-call path. They block run CREATION (in `createRunCtx` step 3d). By the time a tool call is happening, the run already passed the kill switch check at creation time.

## Keeping the upstream credential out of `langgraph.json` (`secret_ref`, shipped in v0.3)

Everything above assumes the control plane already HAS the real upstream credential (the GitHub token, the Salesforce client secret) somewhere. Originally that "somewhere" was `langgraph.json` itself: `auth.api_key: "ghp_abc123..."` sitting in plaintext in a config file — the same file you'd want to commit to git for the rest of your infrastructure-as-code layer (Chapter 12's "three configuration layers"). That's a bad trade: either you don't commit the file (losing config-as-code for everything else in it) or you commit a real credential.

`secret_ref` (`internal/connector/config.go`'s `AuthConfig.SecretRef`) replaces the literal value with a small reference string, resolved fresh every time a runner asks for a session — not baked in at boot, not cached to disk:

- `env:GITHUB_TOKEN` — read from the control plane process's own environment
- `file:/run/secrets/github_token` — read from a file (the standard Docker/Kubernetes secret-mount pattern), trimmed of whitespace
- `vault:secret/data/runkite/github#token` — fetch from HashiCorp Vault's KV v2 API, `#token` selecting one field from the JSON secret

The config file itself now just says `"secret_ref": "vault:secret/data/runkite/github#token"` — a pointer, safe to commit, meaningless without access to the actual Vault/env/file it points at.

**Why `vault:` paths are allowlisted, not free-form:** `ResolveSecretRef` (`internal/connector/secretref.go`) cleans the path (rejects any `..` segment) and then requires it to start with `VAULT_ALLOWED_PREFIX` (default `secret/data/runkite/`). Without this, a `secret_ref` value — which could come from an Admin UI edit, not just a deploy-time file — could point anywhere in your organization's Vault and exfiltrate an unrelated secret through a connector's `GetSession` call. The allowlist means a Runkite connector can only ever resolve secrets that live under the path your Vault admin explicitly carved out for it.

**Failure mode is fail-closed:** an env var that's unset or empty, a file that doesn't exist, or a Vault path outside the allowlist all return an error, not an empty string silently used as "no credential." `GetSession` fails; the runner gets an error instead of authenticating to the upstream service with a blank token.

**Code:** `internal/connector/secretref.go` (`ResolveSecretRef`, one function per scheme), `internal/connector/config.go` (`AuthConfig.SecretRef` field), called from `internal/connector/registry.go`'s session-minting path — deliberately at GetSession time, same reasoning as the connector session tokens above: resolve at use time, not dispatch time, so credential rotation doesn't need a restart.

## The three configuration layers (why settings live in different places)

All governance configuration exists across three layers, each for a different change velocity:

**Layer 1: Deploy-time (langgraph.json + env vars).** This is the baseline. Runner tokens, webhook URL, static grants, mandatory HITL rules, rate limit rules, auth config. Changing these requires editing the file and restarting the CP. This is your "infrastructure as code" layer — version-controlled, reviewed in PRs, deployed with the binary.

**Layer 2: Runtime Postgres (Admin UI overlays).** Administrators can add/remove grants, mandatory HITL rules, kill switches, break-glass windows, and pending action approvals through the Admin UI without any restart or redeploy. These rows in Postgres are called "overlays" — they merge with the deploy-time baseline. On the same `(tenant, agent, connector)` key, an overlay wins over the baseline.

**Layer 3: In-memory (the merged result).** The `Decide()` function never queries Postgres on the hot path — it reads from an in-memory merged cache rebuilt from Layer 1 + Layer 2. Every ~15 seconds, each CP polls Postgres for a fingerprint of all overlay grants. If the fingerprint changed (admin modified something), the CP reloads and rebuilds. This means: admin changes take effect across all replicas within ~15 seconds, and `Decide()` adds near-zero latency (nanoseconds for in-memory lookup vs milliseconds for a DB query).

**Why three layers?** Speed (in-memory is fast), flexibility (admins don't need a deploy to change permissions), durability (Postgres survives CP restarts, config survives container rebuilds), and separation of concerns (developers set baselines, admins adjust at runtime).

---



# 13. Cron — Scheduled Runs



## What cron does

Cron is the control plane's built-in scheduler. You configure it to create runs on a schedule — like "run the nightly-report agent every day at 3 AM UTC." It is essentially an automated client that creates runs at specified times.

## How it works

You configure schedules in `langgraph.json`:

```json
"cron": {
  "nightly-echo": {
    "agent_id": "echo_agent",
    "expression": "0 3 * * *",
    "timezone": "UTC",
    "input": {"messages": [{"role": "human", "content": "nightly health check"}]}
  }
}
```

A background goroutine in the control plane wakes every ~15 seconds and checks: "Is any schedule's next fire time in the past?" If yes, it tries to claim that fire.

**Multi-replica safety:** With three control planes running, all three will check schedules. To prevent triple-firing, the control plane uses a `cron_claims` table in Postgres. It attempts an insert of `(schedule_name, fire_time)`. If the insert succeeds (no duplicate), this replica wins and creates the run. If it fails (another replica already inserted), this replica does nothing. This is "insert-if-absent" election — exactly one replica fires each schedule.

After claiming, the winning replica creates a run through the normal createRunCtx path. The run goes through the same enqueue, GetJob, execution, completion flow as any other run. Cron does not invent a separate execution mechanism.

## What happens if the winning replica dies after claiming but before creating the run?

The claim row exists (so nobody else will fire it), but the run was never created. This is a rare edge case. The system prefers "at most once" over "at least once" for schedule fires. An operator can investigate by checking the cron_claims table against actual run records.

---



# 14. The Admin UI



## What it is

The Admin UI is a React single-page application embedded into the control plane binary. You open it at `http://your-control-plane:2026/admin/`. It requires admin-level authentication (either an admin API key or an admin session cookie after login).

## What you can do in it

- **Overview:** See system health, active runs, recent errors.
- **Agents:** List configured agents and their settings.
- **Threads:** Browse conversations, see their status and run history.
- **Runs:** List runs with filters, see details, cancel runs.
- **Connectors:** See configured connectors and their status.
- **Cron:** See scheduled tasks, their next fire times, recent executions.
- **Grants:** Create and manage policy grants (who can use what connector/tool). This is the admin overlay on top of config-file grants.
- **Mandatory HITL:** Configure rules that force specific tool calls to require human approval regardless of what the webhook PDP says.
- **Pending:** See tool calls waiting for admin approval (Kind 2 HITL). Approve or deny them here.
- **Kill Switches:** Activate or deactivate emergency stops for tenants/agents.
- **Break-glass:** Mint or close emergency policy bypass windows.
- **Audit:** Search the audit trail of all policy decisions.



## Technical details

The UI is built separately (in `admin-ui/`) and the production build is embedded into the Go binary via Go's `embed` package. End users do not need Node.js installed — the binary serves the pre-built JavaScript/CSS directly.

Browser authentication uses httpOnly cookie sessions with CSRF protection for mutations. Machine automation can use `Authorization: Bearer` tokens instead. Sessions are stored in Redis (if configured) for cross-replica persistence, or in process memory (requiring sticky routing to `/admin`).

**Code:** `admin-ui/` for the React source, `internal/adminui/` for the embedded dist, `internal/api/admin_*.go` for the API routes, `internal/auth/adminsession*.go` for session management.

---



# 15. Rate Limiting



## The problem

You need to prevent individual tenants, users, or agents from overwhelming the system with too many requests. Rate limiting says "you can make at most N requests per time window."

## How it works with one control plane

Simple: an in-memory token bucket per dimension (per-user, per-agent, per-tenant, global). Every request checks and decrements the bucket. If the bucket is empty, return 429 Too Many Requests.

## How it works with multiple control planes

This is the tricky part. With three replicas, each has its own in-memory buckets. A user hitting all three replicas via round-robin effectively gets 3x the configured limit — because each replica independently thinks "you still have budget."

**Fix:** When `REDIS_URL` is configured, rate limiting automatically switches to Redis-backed shared counters. All replicas decrement from the same bucket. The cluster-wide limit is now correct.

If you explicitly set `backend: memory` with multiple replicas, you are misconfigured and your limits will be soft.

## Fail-open on Redis blip

If Redis is briefly unreachable during a rate limit check, the request is **allowed** (fail-open). This is a deliberate availability choice: a brief Redis blip should not kill your entire API.

This is DIFFERENT from policy Decide, which is fail-closed (Redis/webhook errors = deny). The reasoning: rate limiting protects availability (let traffic through if unsure), while policy protects security (block if unsure). Different risk preferences.

## Rate limits vs admission limits

Do not confuse them:

- **Rate limits** answer: "Is this HTTP request allowed RIGHT NOW given recent traffic?" Response: 429.
- **Admission limits** answer: "Does this tenant/agent already have too many concurrent runs or have they exceeded their daily quota?" Response: also 429, but a different check with a different backend (SQL atomic count+insert).

Both return 429, but they are different systems checking different things.

---



# 16. Fail-Closed Serve — Why Production Refuses to Start Insecurely



## The problem it solves

In an earlier version, running `runkite serve` without proper configuration would start the server with: a local SQLite database (no multi-replica safety), in-process transport (no shared queues), no runner authentication (any process can claim to be a runner), and no client authentication (any HTTP client can access the API). Kubernetes would see the process as healthy (it passes `/readyz`) and send production traffic to this insecure server.

## How it works now

When you run `runkite serve`, the very first thing it does (before listening for any requests) is run a safety checklist called "admission problems":

1. **Is a durable database configured?** (POSTGRES_DSN, MYSQL_DSN, or MONGO_URI?) SQLite is not safe for multi-replica.
2. **Is a shared transport configured?** (REDIS_URL, NATS_URL, or KAFKA_URL?) In-process cannot coordinate replicas.
3. **Are runner tokens configured?** (Any `RUNNER_TOKEN_`* environment variables?) Without these, any process can be a runner.
4. **Is client auth configured?** (`auth` section in langgraph.json?) Without it, the API is wide open.

If ANY of these checks fails, the server prints a clear error message explaining what is missing and **exits with status 1**. It never starts listening. It never passes a readiness probe. It never serves a single request.

This is "fail-closed admission": when you cannot prove the system is configured safely, refuse to start rather than running insecure.

## Escape hatches

- `RUNKITE_ALLOW_INSECURE_SERVE=1` — tells the server "I know what I am doing, start anyway." For private networks, CI environments, or deliberate demos.
- `RUNKITE_MODE=test` — test mode also bypasses.
- `runkite dev` — the development command is intentionally zero-dependency and always starts without these checks.



## The design philosophy

Three different things in Runkite use "fail-closed" but at different layers:

1. **Serve admission** (this chapter): fail to START if not configured safely.
2. **Policy Decide** (Chapter 11): fail to ALLOW a tool call if the webhook errors.
3. **Rate limiting** (Chapter 15): fail OPEN (allow) if Redis blips — different because it protects availability, not security.

**Code:** `cmd/serve.go`, function `admissionProblems` or `checkProductionAdmission`.

---



# 17. Conformance — How We Test Multiple Backends



## The problem

Runkite supports multiple state backends (Postgres, MySQL, SQLite, MongoDB) and multiple transport backends (Redis, NATS, Kafka, in-process). How do you ensure that switching from Postgres to MySQL does not subtly break thread claiming? How do you ensure that NATS event streaming behaves the same as Redis?

## The solution: shared test suites

Conformance suites are test batteries that define the BEHAVIOR a backend must exhibit, independent of implementation. For example:

"If I enqueue a job and then dequeue, I should get that job back."
"If I publish an event and someone is subscribed, they should receive it."
"If a lease expires and I run reclaim, the job should be requeued with an incremented generation."

Every backend implementation must pass these exact tests. If Postgres passes "create thread, claim thread, release thread, verify idle" then MySQL must pass the same test with the same assertions.

The tests live in `internal/transport/conformance/` for transport backends and equivalent locations for state backends.

## What conformance is NOT

- Not a production soak test (running under load for days)
- Not a multi-replica HA test (that is the docker-compose multi setup)
- Not a performance benchmark

It is purely: "does this implementation correctly fulfill the interface contract?"

## Tier system

Based on conformance AND operational evidence:

- **Supported:** Conformance passes + multi-replica soak tested + production recommended. Today: Postgres + Redis.
- **Compatible:** Conformance passes + some operational evidence but not full production soak. Today: NATS triad, MySQL, SQLite (governance), Kafka+Redis, Helm packaging.
- **Experimental:** Partial conformance, known gaps, use at your own risk. Today: Kafka without Redis for multi-replica.

---



# 18. Gaps and Honest Limitations

This is the list of things that do NOT work perfectly, that have known race conditions, or that are not yet built. They are documented in `docs/limitations.md` and I am listing them here because honest documentation is part of the product's trust story.

**MongoDB governance gap:** Mongo can enforce allow/deny from config grants. But it cannot store audit events, admin overlay grants, pending actions, kill switches, break-glass, or mandatory HITL rules. Admin governance routes return 501. Pending fails closed to deny. Do not market governance for Mongo.

**Kafka without Redis:** Kafka alone lacks EventBroker fan-out and CancelBroker shapes for multi-replica. Without Redis you get single-instance event delivery only. ReclaimStale across replicas needs Redis for leader election. This combination is Experimental.

**MCP sticky routing:** Agent Protocol MCP sessions (`/mcp`) store state in process memory. Multi-replica deployments MUST configure sticky load balancing for this path or sessions break randomly.

**Non-terminal event non-fencing:** Progress events (tokens, intermediate updates) from a superseded runner are NOT filtered. Only terminal (completion) events are generation-fenced. This means you might see a few stray progress events from a dead runner before it receives `superseded: true` on its next heartbeat. The final status is always correct.

**A2A breadth race:** Concurrent fan-out under the same parent can exceed max_breadth due to the check-then-create pattern without locking. Documented, tolerated as a safety-net rather than an iron guarantee.

**Rate limits with memory backend on multi-replica:** Provides N-times the configured ceiling (where N is the number of pods). Misconfiguration, not a product bug. Redis fixes it.

**Direct-mode trust:** When runners connect directly to Postgres for LangGraph checkpoints (direct mode), they hold database credentials. In multi-tenant scenarios, this means a compromised runner has read/write access to all tenants' checkpoints. Single-tenant deployments are fine. Multi-tenant production should use the proxy store mode.

**NATS EventBroker.Close:** A known semantic gap where a late subscriber after a run terminates may get an open empty channel instead of a "closed" marker (as Redis provides). Callers also check SQL terminal status as a safety net.

---



# 19. Observability — OpenTelemetry and Prometheus



## OpenTelemetry (distributed tracing)

The control plane and runners all use the standard OpenTelemetry libraries:

- Control plane: `go.opentelemetry.io/otel` (official Go SDK)
- Python runners: `opentelemetry-sdk` (official Python SDK)
- TypeScript runners: `@opentelemetry/sdk-node` (official Node SDK)

**How to enable:** Set `OTEL_EXPORTER_OTLP_ENDPOINT=http://your-collector:4317` (or equivalent). Without this env var, tracing is a no-op with near-zero overhead.

**What gets traced:**

- HTTP request spans (each API call gets a span)
- gRPC request spans (each runner RPC gets a span)
- Run lifecycle spans (from create to terminal, with the run_id as correlation)
- Policy decisions (as span events within the run span)
- Runner-side: LLM calls, tool calls (as child spans under the run)

**How correlation works:** When createRunCtx builds the assignment, it includes a W3C `traceparent` header in the assignment JSON. The runner reads this and creates its child spans as children of the control plane's span. This gives you a complete distributed trace: client → CP HTTP span → queue wait → runner execution span → LLM call span → back through CP to client.

**What is NOT captured:** Prompt and completion content. Runkite does not capture what the LLM said. This is deliberate: "govern the plane, not the prompt." Use Langfuse, LangSmith, or your observability backend for prompt-level tracing.

## Prometheus

The control plane exposes `/metrics` in Prometheus format. Scrapers like Prometheus, Datadog Agent, or Grafana Agent can collect these.

Metrics include: active run count, request latency histograms, queue depth (polled periodically from Redis), error counters, rate limit hit counters.

**Security note:** `/metrics` is unauthenticated by default. Protect it with network policy or set `RUNKITE_METRICS_TOKEN` to require a bearer token.

---



# 20. Boot Path — What Happens When the Server Starts

When you run `runkite serve`, here is the sequence:

1. **Parse CLI flags and environment.** Determine mode (serve vs dev vs db migrate).
2. **Production admission check** (Chapter 16). If this is `serve` mode and safety checks fail, exit immediately.
3. **Select and connect state backend.** Based on `POSTGRES_DSN` / `MYSQL_DSN` / `MONGO_URI` / fallback SQLite, create the state store connection. Run database migrations if needed (`runkite db upgrade` or auto-migrate depending on config).
4. **Select and connect transport.** Based on `REDIS_URL` / `NATS_URL` / `KAFKA_URL` / fallback in-process, create the job queue, event broker, and cancel broker.
5. **Initialize shared Redis client** (if `REDIS_URL` set). Used by: transport, admin sessions, rate limits, reclaim leader, queue depth polling.
6. **Initialize authentication.** Load auth config (JWT, API keys, webhook), runner tokens from `RUNNER_TOKEN_`* env vars, admin keys, session config.
7. **Initialize policy engine.** Load grants from config, connect webhook client if configured, load SQL overlays from database, set up cache.
8. **Initialize connectors.** Load connector configs, set up session stores, circuit breakers, OAuth token managers.
9. **Build the API server.** Wire all the above into `api.NewServer(...)`. This creates the HTTP mux with all routes.
10. **Start background goroutines:**
  - Reclaim loop (every 2s, checks for stale inflight jobs)
    - Cron scheduler (every ~15s, checks for due schedules)
    - Retention cleanup (deletes old data per config)
    - Run timeout enforcer (if configured, cancels runs exceeding wall-clock limits)
    - Policy overlay poller (every ~15s, syncs SQL grant changes across replicas)
    - Queue depth poller (periodically reads queue size for Prometheus metrics)
11. **Register gRPC bridge.** The Runner Protocol service is registered on the gRPC server.
12. **Start listening.** HTTP server and gRPC server begin accepting connections. TLS if configured.

At this point the server is live and ready for traffic.

**Code:** `cmd/serve.go` is the orchestrator. `cmd/main.go` dispatches between commands. Individual `cmd/*.go` files contain the background goroutine implementations.

---



# 21. Security Findings — What We Fixed and What Remains



## Fixed vulnerabilities (selected highlights)

**Empty permissions treated as unrestricted:** When `strict_permissions` was enabled but a token had an empty permissions set, it was incorrectly treated as "can do anything" instead of "can do nothing." Fixed to fail-closed: empty = no permissions.

**Admin localStorage secrets:** The Admin UI originally stored API keys in browser localStorage (accessible to JavaScript, vulnerable to XSS). Fixed to use httpOnly cookie sessions (not accessible to JavaScript) with CSRF protection for mutations.

**CORS wildcard with credentials:** The old CORS handler reflected any Origin header AND set `Allow-Credentials: true`. This is strictly worse than `Access-Control-Allow-Origin: `* because browsers actually block `*` with credentials but allow reflected origins with credentials — effectively opening authenticated cross-site requests from any domain. Fixed: credentials only for explicitly listed origins; never with wildcard.

**Serve insecure defaults:** Covered in Chapter 16. Fixed with fail-closed admission.

**Tenant forgery on proxy paths:** Runners could set `X-Runkite-Tenant-Id` headers to impersonate other tenants on connector/store/vector paths. Fixed with run-binding: tenant is derived from the inflight assignment, not from runner-supplied headers.

**Policy cache missing principal:** Cache key was `(stage, tenant, agent, connector, tool)` without including the user principal. Result: Bob could receive Alice's cached "allow" decision. Fixed: include principal in cache key.

**MCP session hijack:** Connector session tokens were usable across different runs or generations. A token minted for one run could be reused by another. Fixed: bind token to (run_id, generation, connector) and validate on every use.

**Redis fencing corruption:** Race conditions in Redis operations allowed generation mismatches to corrupt inflight tracking. Fixed with atomic Lua scripts and proper generation checks.

**Kill drain stopping at 200:** The kill switch drain loop stopped after processing 200 runs and miscounted cancels, leaving additional runs alive under a "killed" tenant. Fixed with proper pagination until scope is empty.

## Fixed in the pre-v0.3 hardening pass — a genuine "the fix itself had a bug" story

An external review of the codebase right before the v0.3.0 tag surfaced several real issues, and fixing them surfaced a second layer of bugs worth remembering because they're the kind that slip past a first pass:

**Admin login rate limiter that rate-limited nothing.** `POST /admin-api/session` had zero brute-force protection — unlimited login attempts against `admin_keys`. The first fix added a 5/min-per-IP token bucket, keyed off `X-Forwarded-For` when present. That fix shipped a WORSE problem than having no limiter at all: `X-Forwarded-For` is a header the *client* sets, and this project has no trusted-reverse-proxy configuration anywhere. An attacker sends a fresh, made-up `X-Forwarded-For` value on every single request and gets a brand-new rate-limit bucket every time — proved with a live test: 20 login attempts from the same real connection, spoofing the header each time, 0 got blocked. The real fix: `clientIP()` ignores `X-Forwarded-For` entirely and keys only on `r.RemoteAddr` (the actual TCP peer, which the client cannot spoof no matter what headers it sends). Coarser if you ever put a real reverse proxy in front (rate-limits per proxy IP, not per real client) — but never silently a no-op. **Lesson:** a security control that trusts client-supplied identity data is not a security control; it's a checkbox that looks like one until someone tests it adversarially.

**`ListNamespaces`'s `suffix` filter, silently ignored — and the fix broke a different backend's CI.** The store's `ListNamespaces` accepted a `Suffix` field in its request struct, but every SQL backend's query builder only ever applied `Prefix` — `Suffix` was parsed and then dropped on the floor, for every backend, since whenever this endpoint was written. Fixing it meant adding the equivalent `nsSuffixPattern`/`nsSuffixRegex` matcher to Postgres, MySQL, SQLite, AND a new conformance test (see Chapter 17) asserting the fix in the *shared* test suite every backend runs. That shared test immediately broke MongoDB's CI, because MongoDB's `ListNamespaces` was deliberately left un-fixed at first (documented reasoning: "Mongo is Compatible tier, not Supported") — except the new test doesn't care about tier, it runs against every backend that calls `RunStoreSuite`, Mongo included. **Lesson:** a shared conformance suite is a double-edged sword — it catches a bug once, everywhere, but that also means a partial fix that skips one backend on purpose will get caught by the SAME mechanism that's supposed to protect you, if you add the regression test to the shared suite instead of a backend-specific one. Fixed by adding the same suffix matcher to Mongo's `ListNamespaces` too, closing the tier gap rather than special-casing the test.

**`RunkiteStore.batch()` calling itself from its own event loop.** The sync `batch()` API, when called from inside a *running* event loop, hopped to a worker thread and used `run_coroutine_threadsafe` to schedule `abatch()` back onto "the loop" — but didn't check WHICH loop it was already on. If the caller was already running on the store's own loop, this deadlocks: the calling coroutine is blocked in `.result()` waiting for `abatch()`, but `abatch()` can only run once the loop is free to schedule it, and the loop is busy running the blocked caller. No exception, just a hang — discovered as a live 30-second `PoolTimeout` in a soak test, not a fast failure. Fixed by checking `running is self._loop` first and raising a clear `RuntimeError` immediately ("use `await store.abatch(...)` instead") rather than deadlocking. **Lesson:** fail fast and loud beats fail slow and silent, especially for a class of bug (self-deadlock) that by definition never produces a stack trace pointing at the actual cause.

**Supply-chain: every GitHub Action pinned by a floating tag.** All 5 workflows referenced actions by tag (`@v4`, `@v6`, `@v9`) rather than commit SHA — the textbook GitHub Actions supply-chain risk (a compromised tag pushes malicious code straight into CI; see the real-world `tj-actions/changed-files` incident). Fixed by resolving all 15 distinct actions used across the workflows to their current commit SHA via the GitHub API and pinning with a `# vX.Y.Z` trailing comment for human readability — confirmed each pin resolves to the SAME version already in use (not a silent major-version jump) by independently re-resolving every SHA against GitHub's API a second time.

**Checkpoint lock map and admin `finishedRuns` map, both unbounded.** Two small process-local memory leaks: the proxy checkpoint saver's per-thread `asyncio.Lock()` map never evicted entries (one lock per checkpoint key, forever), and the control plane's `finishedRuns sync.Map` (the process-local fast path that skips a second DB claim on cancel/StatusCallback races) never expired entries either. Fixed with a 4096-entry cap that evicts idle (unlocked) locks on the Python side, and a `time.AfterFunc(1 hour, ...)` expiry on the Go side.

## Known residuals (still true today)

- **Direct-mode trust:** Runner holds database credentials. Multi-tenant risk.
- `/metrics` **unauthenticated by default:** Network-restrict or use token.
- **MongoDB governance gap:** 501 on admin governance routes.
- **Non-terminal event non-fencing:** Stray progress events from stale runners.
- `/mcp` **requires sticky routing:** Process-local sessions.
- **Kafka without Redis:** Not safe for multi-replica reclaim.
- **A2A breadth race:** Can overshoot under concurrency.
- **Admin session TTL is sliding, no absolute max:** A browser that keeps making requests within each 12h window stays logged in indefinitely. No hard cap yet.
- **Reverse-proxy trust is all-or-nothing:** The admin login limiter (above) now correctly ignores `X-Forwarded-For`, which means a deployment genuinely behind a reverse proxy rate-limits per proxy IP, not per real client, until real trusted-proxy configuration exists. Coarser, not broken.

---



# 22. File Map — Where Everything Lives in the Code

When you open a file, this tells you which product part you are in.


| Path                               | What it is                                                        |
| ---------------------------------- | ----------------------------------------------------------------- |
| `cmd/main.go`                      | CLI entry point (dev/serve/db/vector)                             |
| `cmd/serve.go`                     | Boot wiring: connects everything, starts the server               |
| `cmd/reclaim.go`                   | Background goroutine that detects dead runners and reassigns work |
| `cmd/cron.go`                      | Background goroutine that fires scheduled runs                    |
| `cmd/retention.go`                 | Background goroutine that deletes old data                        |
| `cmd/run_timeout.go`               | Background goroutine that enforces wall-clock deadlines           |
| `cmd/policy.go`                    | Policy engine initialization                                      |
| `cmd/policy_overlay_poll.go`       | Background goroutine syncing SQL grants across replicas (~15s)    |
| `cmd/db.go`                        | Database migration CLI commands                                   |
| `cmd/tls.go`                       | TLS certificate helpers                                           |
| `internal/api/server.go`           | HTTP server struct and route registration                         |
| `internal/api/runs.go`             | createRunCtx and all run-related HTTP handlers                    |
| `internal/api/streaming.go`        | SSE implementation                                                |
| `internal/api/websocket.go`        | WebSocket command/event handling                                  |
| `internal/api/a2a.go`              | Agent-to-agent delegation handlers                                |
| `internal/api/alias.go`            | A/B weighted routing                                              |
| `internal/api/admin_*.go`          | Admin API (grants, kill, break-glass, pending, audit)             |
| `internal/api/admission.go`        | Run admission gate                                                |
| `internal/auth/`                   | Client auth, admin sessions, runner tokens                        |
| `internal/auth/runbind.go`         | Run-binding enforcement (prevents tenant forgery)                 |
| `internal/policy/`                 | Policy engine: Decide, grants, webhook, cache, audit              |
| `internal/bridge/server.go`        | The five gRPC RPCs (Runner Protocol implementation)               |
| `internal/bridge/interceptors*.go` | Runner authentication on gRPC                                     |
| `proto/runner.proto`               | Runner Protocol gRPC schema definition                            |
| `runner-protocol/PROTOCOL.md`      | Runner Protocol documentation                                     |
| `internal/transport/`              | Transport interfaces (JobQueue, EventBroker, CancelBroker)        |
| `internal/transport/redis/`        | Redis implementation of all three (Supported)                     |
| `internal/transport/nats/`         | NATS implementation (Compatible)                                  |
| `internal/transport/kafka/`        | Kafka JobQueue (needs Redis for broker/cancel)                    |
| `internal/transport/inprocess/`    | Single-process implementation (dev mode)                          |
| `internal/transport/conformance/`  | Shared test suites for transport backends                         |
| `internal/state/`                  | State store interface and all backend drivers                     |
| `internal/connector/`              | Connector session management, MCP proxy, circuit breaker          |
| `internal/ratelimit/`              | Rate limiting (memory and Redis backends)                         |
| `internal/hooks/`                  | Preflight and observational hooks                                 |
| `internal/tracing/`                | OpenTelemetry helpers                                             |
| `internal/cors/`                   | CORS rules (fixed credential handling)                            |
| `internal/secureheaders/`          | Security response headers                                         |
| `docs/architecture.md`             | Public architecture documentation                                 |
| `docs/trust-governance.md`         | Public governance documentation                                   |
| `docs/limitations.md`              | Public honest gaps                                                |
| `docs/api.md`                      | API reference                                                     |
| `examples/policy_webhook/`         | Reference policy decision point (PDP) implementation              |
| `python/runkite_runner/`           | Python runner (LangGraph reference implementation)                |
| `typescript/runkite-runner/`       | TypeScript runner (LangGraph.js)                                  |
| `python/adapters/`                 | CrewAI, LlamaIndex, AutoGen, LangChain adapters                   |
| `admin-ui/`                        | React Admin UI source                                             |
| `deploy/helm/runkite`              | Helm chart for Kubernetes                                         |
| `docker-compose.multi.yml`         | Multi-replica HA topology for testing                             |


---



# Appendix A: Policy Configuration Examples



## Example 1: Grants-only (no webhook)

```json
"policy": {
  "default_effect": "deny",
  "grants": [
    {
      "id": "acme-sales-sf",
      "tenant_id": "acme",
      "agent_id": "sales-assistant",
      "connector": "salesforce",
      "tools": {"allow": ["query", "getRecord"], "deny": ["updateRecord", "deleteRecord"]}
    }
  ]
}
```

What this means in English: For tenant "acme", agent "sales-assistant" can use the "salesforce" connector but ONLY the tools "query" and "getRecord". It cannot use "updateRecord" or "deleteRecord" (explicitly denied). Any other tool on salesforce that is not in the allow list is also denied (because allowlist semantics: if you specify an allow list, only those are allowed). Any other tenant/agent combination is denied by default. No external webhook is consulted.

## Example 2: Empty policy (Admin-managed)

```json
"policy": {}
```

What this means: The policy engine is ENABLED (fail-closed, all connector calls denied by default), but there are no grants in the config file. All permission management happens through the Admin UI (SQL overlay grants). This is for organizations that want runtime-managed permissions without redeploying.

## Example 3: External webhook PDP

```json
"policy": {
  "webhook": {
    "url": "http://my-opa-server:8181/v1/data/runkite/decide",
    "secret": "hmac-signing-secret",
    "timeout_ms": 2000
  }
}
```

What this means: Every policy decision is forwarded to your own server. Runkite sends a POST with details (tenant, agent, connector, tool, stage) and expects back `{"effect": "allow"}` or `{"effect": "deny"}` or `{"effect": "pending"}`. If your server is slow (>2 seconds) or returns an error, the decision is DENY (fail-closed). The secret is used to HMAC-sign requests so your PDP can verify they came from Runkite.

## Example 4: Break-glass usage

This is not JSON config — it is an admin action:

1. OPA server is down. All connector calls are being denied.
2. Admin opens `/admin/break-glass` and creates a window: scope=tenant "acme", duration=2 hours, reason="OPA outage".
3. For the next 2 hours, policy Decide is skipped for tenant "acme" connector calls. Calls proceed.
4. Audit records note every call that was made during the break-glass window.
5. Fix OPA. After 2 hours (or manual close), policy enforcement resumes.

---



# Appendix B: Key Numbers to Remember


| What                  | Value                     | Why                                                      |
| --------------------- | ------------------------- | -------------------------------------------------------- |
| Heartbeat interval    | ~2 seconds                | Frequent enough to detect crashes quickly                |
| Reclaim stale maxAge  | ~6 seconds                | 3 missed heartbeats = declared dead                      |
| gRPC keepalive        | ~4 seconds                | Detects dead TCP connections                             |
| Connector session TTL | 15 minutes absolute       | Limits blast radius of leaked tokens                     |
| Admin session TTL     | 12 hours sliding          | Convenient for operators without being permanent         |
| Policy overlay poll   | ~15 seconds               | Cross-replica grant convergence time                     |
| Reclaim leader TTL    | 3 seconds (with 2s renew) | Ensures only one reclaim process                         |
| Break-glass max       | 24 hours                  | Cannot create permanent policy bypass                    |
| Generation starts at  | 1 (not 0)                 | 0 = legacy/unknown, distinguishable from "first attempt" |


---



# Appendix C: Glossary

**Ack** — Tell the queue "this job is finished" for its generation. Removes inflight tracking.

**Agent Protocol** — The public HTTP/SSE/WS API that clients use.

**Assignment** — The JSON payload a runner receives from GetJob containing everything needed to execute.

**Break-glass** — Time-bounded policy bypass with mandatory audit trail.

**CancelBroker** — The Redis Pub/Sub abstraction that delivers cancel signals across replicas.

**Conformance** — Shared test suites that all backends must pass to prove correct behavior.

**Control plane (CP)** — The Go binary (`runkite serve`) that coordinates everything.

**createRunCtx** — The single function in `internal/api/runs.go` that all run creation passes through.

**Decide** — The policy engine's evaluation function. Returns allow/deny/pending.

**Dequeue** — Pop the next job from the ready queue into inflight (with lease).

**EventBroker** — The Redis Streams abstraction that fan-outs events to SSE/WS subscribers.

**Fail-closed** — When uncertain, deny/refuse. Used for policy and serve admission.

**Fail-open** — When uncertain, allow. Used for rate limiting during Redis blips.

**Fan-out** — One publisher, many subscribers. Redis Streams is the bulletin board.

**Fencing/Generation** — A counter that prevents stale runners from corrupting results.

**GetJob** — gRPC long-poll where a runner waits for work.

**Heartbeat** — Runner's periodic proof-of-life call (every ~2s).

**HITL** — Human In The Loop. Two kinds: framework interrupt (user resumes) and connector pending (admin approves).

**Inflight** — A job that has been dequeued but not yet Acked (completion acknowledged).

**Kill switch** — Emergency stop for a tenant/agent. Refuses new runs, optionally drains existing.

**Lease** — Time-bounded ownership of an inflight job, refreshed by heartbeat Renew.

**Nack** — Return a job to the ready queue without completing it (e.g., runner died before starting).

**Overlay** — Admin-created SQL grants that supplement/override config-file grants.

**Pending (run status)** — Created but not yet picked up by a runner.

**Pending (policy)** — Decide returned "pending" = needs admin approval before proceeding.

**Reclaim** — The background process that finds stale inflight jobs and reassigns them.

**Renew** — Refresh the lease timestamp for an inflight job (what Heartbeat calls).

**Run-binding** — Deriving tenant/agent identity from the trusted inflight assignment, ignoring runner headers.

**Runner** — Worker process that executes agent framework code.

**Runner Protocol** — Private gRPC contract (five RPCs) between runners and the control plane.

**runner_kind** — Which type of runner should execute this job (e.g., "python-langgraph").

**SSE** — Server-Sent Events. One-way server-to-client streaming over HTTP.

**Superseded** — The heartbeat response that tells a runner "you have been replaced, stop now."

**Tenant** — A flat string identifier that scopes all data for multi-tenancy.

**Thread** — A container for sequential runs (like a chat session).

**Transport** — The queue/event/cancel abstraction layer (Redis, NATS, Kafka, in-process).

---



# Appendix D: Reading Path

**Day 1:** Read Chapters 1-4. At the end, you should be able to explain: what the three processes are, how a run flows from client through the system and back, and which steps touch Postgres vs Redis.

**Day 2:** Read Chapters 5-8. At the end, you should be able to explain: thread/run statuses, how cancel crosses replicas, the three crash scenarios with generation fencing, and what needs sticky routing.

**Day 3:** Read Chapters 9-12. At the end, you should be able to explain: how agents delegate to each other without direct communication, the two kinds of HITL without confusing them, how governance works with examples, and what connectors do.

**Day 4:** Read Chapters 13-22. At the end, you should be able to explain: why rate limits need Redis in multi-replica, why serve refuses to start insecurely, what conformance tests prove, what our honest limitations are, and where any file in the codebase belongs.

**Verification exercise:** Draw the Chapter 4 sequence (client → CP → Redis → runner → CP → Redis → client) from memory on a whiteboard. Label which steps are Postgres, which are Redis. If you can do this, you understand the product.

---



# 23. Deep Dive — The Complete HA Run With Replica Names and IDs

The earlier chapter showed you the flow conceptually. Now let me retell it with specific replica names, specific IDs, and every Postgres/Redis touch labeled. This is the version you should be able to reproduce on a whiteboard from memory.

## Setup for this walkthrough

- Three CP replicas behind nginx: CP-1, CP-2, CP-3
- One Postgres database shared by all three
- One Redis instance shared by all three
- Two Python runners (Runner-A, Runner-B) connected via gRPC through the load balancer
- Client wants to run agent `research_bot` on thread `thr_42`



## The numbered sequence

**Step 1 — Client sends HTTP request.**

The client application opens an HTTPS connection to the load balancer (nginx). Nginx picks CP-1 for this connection using round-robin. CP-1 receives:

```
POST /threads/thr_42/runs HTTP/1.1
Authorization: Bearer eyJ...
Content-Type: application/json
{"agent_id": "research_bot", "input": {"messages": [{"role": "human", "content": "What is quantum computing?"}]}}
```

At this point only CP-1 knows about this request. CP-2 and CP-3 have no idea it happened.

**Step 2 — Auth middleware on CP-1.**

CP-1 validates the JWT token. It extracts: tenant_id = "acme", principal = "user_alice", permissions = ["read", "write"]. These go into the Go request context. If Redis-backed rate limiting is configured, CP-1 may also check a Redis counter here (one Redis round-trip for the global/user rate limit).

**Step 3 — createRunCtx on CP-1 (the Postgres-heavy part).**

Now CP-1 runs createRunCtx. Here is every database operation:

- Alias resolve: checks in-memory config. No DB call needed — aliases are loaded at boot from langgraph.json.
- GetAgent: SQL SELECT from `agents` table. One Postgres round-trip. Confirms `research_bot` exists for tenant `acme`.
- Rate limit (if agent-scoped): possibly another Redis round-trip to check agent-specific limits.
- Preflight/admission: SQL SELECT from `kill_switches` table — is tenant `acme` or agent `research_bot` killed? One Postgres round-trip. Also checks `agents:run` permission from the auth context.
- GetThread: SQL SELECT from `threads` table for `thr_42`. One Postgres round-trip.
- TryClaimThread: SQL UPDATE `threads SET status='busy' WHERE id='thr_42' AND status='idle'`. One Postgres round-trip. This is atomic — if another request was racing us, only one will succeed (the WHERE clause acts as a lock).
- CreateRun (or CreateRunAdmitted if limits configured): SQL INSERT into `runs` table with run_id = `run_9f`, status = `pending`, generation will be on the assignment. One Postgres round-trip (or two if admission_limits need a locked COUNT first).

Total Postgres round-trips in createRunCtx: approximately 5-6. Total Redis round-trips so far: 0-2 (only rate limit if configured).

After this, CP-1 has a run row in Postgres (`run_9f`, pending) and a thread row marked busy. Redis has NOT been involved for any correctness-critical state yet.

**Step 4 — Enqueue on CP-1 (the Redis part).**

CP-1 serializes the assignment into JSON:

```json
{"run_id": "run_9f", "thread_id": "thr_42", "graph_id": "research_bot", "generation": 1, "tenant_id": "acme", "input": {...}, "user": {"id": "user_alice"}, "connector_needs": ["github"], "stream_modes": ["values", "updates"], "trace_context": {"traceparent": "00-abc..."}}
```

CP-1 pushes this onto a Redis list keyed by runner_kind `python-langgraph`. One Redis round-trip.

Now the truth is split: Postgres says "run pending, thread busy" and Redis says "job ready in the python-langgraph queue." If CP-1 crashes right now, the run still exists in Postgres and Redis. Another replica can serve GET requests for the run, and a runner can still dequeue the job.

**Step 5 — HTTP response.**

If this was a background create, CP-1 returns `{"run_id": "run_9f", "status": "pending", ...}` immediately.

If this was a stream-on-create, CP-1 keeps the HTTP connection open and subscribes to the Redis event stream for `run_9f` so it can forward events as SSE frames.

**Step 6 — Runner GetJob hits CP-2.**

Runner-A has been sitting in a gRPC GetJob call. Its gRPC connection goes through the load balancer and happens to be connected to CP-2 (not CP-1, which created the run). This is fine — they share Redis.

CP-2 calls Redis Dequeue: atomic operation that moves `run_9f` from the ready list to an inflight hash/sorted-set with a timestamp (the lease start). One Redis round-trip.

Before returning the assignment to Runner-A, CP-2 does two things:

1. Subscribes to Redis Pub/Sub channel `cancel:run_9f` (so cancel signals can reach this runner later)
2. Checks `ctx.Err()` — if Runner-A disconnected between Dequeue and this moment, calls Nack to return the job to the queue

Then CP-2 returns the assignment JSON to Runner-A over gRPC.

**Step 7 — Runner-A starts executing.**

Runner-A parses the assignment. It starts:

- A heartbeat goroutine/task that calls gRPC Heartbeat every 2 seconds
- Loads the LangGraph graph for `research_bot`
- Calls astream() or equivalent to begin execution

**Step 8 — Heartbeats flow through the same CP as GetJob.**

Every 2 seconds, Runner-A calls Heartbeat with `run_id=run_9f, generation=1`. Because all of Runner-A's RPCs share the same gRPC channel (HTTP/2 multiplexing), heartbeats go to the SAME CP that served GetJob — in this example, CP-2. CP-2 calls `queue.Renew(run_9f, generation=1)` on the shared Redis inflight record. One Redis round-trip per heartbeat. The timestamp refreshes.

If the Renew discovers that generation 1 is no longer current (meaning reclaim happened and bumped to generation 2), it returns "not current" and the CP responds with `HeartbeatResponse{ok: true, superseded: true}`.

**Step 9 — StreamEvents flows to CP-2 (same as GetJob).**

As the LLM generates tokens, Runner-A sends them via StreamEvents. Its gRPC connection (long-lived, HTTP/2) goes to the same CP it is already connected to — CP-2. All RPCs share one gRPC channel, so GetJob, StreamEvents, Heartbeat, and WatchCancels all go to CP-2.

CP-2 receives each event and calls `EventBroker.Publish(run_9f, event_bytes)` which writes to a Redis Stream with key like `events:run_9f`. One Redis XADD per event.

For terminal events (the final success/error event), CP-2 also checks the generation: if the event claims to be terminal but the generation is stale (not matching the current inflight generation), the event is DROPPED before publish. This prevents a stale runner from publishing a false "done" event.

**Step 10 — SSE fan-out on CP-1.**

The client's SSE connection is still held by CP-1 (from the stream-on-create, or from a separate GET /runs/run_9f/stream that happened to hit CP-1). CP-1 is subscribed to the Redis Stream `events:run_9f` via XREAD (blocking read).

When CP-2 writes events to the Redis Stream, CP-1's XREAD goroutine picks them up. CP-1 reads the events and writes them as SSE frames to the client's HTTP connection:

```
data: {"event": "values", "data": {"messages": [...]}}

data: {"event": "values", "data": {"messages": [...]}}
```

The client sees tokens appearing in real-time.

**Step 11 — Run completes.**

Runner-A finishes execution. It calls `ReportStatus(run_id=run_9f, status="success", generation=1)`. This goes to CP-2 (same gRPC channel as all other RPCs).

CP-2 does:

1. `queue.Ack(run_9f, generation=1)` on Redis — verifies generation is current, removes inflight entry. If generation is stale, returns "superseded" and stops here.
2. Status callback: UPDATE runs SET status='success' WHERE id='run_9f' in Postgres. UPDATE threads SET status='idle' WHERE id='thr_42' in Postgres.
3. Publish terminal event to Redis Stream.
4. OTel span end.
5. Completion hooks fire (if any).

**Step 12 — Client sees completion.**

CP-1's SSE subscription receives the terminal event from Redis. It writes the final SSE frame and closes the connection:

```
data: {"event": "end"}

```

The client knows the run is done. It can GET /threads/thr_42/runs/run_9f for the full result.

## Teaching summary: what touched what


| Step                  | Process | Postgres                    | Redis                              |
| --------------------- | ------- | --------------------------- | ---------------------------------- |
| Create run            | CP-1    | claim thread + insert run   | rate limit (maybe)                 |
| Enqueue               | CP-1    | —                           | push to queue                      |
| GetJob                | CP-2    | —                           | dequeue + lease + cancel subscribe |
| Heartbeat (x many)    | CP-2    | —                           | renew lease timestamp              |
| StreamEvents (x many) | CP-2    | —                           | XADD events                        |
| SSE delivery          | CP-1    | —                           | XREAD events                       |
| ReportStatus          | CP-2    | update run + release thread | Ack (remove inflight)              |




## What if CP-1 dies after enqueue but before the client gets a response?

The run already exists in Postgres and the job is in Redis. The runner will still pick it up and execute it. The client's HTTP connection dies (they get a network error). They can reconnect to any replica, GET the run status, and see it progressing. If they open a new SSE connection on CP-3, they will receive events from Redis (including replayed events they missed). The system self-heals without manual intervention.

## What if the runner never picks up the job?

The job sits in Redis forever. No heartbeats arrive because no runner took it. Eventually the reclaim loop checks... but wait, reclaim only checks INFLIGHT jobs (ones that were dequeued). A job sitting in the READY queue is not inflight — it is just waiting. This is a queue depth problem, not a reclaim problem. Operators should alert on growing queue depth (the Prometheus gauge). If all runners of a given kind are dead, jobs pile up until runners come back.

## What if Redis dies mid-run?

StreamEvents publishes fail. Heartbeat Renews fail. The run may error out because the runner cannot stream events. Reclaim also cannot check leases. This is a shared dependency failure — Redis must be treated with the same operational seriousness as Postgres on the Supported tier. Redis is not optional infrastructure in production.

---



# 24. Deep Dive — Postgres vs Redis: Why Both and What Each Holds

People sometimes ask: "isn't writing both Postgres and Redis a dual-write bug waiting to happen?" The answer is no, because they store different concerns that serve different purposes.

## What Postgres holds (the durable truth)

- Agent definitions (what agents exist, their config)
- Thread records (id, tenant, status)
- Run records (id, thread, agent, status, input, output, timestamps)
- Cron claims (which schedule fires were handled)
- Policy grants (admin-created overlays)
- Audit events (every policy decision ever made)
- Kill switches, break-glass windows, mandatory HITL rules
- Pending actions (connector tool calls waiting for approval)
- Store items (key-value data for agents)
- Checkpoint metadata (proxy mode)
- Registry entries (agent cards)

If Postgres dies and you restore from backup, you have the complete history of what happened. Nothing critical is lost.

## What Redis holds (the coordination machinery)

- Job queue (pending assignments waiting for runners)
- Inflight leases (which jobs are being worked on, with timestamps)
- Event streams (the real-time token stream for SSE fan-out)
- Cancel pub/sub channels (ephemeral signals)
- Rate limit counters (sliding windows)
- Admin session store (when configured)
- Reclaim leader lock (which replica runs reclaim)
- Queue depth cache (for metrics)

If Redis dies and comes back empty, you lose: in-progress event streams (clients must reconnect), pending queue jobs (runs stay "pending" in Postgres but nobody picks them up until re-enqueue or manual intervention), rate limit state (limits reset). You do NOT lose: run history, thread state, agent config, audit trail, governance config.

## How crash windows are closed

The concern with dual writes is: "what if I write Postgres but Redis fails, or vice versa?" Here is how each case is handled:

**CreateRun succeeds in Postgres, then Enqueue to Redis fails:**
The handler calls `rollbackCreatedRun` which marks the run as errored and releases the thread. The user sees an error. No zombie stuck-busy thread.

**Enqueue succeeds, but runner never completes (crash):**
Heartbeats stop, reclaim fires, job is requeued with a new generation. Eventually a runner succeeds and updates Postgres. The run goes from pending to success via the normal path — just slower.

**Runner completes work but ReportStatus fails to reach the CP:**
From the runner's perspective, it is done. Heartbeats stop (because the runner stopped working). Reclaim fires, bumps generation, requeues. A second runner gets the job and may redo work (this is why tool idempotency matters). Eventually one attempt's ReportStatus succeeds and Postgres updates.

The key insight: there is no distributed transaction across Postgres and Redis. Instead, the system uses lease timeouts and reclaim as the self-healing mechanism. Brief inconsistency windows exist but are bounded by the ~6 second reclaim cycle.

---



# 25. Deep Dive — All Failure Scenarios You Should Mentally Simulate

Here are twenty scenarios to think through. For each one, trace what happens step by step using what you now know about the system.

## Scenario 1: Client typos the agent_id

Client sends `{"agent_id": "reserch_bot"}` (typo). createRunCtx calls GetAgent, which does a Postgres SELECT — no row found. Returns 404 to the client. Thread was never claimed. Nothing needs cleanup.

## Scenario 2: Two clients create runs on the same thread simultaneously

Client A and Client B both send POST /threads/thr_42/runs at the same time. Both hit different CP replicas (CP-1 and CP-2). Both enter createRunCtx. Both get past GetAgent and rate limits. Both try TryClaimThread: `UPDATE threads SET status='busy' WHERE id='thr_42' AND status='idle'`. Only ONE can succeed (the WHERE clause is atomic in SQL). The other gets zero rows affected, which means the claim failed — it returns 409 Conflict to its client. The winner proceeds normally.

## Scenario 3: Cache hit — run already completed with same input

If caching is enabled and the exact same (thread, agent, input, config) combination was already completed recently, createRunCtx returns a cached result with nil assignment. The handler returns the cached run to the client immediately. No enqueue. No runner work. The thread is not even claimed (because no run is being created).

## Scenario 4: Enqueue fails (Redis is down)

createRunCtx succeeded — the run row exists in Postgres with status pending, thread is busy. Then the handler tries to push to Redis and fails. The handler calls `rollbackCreatedRun`: UPDATE run status to error with message "enqueue failed", UPDATE thread status back to idle, decrement ActiveRuns prometheus counter. The client gets a 500 error. Without this rollback, the thread would be stuck busy forever with a run that will never execute.

## Scenario 5: Runner never starts (crashes between GetJob and execution)

The job was dequeued from Redis into inflight. The runner dies. Two recovery paths:

- Fast path: gRPC keepalive detects the dead connection in ~4 seconds. The GetJob handler code had a deferred Nack that fires on context cancellation — job goes back to ready queue immediately.
- Slow path: if the fast Nack misses (rare race), the inflight lease just sits there. After ~6 seconds, reclaim sees it is stale, bumps generation, requeues. Another runner gets it.



## Scenario 6: Runner paused mid-execution (docker pause)

The runner cannot send heartbeats because the entire process is frozen. After ~6 seconds of staleness, reclaim fires. Generation bumps from 1 to 2. Job requeued. Runner-B picks it up with generation 2. Runner-B completes successfully.

When the original runner is unpaused, its next heartbeat sends generation=1. The CP responds with superseded=true. The runner cancels local execution and stops. If it tries ReportStatus, that is also rejected. Final result: success from Runner-B.

## Scenario 7: Client cancels a running job

Client calls cancel on CP-A. CP-A publishes cancel signal to Redis Pub/Sub channel `cancel:run_9f`. CP-B (which subscribed during GetJob) receives the signal. CP-B's goroutine calls notifyWatchers for that run. The runner's WatchCancels stream on CP-B receives the signal. Runner stops execution, reports status interrupted. Thread returns to idle.

## Scenario 8: Policy webhook denies a connector session

During execution, the runner tries to mint a connector session (POST /internal/connectors/salesforce/session). The CP runs Decide: the sync webhook returns `{"effect": "deny", "reason": "agent not authorized for salesforce"}`. The CP returns an error to the runner. The runner cannot access salesforce. The agent code must handle this error — typically it tells the user "I cannot access salesforce" and finishes the run with that information.

## Scenario 9: Policy returns pending for a tool call

The agent tries to call `transfer_funds` on the banking connector. The webhook returns `{"effect": "pending"}`. The CP stores a pending_actions row in Postgres. It returns a JSON-RPC error to the runner: `{"error": {"code": -32000, "data": {"reason_code": "policy_pending", "action_id": "act_xyz"}}}`. The agent code receives this error. A well-written agent would tell the user "Waiting for approval on the transfer" and keep the run alive (or error gracefully). An admin sees the pending action in the Admin UI, reviews it, and approves. The agent must RETRY the same tool call to consume the one-shot approval.

## Scenario 10: Mandatory HITL overrides a webhook allow

The webhook says allow for `transfer_funds`. But a mandatory HITL rule exists for `(tenant=acme, connector=bank, tool=transfer_funds)`. The mandatory rule OVERRIDES the webhook allow to pending. Same pending flow as Scenario 9. This exists as defense-in-depth: even if the webhook PDP is compromised and allows everything, mandatory HITL still blocks.

## Scenario 11: Break-glass is active

An admin minted a break-glass window for tenant acme (2 hours, reason: "PDP outage"). During this window, all connector calls from tenant acme skip Decide entirely. The call proceeds to the upstream directly. But every skipped decision writes an audit record: `{decision: "allow", reason_code: "break_glass", ...}`. After the window expires, normal Decide enforcement resumes.

If the audit write FAILS (Postgres is down), the break-glass itself fails. The system says: "I cannot prove I audited this bypass, so I will not bypass." This is fail-closed audit — you cannot get invisible emergency access.

## Scenario 12: Kill switch activated for a running agent

Admin activates kill switch for agent `bad_bot`. Two things happen:

1. All NEW createRunCtx calls for `bad_bot` immediately fail at the kill/pause check (403).
2. If not pause-only: the CP searches for all pending/running runs of `bad_bot`, paginates through them, and cancels each one (publish cancel signals). This drain ensures existing work stops, not just future work.



## Scenario 13: Alias routes to a deleted agent

Alias `support_bot` is configured to route 100% to `support_bot_v3`. But someone deleted `support_bot_v3` from the agents config and restarted. Now every request for `support_bot` resolves the alias to `support_bot_v3`, then GetAgent fails with 404. Every create fails. Fix: update the alias config and restart.

## Scenario 14: A2A depth exceeded

Agent A (depth 0) calls Agent B (depth 1) which calls Agent C (depth 2) which tries to call Agent D. If max_depth is 3, Agent D would be depth 3 — at the limit. If max_depth is 2, the create for Agent C would already have been rejected. The rejection happens in createRunCtx before any enqueue, so no half-created run exists.

## Scenario 15: A2A breadth race

Agent A (parent) tries to spawn 20 children simultaneously (fan-out). max_breadth is 10. Ideally only 10 should succeed. But the breadth check is: search children count, then create if under limit. With 20 concurrent requests, many might read count=0 (all at the same moment before any inserts land), all pass the check, all create. Result: more than 10 children. This is the documented race. In practice, it is bounded — steady state (non-simultaneous) respects the limit. The race only matters during bursts.

## Scenario 16: Rate limit memory backend with 3 pods

Each pod has its own in-memory bucket set to 100 req/s. A client round-robining across 3 pods can do 300 req/s total without being limited. This is a misconfiguration. Fix: set REDIS_URL so all pods share one bucket. Then the cluster-wide limit is correctly 100 req/s.

## Scenario 17: Rate limit Redis briefly unreachable

Redis goes down for 3 seconds. During those 3 seconds, rate limit checks fail. The system ALLOWS the requests (fail-open). Rationale: a brief coordination blip should not kill the entire API. After Redis comes back, limits resume normally. Some requests that should have been limited got through — this is accepted as an availability trade-off.

## Scenario 18: serve without auth configured

Someone runs `runkite serve` on a server without setting up client auth in langgraph.json. The serve admission check detects: no client auth configured. It prints a clear error message and exits with status 1. The process never starts listening. Kubernetes readiness probe fails. No traffic is routed. The fix is to configure auth or set RUNKITE_ALLOW_INSECURE_SERVE=1.

## Scenario 19: SSE on a different replica than the one that created the run

Client creates a run on CP-1 (background create, gets pending response). Client then opens SSE: GET /threads/thr_42/runs/run_9f/stream — this request goes through the load balancer and hits CP-3 (different replica). CP-3 does not know about this run from its own memory, but it subscribes to Redis Stream `events:run_9f`. When events are published (by whatever CP receives StreamEvents from the runner), CP-3 reads them and forwards to the client. Works perfectly because Redis is the shared event bus.

## Scenario 20: /mcp without sticky routing

Client opens MCP session on CP-1. Gets a session token. Next MCP request goes through the load balancer and hits CP-2. CP-2 has no record of this session (it is in CP-1's process memory). The MCP call fails or returns an error. Fix: configure your load balancer with cookie-based sticky sessions for the `/mcp` path prefix.

---



# 26. Deep Dive — Tenancy: How Multi-Tenant Isolation Works



## What a tenant is

A tenant is just a string identifier (like "acme" or "customer_123") that scopes all data. It is NOT a workspace/org/team hierarchy — it is a flat string. There is no tenant registry UI; tenants implicitly exist when resources are tagged with them.

## Where tenant enters the system

When a client makes a request, the authentication middleware determines the tenant. The JWT or API key maps to a tenant_id. This goes into the Go request context and scopes every database query downstream. A thread belongs to a tenant. A run inherits from its thread. Searches are always scoped: you cannot see another tenant's threads.

## How runners interact with tenancy

Runners authenticate with runner tokens (RUNNER_TOKEN_PYTHON_LANGGRAPH etc.). These tokens identify the runner kind but do not inherently identify a tenant. The assignment that GetJob returns includes the tenant_id (set by createRunCtx from the client's auth context).

For proxy paths (/internal/connectors/*, /internal/store/*, /internal/vectors/*), the runner sends headers claiming a run_id. Run-binding derives the tenant from the INFLIGHT ASSIGNMENT (trusted, written by createRunCtx) rather than from any header the runner sends. This is the critical security boundary: even if a runner sends `X-Runkite-Tenant-Id: evil_tenant`, the CP ignores it and uses the assignment's tenant.

## RUNNER_TENANTS allowlists

For additional safety, you can configure `RUNNER_TENANTS_PYTHON_LANGGRAPH=acme,beta` which means runners of that kind can only execute work for tenants acme and beta. If a job for tenant "gamma" is queued for that runner_kind, it will be rejected at dispatch time. This limits blast radius of a compromised runner token.

Without RUNNER_TENANTS configured, a leaked runner token of a given kind could be used to poll jobs for ANY tenant that uses that runner_kind. The control plane emits a startup warning when RUNNER_TENANTS is not set in the presence of auth configuration.

## Direct-mode trust residual

In direct mode, runners connect directly to Postgres for LangGraph checkpoints. This means the runner process holds database credentials. In a multi-tenant setup, a compromised runner could read/write checkpoints for ANY tenant (they all share one database). For multi-tenant production, prefer proxy mode (checkpoints go through the CP's store API) or accept the trust boundary and isolate tenants at the Kubernetes namespace level.

---



# 27. Deep Dive — Authentication: Four Boundaries

There are four separate authentication boundaries in Runkite. Do not conflate them during incident response.

## Boundary 1: Client authentication

Who: Your application (the Agent Protocol client)
How: JWT validation, API key lookup, or custom auth webhook
What it determines: tenant_id, principal identity, permissions (read/write/admin, optionally agents:id:run)
Where: `internal/auth/` middleware on HTTP routes
Failure: 401 Unauthorized

When `strict_permissions` is enabled and a token has an EMPTY permissions set, it is treated as "no permissions at all" (fail-closed), not "unrestricted" (the old buggy behavior).

## Boundary 2: Admin authentication

Who: Operators accessing the Admin UI or admin API
How: Admin API keys (ADMIN_KEYS env/config) or admin session cookies
What it determines: admin-level access to governance routes
Where: `internal/auth/adminsession*.go`, admin middleware
Special: CSRF token required for cookie-based mutations (prevents cross-site request forgery in browsers). Machine clients using Authorization: Bearer are not subject to CSRF (they do not have ambient cookies).

## Boundary 3: Runner authentication

Who: Runner processes connecting via gRPC
How: RUNNER_TOKEN_* environment variables, matched by runner_kind
What it determines: "this process is an authorized runner of this kind"
Where: `internal/bridge/interceptors*.go` (gRPC unary and stream interceptors)
Rotation: comma-separated allowlist so old and new tokens coexist during rolling deploy
Failure: gRPC UNAUTHENTICATED

In dev mode (no RUNNER_TOKEN_* set), runner auth is disabled and any process can claim to be a runner. This is intentional for local development only.

## Boundary 4: Run-binding (derived identity)

Who: Runners making HTTP calls to /internal/* paths
How: Look up inflight assignment by run_id + generation in Redis, derive identity from it
What it determines: tenant_id, agent_id, user context for connector/store/vector operations
Where: `internal/auth/runbind.go`
Why it exists: runner-supplied headers cannot be trusted; only the assignment (written by the trusted createRunCtx path) is authoritative

If run-binding lookup fails (run not inflight, generation mismatch, Redis error), the request is denied. This prevents a runner from accessing connectors outside of active run execution.

---



# 28. Deep Dive — Connector Session Tokens: Why Mint at Use Time

A question that often comes up: why not put the connector session token into the RunAssignment at enqueue time so the runner has it immediately?

Three reasons:

**Reason 1: Queue wait time.** A job might sit in the Redis queue for minutes (or longer during capacity shortages). Connector session tokens have a 15-minute absolute TTL. If you mint at enqueue and the runner starts 20 minutes later, the token is already expired. Minting at use time means the token is always fresh.

**Reason 2: Policy should be checked at use time.** Between enqueue and actual connector use, policy grants may have changed. An admin might have revoked access. If you pre-authorized at enqueue time, the revocation would not take effect until the next run. Minting at use time means every session check reflects current policy.

**Reason 3: Credentials in Redis.** Putting real credentials (or tokens derived from them) into the Redis queue means anyone who can read Redis can access upstream services. The design keeps secrets in the control plane's memory only, never serialized to the queue. The runner only gets a short-lived session token at the moment it needs it.

The runner helpers in Python (`connectors.py`) handle this: they call GET session at use time, cache the token for the run's lifetime, and re-mint once if they get a 401 (token expired mid-run because the run took longer than 15 minutes).

---



# 29. Deep Dive — The Reclaim Leader Lock



## Why only one replica should run reclaim

The reclaim loop scans for stale inflight jobs and requeues them. If every replica did this independently and simultaneously, you could get:

- CP-1 sees job X stale, bumps to generation 2, requeues
- CP-2 sees job X stale at the same moment, bumps to generation 3, requeues AGAIN
- Now two copies of the same job are in the ready queue with different generations

For Redis, the Dequeue and Reclaim operations are designed to be atomic (using Lua scripts), so this double-reclaim is largely prevented at the Redis level. But for Kafka (where such atomicity is harder), having a single reclaim leader is essential.

## How the lock works

One replica holds a Redis key (`rk:reclaim-leader`) with a TTL of about 3 seconds. Every 2 seconds, the holder refreshes the TTL. Other replicas attempt to acquire the key on each tick — if it already exists (not expired), they skip reclaim for that tick.

If the leader dies, its key expires in 3 seconds. Another replica's next tick successfully acquires the key and becomes the new leader. Failover is automatic within one tick cycle.

## What happens if Redis is unreachable for the lock check

If the lock check itself fails (Redis error), the replica SKIPS reclaim for that tick (fail-closed on the lock). This prevents potential double-reclaim under Redis flakiness. The reclaim will simply not run for a cycle — stale jobs survive one extra tick period, which is acceptable.

---



# 30. Deep Dive — Wire Log: What Messages Actually Look Like on the Network



## Agent Protocol: HTTP create run request and response

```
>>> Request
POST /threads/thr_42/runs HTTP/1.1
Host: runkite.internal:2026
Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyX2FsaWNlIiwidGVuYW50IjoiYWNtZSIsInBlcm1zIjpbInJlYWQiLCJ3cml0ZSJdfQ.xxx
Content-Type: application/json

{"agent_id":"research_bot","input":{"messages":[{"role":"human","content":"What is quantum computing?"}]},"stream_mode":["values"]}

<<< Response (background create)
HTTP/1.1 200 OK
Content-Type: application/json

{"run_id":"run_9f","thread_id":"thr_42","agent_id":"research_bot","status":"pending","created_at":"2026-08-23T10:00:00Z","metadata":{"requested_alias":null}}
```



## Agent Protocol: SSE stream

```
>>> Request
GET /threads/thr_42/runs/run_9f/stream HTTP/1.1
Host: runkite.internal:2026
Authorization: Bearer eyJ...
Accept: text/event-stream

<<< Response (streaming, connection held open)
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

event: metadata
data: {"run_id":"run_9f"}

event: values
data: {"messages":[{"role":"assistant","content":"Quantum computing is"}]}

event: values
data: {"messages":[{"role":"assistant","content":"Quantum computing is a type of computation"}]}

... more events ...

event: end
data: {}
```



## Runner Protocol: GetJob (gRPC, shown as conceptual JSON)

```
>>> Runner sends (blocks until work available):
GetJobRequest {
  runner_kind: "python-langgraph"
}

<<< Control plane responds (when work is available):
GetJobResponse {
  assignment_json: "{"run_id":"run_9f","thread_id":"thr_42","graph_id":"research_bot","generation":1,"tenant_id":"acme","input":{...},"stream_modes":["values"],"connector_needs":["github"],"user":{"id":"user_alice"},"trace_context":{"traceparent":"00-abc..."}}"
}
```



## Runner Protocol: Heartbeat (gRPC)

```
>>> Runner sends (every 2 seconds):
HeartbeatRequest {
  run_id: "run_9f"
  generation: 1
}

<<< Control plane responds:
HeartbeatResponse {
  ok: true
  superseded: false
}
```

If superseded:

```
HeartbeatResponse {
  ok: true
  superseded: true
}
```

Note: `ok` is almost always true — even when superseded. The runner must check BOTH fields. A runner that only checks `ok` and ignores `superseded` will keep burning resources.

## Runner Protocol: StreamEvents (gRPC client-streaming)

```
>>> Runner sends (many messages over one stream):
RunEventProto { run_id: "run_9f", generation: 1, event_json: "{"type":"values","data":{...}}" }
RunEventProto { run_id: "run_9f", generation: 1, event_json: "{"type":"values","data":{...}}" }
... more events ...
RunEventProto { run_id: "run_9f", generation: 1, event_json: "{"type":"end","data":{}}" }

<<< Control plane responds (after stream closes):
StreamEventsResponse { ok: true }
```



## Runner Protocol: ReportStatus (gRPC)

```
>>> Runner sends:
ReportStatusRequest {
  run_id: "run_9f"
  status: "success"
  generation: 1
}

<<< Control plane responds:
ReportStatusResponse {
  ok: true
  superseded: false
}
```



## Connector proxy: MCP tools/call

```
>>> Runner sends HTTP to control plane:
POST /internal/connectors/github/mcp HTTP/1.1
Authorization: Bearer <runner_token>
X-Runkite-Run-Id: run_9f
X-Runkite-Generation: 1
X-Runkite-Connector-Session: sess_abc123
Content-Type: application/json

{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_repos","arguments":{"org":"acme"}}}

<<< Control plane responds (after policy check + upstream forward):
HTTP/1.1 200 OK
Content-Type: application/json

{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"[{"name":"repo1"},{"name":"repo2"}]"}]}}
```

If policy denies:

```
HTTP/1.1 200 OK
Content-Type: application/json

{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"policy denied","data":{"reason_code":"policy_webhook_deny","reason":"agent not authorized for list_repos"}}}
```



## Policy webhook: what the PDP receives

```
>>> Runkite sends to your webhook:
POST /decide HTTP/1.1
Content-Type: application/json
X-Runkite-Signature: sha256=abc123...

{"type":"policy.decide","stage":"tool.call","tenant_id":"acme","agent_id":"research_bot","run_id":"run_9f","generation":1,"connector":"github","tool":"delete_repo","timestamp":"2026-08-23T10:01:00Z","data":{"connector":"github","tool":"delete_repo","identity":"user_alice"}}

<<< Your PDP responds:
HTTP/1.1 200 OK
Content-Type: application/json

{"effect":"deny","reason":"deletion not permitted","reason_code":"dangerous_operation"}
```

---



# 31. Deep Dive — Alias Routing for A/B Testing



## What aliases do

An alias lets you give clients a stable name (like `support_bot`) that internally routes to one or more real agents with configurable weights. This enables A/B testing, canary deployments, and gradual rollouts without changing client code.

## Configuration example

```json
"agent_aliases": {
  "support_bot": {
    "targets": [
      {"agent_id": "support_bot_v1", "weight": 90},
      {"agent_id": "support_bot_v2", "weight": 10}
    ]
  }
}
```

This means: 90% of requests for `support_bot` go to `support_bot_v1`, 10% go to `support_bot_v2`.

## How resolution works

When createRunCtx receives agent_id = "support_bot", the alias resolver checks: is this name in the alias config? If yes, it generates a random number between 0 and total weight (100), walks the targets in sorted order, and picks the one whose cumulative weight range contains the random number. The targets are sorted by agent_id string (alphabetical) to ensure deterministic behavior regardless of Go map iteration order.

After resolution, the run proceeds with the REAL agent_id (e.g., `support_bot_v2`). The original alias name is stored in run metadata as `requested_alias` so you can later query: "what percentage of alias traffic went to v2?"

## Collision dangers

**Collision 1:** If your alias name is the same as a real agent_id, the alias resolver intercepts it first. The real agent with that exact name becomes unreachable by that name. Keep alias names and agent_ids in separate namespaces.

**Collision 2:** If a target agent does not exist (was deleted), GetAgent fails after resolution. Every request for that alias fails until the config is fixed.

**What aliases do NOT provide:** Sticky per-user routing. Each request is independently random. If you need "user X always sees v2 during the experiment," you need to implement that at your gateway level (hash user_id to a target), not in alias.go.

## Relationship to rate limits and metrics

Rate limits and metrics key off the RESOLVED agent_id (v1 or v2), not the alias name. If you set a rate limit for `support_bot`, it applies to the alias name at the resolution step (before GetAgent). After resolution, agent-scoped limits on `support_bot_v2` also apply independently.

**Code:** `internal/api/alias.go`

---



# 32. Deep Dive — The Store: How Agents Persist Data Between Runs



## What the store is

The store is a simple key-value API that agents can use to save and retrieve data across runs. Think of it like a per-tenant database that agents can read/write during execution. Common uses: saving user preferences, caching intermediate results, storing structured data between conversation turns.

## Two modes of access

**Direct mode:** The runner has a direct database connection (POSTGRES_DSN set on the runner). The Python RunkiteStore class reads/writes the `store_items` table directly via SQL.

**Proxy mode:** The runner has no database connection. It calls the control plane's `/internal/store/`* HTTP API. The CP performs the database operation on the runner's behalf. Run-binding ensures tenant isolation.

Both modes read/write the same Postgres table (`store_items`). The data is the same regardless of access mode. Direct mode is faster (no HTTP round-trip to CP) but means the runner holds database credentials (trust boundary concern for multi-tenant).

## TTL support

The Python store supports time-to-live: put a value with `ttl=60` (minutes) and it automatically expires. Reads can optionally refresh the TTL. Background retention cleanup deletes expired items. TypeScript runners may not have TTL support (framework-level limitation, not a Runkite limitation).

---



# 33. Deep Dive — Checkpoints: How Agent State Survives Across Runs



## What checkpoints are

When a LangGraph agent hits an interrupt or completes a run, it saves its internal graph state (called a "checkpoint"). When the user sends a resume or a new message on the same thread, the agent loads the checkpoint and continues from where it left off. This is what makes multi-turn conversations work: the agent "remembers" previous turns.

## Two modes

**Direct mode (Supported on Postgres):** The runner connects to the same Postgres database as the CP using `AsyncPostgresSaver`. Checkpoints go into LangGraph-specific tables (separate from Runkite's own tables, but same database). This is fast and reliable.

**Proxy mode:** Checkpoints go through the CP's proxy API. Less common in practice.

## The tenant prefix

For non-default tenants, checkpoints are keyed with `{tenant_id}:{thread_id}` instead of just `thread_id`. This prevents cross-tenant checkpoint access even in direct mode where the runner sees all rows.

## Why direct mode is a trust boundary concern

In direct mode, the runner holds the Postgres connection string. If the runner is compromised, it can read/write checkpoints for ANY tenant (it is just SQL with a known schema). For single-tenant deployments this is fine. For multi-tenant production, you either accept this risk, isolate tenants at the infrastructure level, or use proxy mode.

---



# 34. Deep Dive — Vector Stores



## What they are

Vector stores let agents do semantic search. If your agent needs to answer questions based on a large document corpus (RAG — Retrieval Augmented Generation), it stores text as high-dimensional vectors (embeddings) and searches for similar content by comparing vector distances. Think of it like a very smart index: instead of matching exact keywords, it finds content that is conceptually similar.

## Backends and when to choose each

**pgvector (Supported on Postgres):** Vectors stored inside a Postgres extension. This is the simplest option if you already run Postgres (which you do, for Runkite state). No additional infrastructure. Good for up to a few million vectors. Beyond that, specialized vector databases scale better.

**Qdrant (Compatible):** A purpose-built vector database. Choose this when you have tens of millions+ vectors, need filtered search (combine semantic + metadata filters), or want horizontal scaling for vector queries. Requires running a separate Qdrant cluster.

**Weaviate (Compatible):** Similar to Qdrant, graph-oriented. Choose if your team already runs Weaviate or needs its specific hybrid search capabilities.

**Pinecone (Compatible):** A managed SaaS vector database. Choose for zero-ops: no infrastructure to run. But data leaves your network (not self-hosted). For organizations that require all data stay on-premises, this is not suitable.

## Access paths (how runners reach vectors)

**Proxy mode (recommended for multi-tenant):** Runners call `/internal/vectors/`* HTTP endpoints on the control plane. These paths are run-bound (see Chapter 12 on run-binding): the control plane derives the tenant from the inflight assignment, not from runner-supplied headers. A compromised runner cannot access another tenant's vectors.

**Direct mode:** Runners have their own connection to the vector database and use it directly (the Python `vectorstore.py` wraps the vector client library). Faster (no HTTP round-trip through CP) but the runner holds credentials and can see all tenants' data. Same trust boundary concern as direct-mode checkpoints.

## Conformance testing

Like state backends and transport backends, vector stores have a conformance test suite (`internal/vectorstore/conformance/`). It verifies: insert, search by similarity, metadata filtering, deletion, and namespace isolation all work correctly regardless of which backend is plugged in.

## Configuration

In `langgraph.json`, vector backends are specified per-agent under `vectorstore_needs` (similar to `connector_needs`). The control plane bootstraps the connection at startup and the runner receives connection details in its assignment or uses the proxy path.

**Code:** `internal/vectorstore/` (Go interfaces + backends), `python/runkite_runner/vectorstore.py`, `typescript/runkite-runner/src/vectorstore.ts`, `docs/vector-store.md`

---



# 35. Deep Dive — Custom Routes



## What problem they solve

Sometimes your agent product needs HTTP endpoints that are NOT part of the Agent Protocol. Examples: a webhook endpoint that receives events from Stripe, a custom REST API that your frontend calls for agent-specific dashboards, or a health check endpoint specific to your agent's domain. These endpoints live in your runner code (a FastAPI app, an Express server) but need to be reachable through the control plane — because the control plane is the only thing exposed to the internet.

## How they work

Custom routes are a reverse proxy. You configure a mount path (default: `/custom`) and a target (where to forward requests — usually a port on the runner or a sidecar). When a request hits `https://runkite.company.com/custom/anything`, the control plane:

1. **Authenticates the request** through the same auth middleware as every other endpoint (JWT, API key, etc.)
2. **Strips all** `X-Runkite-`* **headers** from the incoming request. This is the anti-spoofing step: the client cannot inject fake identity headers.
3. **Injects trusted identity headers** from the authenticated context:
  - `X-Runkite-Identity` — who the caller is (e.g., "user_alice")
  - `X-Runkite-Tenant-Id` — which tenant they belong to
  - `X-Runkite-Permissions` — what they can do (read, write, admin)
  - `X-Runkite-Display-Name` — human-readable name
  - `X-Runkite-User` — all of the above as one JSON object
4. **Forwards the request** to the configured target with the mount prefix stripped (so `/custom/ping` becomes `/ping` at the target)

Your custom app trusts these headers as the source of truth for identity. It does not need to re-validate JWTs or API keys — the control plane already did that.

## Reserved mount paths

You cannot mount custom routes at paths that collide with Agent Protocol or platform routes: `/threads`, `/runs`, `/agents`, `/store`, `/admin`, `/internal`, `/health`, etc. The control plane validates this at startup.

## In-runner vs sidecar

**In-runner mode:** Your Python runner has a FastAPI/ASGI app defined in a separate file (like `app.py`). The runner process serves it on a port. The CP proxies to `http://runner:8001/`. This is the common case for agent-specific APIs.

**Sidecar mode:** A separate container/process hosts the custom API. The CP proxies to whatever URL you configure. Use this when the custom API is a different technology stack or needs independent scaling.

## Configuration

In `langgraph.json`:

```json
{
  "custom_routes": {
    "mount": "/custom",
    "target": "http://localhost:8001"
  }
}
```



## Example: `/custom/whoami`

With auth configured, a client sends: `GET /custom/whoami` with `Authorization: Bearer <jwt>`. The CP validates the JWT, injects `X-Runkite-Identity: user_alice`, and forwards to the runner's `/whoami` endpoint. The runner reads the header and responds with `{"authenticated": true, "identity": "user_alice"}`.

**Code:** `internal/customroutes/proxy.go`, `examples/custom_routes_agent/app.py`, runner helpers in `python/runkite_runner/custom_app.py` and `custom_auth.py`

---



# 36. Deep Dive — How Policy Overlay Sync Works Across Replicas

When an admin creates a grant in the Admin UI, here is exactly what happens:

1. The admin's POST hits some CP replica (say CP-2).
2. CP-2 inserts a row into `policy_grants` table in Postgres.
3. CP-2's in-memory policy engine immediately reloads (it has direct access to the change).
4. CP-1 and CP-3 do NOT know about this grant yet.
5. Every ~15 seconds, each replica runs a fingerprint poll (in `cmd/policy_overlay_poll.go`): it queries Postgres for a hash/count of all grants. If the fingerprint differs from its local cache, it reloads the full grant set from Postgres.
6. Within 15 seconds, CP-1 and CP-3 discover the new grant and reload.

During those 15 seconds, if a request hits CP-1 for the exact scenario covered by the new grant, CP-1 will still use the OLD policy (without the new grant). This is a brief inconsistency window. It is accepted because the alternative (a distributed lock on every Decide call to ensure all replicas agree) would add unacceptable latency to every connector operation.

---



# 37. Deep Dive — Circuit Breakers for Connectors



## What they do

If an upstream service (like Salesforce) is down, every MCP call to that connector will timeout and fail. Without a circuit breaker, every tool call attempt waits for the full timeout before failing — hammering the dead upstream and wasting runner time.

The circuit breaker tracks recent failures for each connector. When failures exceed a threshold, the breaker "opens" — subsequent calls are rejected immediately without contacting the upstream. After a cooldown, the breaker allows one probe call through. If it succeeds, the breaker closes (normal operation resumes). If it fails, the breaker stays open.

## Critical distinction: policy denials are NOT failures

A policy denial (Decide returns deny) is NOT an upstream failure. The upstream might be perfectly healthy — the control plane simply refused to forward the call. If policy denials opened the circuit breaker, a strict grant configuration would make connectors appear "down" — which is absurd. Policy denials and upstream failures are tracked separately.

---



# 38. Deep Dive — The Hooks System (Lifecycle Events, Preflight Gates, and Dead Letters)

Runkite has a complete hooks system that is easy to confuse with other "webhook" concepts in the codebase. Let me untangle the three separate webhook-related features:

## The three webhook concepts (do not confuse them)

1. **Policy webhook (Chapter 11)** — A sync HTTP call to YOUR external policy decision point (OPA, Cedar, custom ABAC) during a Decide evaluation. This is the policy engine consulting an external authority. Lives in `internal/policy/webhook.go`.
2. **Auth webhook** — A sync HTTP call to validate an auth token against your custom auth service. Used when neither JWT nor API keys are appropriate for your setup. Lives in `internal/auth/webhook.go`.
3. **Lifecycle hooks (THIS chapter)** — The platform's own event notification system: fire HTTP notifications when interesting things happen during a run. Lives in `internal/hooks/`.

This chapter is about #3. Do not confuse it with #1 or #2.

## Observational hooks (async, never block anything)

These fire AFTER something happens. They are purely informational — a way to push events to your own systems (Slack notifications, custom dashboards, SIEM ingestion, billing meters, etc.).

Event types that fire observational hooks:

- `run_start` — A run was created and dispatched
- `run_complete` — A run finished (success, error, interrupted, timeout)
- `tool_call` — A connector tool was called (fired from the policy layer)
- `error` — An error occurred during run processing
- `interrupt` — A LangGraph interrupt happened
- `policy_decision` — A policy Decide was evaluated (useful for SIEM/audit export)

How they work: When the event occurs, the control plane puts a delivery job into a bounded worker pool (20 workers, queue of 500). A worker picks it up and sends an HTTP POST to your configured webhook URL with a JSON payload containing the event data, signed with HMAC if you configured a secret.

**Key property: they never slow down the run.** If the webhook target is slow or unreachable, the delivery job fails quietly (logged, possibly dead-lettered). The run itself continues unaffected.

## Preflight hooks (sync, CAN block run creation)

This is the `before_run` hook type. It is fundamentally different from observational hooks: it fires BEFORE a run is created and can DENY the run.

Flow:

1. Client calls `POST /threads/{id}/runs` (create run)
2. Before thread auto-creation, before run row insertion, before anything — the control plane sends a sync HTTP POST to your preflight hook endpoint
3. Your endpoint inspects the request (who is creating it, which agent, what input) and responds with `{"allow": true}` or `{"allow": false, "reason": "..."}`
4. If denied: the run is never created. Client gets a 403 with the reason.
5. If the hook times out or returns an error: the run is DENIED (fail-closed). A dead preflight hook blocks all run creation until fixed or disabled.

Use cases: custom billing checks ("has this tenant paid?"), compliance gates ("this agent is not approved for this tenant's data classification"), capacity management ("this tenant's queue is too deep, reject new work").

## Configuration

In `langgraph.json`:

```json
{
  "webhooks": {
    "sinks": [
      {
        "url": "https://your-app.com/hooks/runkite",
        "secret": "whsec_abc123",
        "events": ["run_complete", "error"]
      }
    ],
    "preflight_hooks": [
      {
        "url": "https://your-app.com/hooks/before-run",
        "timeout_ms": 3000
      }
    ]
  }
}
```

`sinks` are observational (async). `preflight_hooks` are sync gates. You can have multiple of each.

## Dead letters

When an observational webhook delivery fails (target returns non-2xx or times out after retries), the payload is stored as a "dead letter" in Postgres. The Admin UI shows dead letters with their original payload, delivery attempts, and failure reasons. Admins can trigger manual redelivery.

**Caveat with redelivery:** The webhook secret used for HMAC signing may not be persisted alongside the dead letter payload in all backends. Redelivered webhooks may have invalid or missing signatures. Receivers that strictly validate HMAC will reject redeliveries. This is a documented limitation — configure your receiver to optionally allow redeliveries based on Admin UI source IP or a separate mechanism.

## Worker pool design (why bounded, not goroutine-per-event)

Early versions spawned one goroutine per webhook delivery. Under burst load (hundreds of runs completing simultaneously), this created thousands of concurrent HTTP connections, exhausting file descriptors and overwhelming targets. The current design uses a fixed pool of 20 workers with a queue of 500. If the queue fills (extreme burst beyond what 20 workers can drain), newer events are dropped with a log warning. This bounds resource usage to a known ceiling regardless of run volume.

**Code:** `internal/hooks/hooks.go` (dispatcher, worker pool, event types), `internal/hooks/webhook.go` (HTTP delivery + HMAC signing), `internal/hooks/gate.go` (preflight gate logic)

---



# 39. Deep Dive — The Dual-Write Intuition (Why Postgres + Redis Is Safe)

People hear "we write to two systems" and immediately worry about distributed transaction issues. Here is why it works:

**Different responsibilities, not duplicated data.** Postgres stores WHAT happened (runs, statuses, threads — the permanent record). Redis stores HOW TO COORDINATE (queues, leases, events — the ephemeral machinery). They are not storing the same data in two places; they are storing different data for different purposes.

**Crash windows are bounded by reclaim.** The worst case is: "Postgres says pending, Redis lost the job." In this case, the run stays pending forever... unless you have monitoring that detects runs stuck in pending for abnormally long. The reclaim loop specifically handles the case where a job was dequeued but never completed. For the case where enqueue itself fails, `rollbackCreatedRun` immediately fixes Postgres.

**There is no distributed transaction.** We do not try to atomically commit to both Postgres and Redis. Instead:

- If Postgres succeeds but Redis fails → rollback Postgres (immediate fix)
- If Redis succeeds but ReportStatus never reaches Postgres → lease expires, reclaim fires, new attempt eventually succeeds in updating Postgres
- If both succeed → happy path

The design is: write Postgres first (durable truth), then Redis (coordination). If the coordination step fails, fix the truth immediately. If the truth write succeeded but coordination eventually fails during execution, self-healing via leases and reclaim fixes it within seconds.

---



# 40. Deep Dive — Production Day-One Checklist

If you are deploying Runkite for real production use, here is what you need:

**Infrastructure:**

- Postgres (managed, with backups and failover)
- Redis (managed, with persistence and ideally Redis Sentinel or cluster for HA)
- Load balancer (nginx/Envoy) with HTTP/2 support for gRPC
- TLS certificates for external traffic

**Configuration:**

- Client auth in langgraph.json (JWT or API keys)
- RUNNER_TOKEN_* for each runner kind
- RUNNER_TENANTS_* allowlists for multi-tenant
- REDIS_URL for rate limits, sessions, and HA coordination
- POSTGRES_DSN for durable state
- Policy section if you need governance

**Operational readiness:**

- Sticky routing for /mcp (if using Agent Protocol MCP)
- Redis-backed admin sessions (or sticky for /admin)
- OTEL_EXPORTER_OTLP_ENDPOINT for observability
- Prometheus scraping /metrics (protect with token or network)
- Alert on: queue depth growing, reclaim count > 0, error rate, rate limit 429 rate

**Security verification:**

- Run `make smoke-governance` on Postgres to verify policy enforcement works
- Test kill switch: activate for a test tenant, verify creates fail
- Test cancel: create a slow run, cancel it, verify interrupted status
- Test crash recovery: pause a runner, verify reclaim fires and another runner picks up

**What to rehearse:**

- Kill switch activation and deactivation
- Break-glass mint and close
- Runner token rotation (comma allowlist for zero-downtime)
- Postgres failover (verify runs in-flight still complete via Redis)

---



# 41. Deep Dive — How to Read Control Plane Logs

When you look at production logs, here is what key messages mean:

**"job dispatched to runner"** — GetJob succeeded on this replica. Look for run_id and graph_id fields. The runner now has work.

**"reclaimed stale jobs count=N"** — N jobs had expired leases and were requeued. If this is occasionally 1, that is normal (a runner crashed). If it is consistently high, runners are overwhelmed or dying.

**"heartbeat superseded, signaling runner to stop"** — A runner sent a heartbeat for a generation that is no longer current. The system told it to stop. This means reclaim already reassigned its work.

**"ignored superseded status report"** — A stale runner tried to report a final status, but fencing rejected it. The correct status from the winner is already in Postgres.

**"connector pre-warm skipped by policy"** — During createRunCtx, the system tried to pre-mint connector sessions for this run's connector_needs list. Policy denied it. The connector will still be accessible at use time if policy changes, or it will fail at use time with a policy error.

**"rollbackCreatedRun"** — Enqueue to Redis failed after the run was created in Postgres. The run was marked error and the thread released. The client got an error.

**"admission problem: no durable state"** — serve was started without a database configured. It is about to exit.

**"kill switch active, refusing create"** — A run create was rejected because the target tenant/agent has an active kill switch.

---



# 42. Deep Dive — Runner Token Rotation Without Downtime



## The problem

You need to rotate a runner token (perhaps it was leaked, or you have a rotation policy). But if you change the token atomically, all existing runners are immediately unable to authenticate — they get UNAUTHENTICATED on every gRPC call and stop working.

## The solution: comma-separated allowlist

Runner tokens support a comma-separated list: `RUNNER_TOKEN_PYTHON_LANGGRAPH=old_token,new_token`

During rotation:

1. Configure the CP with BOTH tokens (comma-separated). Restart CPs.
2. Update runner configuration to use the new token. Rolling restart runners.
3. Verify in logs that GetJob succeeds with the new token.
4. Remove the old token from the CP config. Restart CPs.

During step 1-3, both tokens are valid simultaneously. Runners using either token can authenticate. This gives you a zero-downtime rotation window.

The token comparison uses constant-time comparison across the allowlist entries so timing attacks cannot reveal which token matched.

---



# 43. Deep Dive — How LangGraph Resume Works Through the Control Plane



## The full lifecycle of a framework interrupt/resume

1. **Agent code calls interrupt().** The LangGraph graph reaches a point where it needs human input. It saves a checkpoint to Postgres (direct mode) with the interrupt state.
2. **Runner finishes the run.** The runner reports an interrupted status (or the run completes with interrupt events). The thread may be set to an interrupted state depending on the framework's semantics.
3. **Client shows the interrupt to the user.** Your application receives the interrupt event via SSE and shows a question/prompt to the end user.
4. **User responds.** The user types an answer in your application UI.
5. **Client creates a new run with resume_command.** Your application sends:

```json
POST /threads/thr_42/runs
{"agent_id": "travel_bot", "command": {"resume": {"value": "Flight B please"}}}
```

1. **createRunCtx processes the resume.** It normalizes the resume command format (different SDK versions may structure it differently). The thread must be claimable (idle or interrupted depending on the state machine). A new run is created with pending status. The resume_command is included in the RunAssignment.
2. **Runner picks up the assignment.** GetJob returns the new assignment. The runner sees a resume_command in it. It loads the checkpoint from Postgres (the interrupted state from step 1) and applies the resume value.
3. **Execution continues from the checkpoint.** The graph continues from where it paused, now with the user's response ("Flight B please") available as input to the interrupted node.
4. **Normal completion.** Events stream, run completes, status updated to success.

The key point: resume creates a COMPLETELY NEW RUN. It goes through all of createRunCtx, enqueue, GetJob, execution. It is not "continuing" the old run — it is starting a new one that happens to load the old checkpoint. This is why the old run stays "interrupted" forever and the new run is the one that succeeds.

---



# 44. Deep Dive — Retention: Automatic Cleanup of Old Data



## What retention does

A background goroutine (`cmd/retention.go`) periodically deletes old data from Postgres based on configured retention windows. This prevents the database from growing indefinitely.

## What it cleans

- Old completed runs beyond a configured age
- Old thread data (if threads have no recent runs)
- Expired store items (TTL-based cleanup)
- Proxy-mode checkpoint data beyond configured age



## What it does NOT clean

- LangGraph's own checkpoint tables in direct mode (those are managed by LangGraph's checkpointer, not Runkite's retention)
- Audit events (those often need longer retention for compliance)
- Policy configuration (grants, rules — those are operational state, not historical data)

---



# 45. Deep Dive — Run Timeout: Killing Long-Running Agents



## What it does

Optional feature: configure a maximum wall-clock time for agent runs. If a run exceeds this time, the control plane cancels it (similar to the cancel path) and sets its status to "timeout."

## How it works

A background goroutine (`cmd/run_timeout.go`) periodically checks for runs that have been in pending or running status longer than their configured timeout. When found, it initiates the cancel/timeout flow: publish cancel signal, runner cooperatively stops, status becomes timeout.

## Why it is disabled by default

Historical behavior was "run until done" with no time limit. Adding a default timeout would break existing deployments where long-running agents are intentional (like agents that wait for external callbacks). Operators opt in by configuring `run_timeout` per agent or globally.

---



# 46. Deep Dive — The gRPC Connection Affinity Subtlety for Cancel

This is a professor-level detail about cancel delivery in multi-replica deployments.

## The problem

When Runner-A calls GetJob and gets work from CP-2, CP-2 subscribes to the cancel channel for that run (Redis Pub/Sub). CP-2 holds a goroutine that listens for cancel signals.

When the cancel signal arrives (published by some CP into Redis), CP-2's subscription goroutine fires. It calls `notifyWatchers(runner_kind, run_id)` — this is an IN-PROCESS operation that writes to a channel that the WatchCancels stream reads from.

Here is the critical point: `notifyWatchers` is PROCESS-LOCAL. It only notifies WatchCancels streams connected to THIS CP process. If Runner-A's WatchCancels stream is on a different CP (say CP-3), the notification would never reach it.

## Why it usually works

In practice, runners maintain ONE gRPC connection (HTTP/2) to the load balancer, and all their RPCs go over that connection to the same backend. GetJob and WatchCancels share the same TCP connection, so they land on the same CP process. The cancel subscription and the WatchCancels stream are co-located by construction.

## When it breaks

If your load balancer sprays unary RPCs (GetJob) and streaming RPCs (WatchCancels) to different backends independently (which some misconfigured L7 LB configs do), then GetJob might be on CP-2 while WatchCancels is on CP-3. Cancel signals would never reach the runner through WatchCancels.

## The fix

Use a gRPC-aware load balancer that respects HTTP/2 connection multiplexing. Or configure sticky routing for gRPC traffic. Most standard deployments get this right by default because gRPC clients maintain persistent connections.

---



# 47. Deep Dive — Why Generation Starts at 1, Not 0

Generation 0 is reserved as a compatibility value meaning "unknown / legacy runner." Older runners that were built before generation fencing was added would send generation 0 (or not send it at all). The control plane treats generation 0 as a bypass: it does not enforce fencing for generation 0 heartbeats or ReportStatus calls.

This allows mixed-version rollouts: you can upgrade the control plane with fencing support while old runners still work (they just do not get fencing protection). But for production safety, all runners should send real generation values (1+). Generation 0 should be treated as a temporary compatibility measure, not a permanent state.

Starting at 1 makes "uninitialized/zero-value" distinguishable from "first actual attempt." If generation were 0-based, you could not tell "this runner never set a generation" from "this is the first attempt."

---



# 48. Deep Dive — Multi-Tenancy Practical Implications



## Data isolation

All SQL queries include a tenant_id scope. A user in tenant "acme" cannot see threads, runs, or store items from tenant "beta." This scoping happens in the state store layer — every query adds `WHERE tenant_id = ?`.

## Cross-tenant A2A prevention

When an agent creates an A2A child run, the control plane looks up the parent run, extracts the parent's tenant_id, and FORCES the child to inherit it. The runner cannot say "create this child run for tenant X" if X differs from the parent's tenant. This prevents cross-tenant delegation forgery.

## Admin access

Admins with admin-level auth can typically see across tenants (for operational purposes). The Admin UI does not scope to a single tenant — it shows everything for diagnostics.

## Direct-mode DB access

In direct mode, runners bypass tenant scoping because they write raw SQL. The checkpoint prefix convention (`tenant:thread_id`) provides some isolation, but a compromised runner could ignore it. This is why direct mode is a trust boundary concern for multi-tenant production.

---



# 49. Putting It All Together — The Security Story

Here is the complete security posture of Runkite, layer by layer:

**Layer 1: Network.** TLS at the edge. Runner-to-CP traffic can also be TLS (cmd/tls.go helpers). gRPC ports should not be exposed to the internet — only runners need them.

**Layer 2: Authentication.** Client auth (JWT/API key/webhook). Admin auth (keys + sessions). Runner auth (kind tokens). All validated before any logic runs.

**Layer 3: Authorization.** Permission-based (read/write/admin, agents:id:run). Thread/run scoped to tenant. Admin routes require admin permission.

**Layer 4: Run-binding.** Proxy paths derive identity from the inflight assignment, not from runner headers. Forgery is impossible unless you can manipulate the Redis inflight record (which requires infrastructure access).

**Layer 5: Policy Decide.** Grants + webhook + mandatory HITL. Fail-closed on errors. Tool-level access control.

**Layer 6: Audit.** Every policy decision recorded. Durable on SQL. Searchable.

**Layer 7: Emergency controls.** Kill switches (immediate stop). Break-glass (temporary bypass with audit). Both fail-closed on audit write failure.

**Layer 8: Serve admission.** Production refuses to start without proper configuration. Cannot accidentally run insecure.

**Layer 9: Operational.** Non-root containers. CORS fixed. Secure headers. CSRF for cookie mutations. Constant-time token comparison.

No single layer alone is sufficient. The strength is in their combination: even if one layer is bypassed (e.g., a runner token is leaked), run-binding prevents tenant escalation. Even if the webhook PDP is compromised (returns "allow" for everything), mandatory HITL still blocks dangerous operations. Even if break-glass is used, audit writes are mandatory.

---



# 50. Interview Preparation — If Someone Asks You About Runkite

**"What is Runkite in one sentence?"**
A self-hosted Agent Protocol control plane that coordinates AI agent execution across any framework, providing dispatch, streaming, multi-tenancy, governance, and credential management in one Go binary.

**"How does a run execute?"**
Client HTTP → CP writes Postgres (run pending, thread busy) → CP pushes Redis (job queue) → Runner gRPC polls Redis (GetJob) → Runner executes agent → Runner streams events (gRPC → CP → Redis → SSE) → Runner reports status → CP Acks Redis + updates Postgres → Thread idle.

**"How do you handle crashes?"**
Runner heartbeats every 2s. Lease in Redis. Stale after 6s → reclaim bumps generation → requeue. New runner wins. Old runner gets "superseded" on next heartbeat and stops. Generation fencing prevents stale writes.

**"How do multiple control planes work?"**
Shared-nothing processes, shared-everything backends. Postgres is truth. Redis is coordination. Any CP handles any request. No affinity needed (except /mcp sticky).

**"What is your security model?"**
Govern the plane, not the prompt. Policy engine evaluates grants + webhook for every connector tool call. Fail-closed on errors. Kill switches, break-glass, mandatory HITL, audit trail. Run-binding prevents tenant forgery. Serve refuses to start insecure.

**"What about Kafka?"**
Kafka for JobQueue is Compatible. But EventBroker fan-out and CancelBroker need Redis alongside Kafka. Kafka alone for multi-replica HA is Experimental because you lack fan-out, cancel delivery, and safe reclaim leader.

**"What about MongoDB?"**
State store: Compatible for agents/threads/runs. Governance: NOT supported — no audit tables, Admin governance returns 501, pending fails closed to deny. Do not market governance for Mongo.

**"What are the two kinds of HITL?"**
Framework interrupt: agent pauses, user resumes via SDK (new run with resume_command). Connector pending: policy blocks tool call, admin approves in Admin UI, agent must retry. Different problem, different protocol, different approver. Never confuse them.

---



# 51. Deep Dive — Exactly What createRunCtx Does With Each Error

When createRunCtx encounters problems at each step, here is exactly what error the CLIENT sees:

**Unknown agent (GetAgent fails):** The client gets HTTP 404 — "agent not found." Nothing was claimed, nothing needs cleanup.

**Rate limited (AllowAgent fails):** The client gets HTTP 429 — "rate limit exceeded." No thread was claimed, no run was created. Simple rejection.

**Kill switch active:** The client gets HTTP 403 — "forbidden: kill switch active for [scope]." No thread was claimed.

**Permission denied (agents:run):** The client gets HTTP 403 — "forbidden: insufficient permissions." No thread was claimed.

**Policy Decide denies run.create:** The client gets HTTP 403 — "forbidden: policy denied run creation." No thread was claimed.

**Thread not found (strict mode):** The client gets HTTP 404 — "thread not found." Nothing else happens.

**Thread busy (TryClaimThread fails):** The client gets HTTP 409 — "conflict: thread is busy." This means another run is already active on that thread. The client should wait and retry, or use a different thread.

**A2A depth exceeded:** The client (or the parent runner calling A2A) gets a typed error. The HTTP mapping is typically 400 — "bad request: A2A depth limit exceeded."

**A2A breadth exceeded:** Similar — 400 with breadth message.

**Admission limits (too many concurrent/daily runs):** The client gets HTTP 429 — "admission limit: concurrent runs exceeded" or "daily limit exceeded." This is different from rate limiting — rate limits are about request frequency, admission limits are about occupancy.

**Database error during CreateRun:** The client gets HTTP 500 — internal server error. The thread claim may or may not have happened — depends on where the error occurred. Error handling paths attempt cleanup but may leave traces.

**After createRunCtx succeeds but enqueue fails:** The handler calls rollbackCreatedRun (marks run as error, releases thread) and the client gets HTTP 500 with "enqueue failed." The run exists in Postgres marked as errored — it is not invisible; you can see it in the Admin UI.

---



# 52. Deep Dive — Stream-on-Create vs Background-Create vs Wait

When you create a run, you have three choices for how to get the result. All three use the same createRunCtx + enqueue internally — they differ in what happens AFTER enqueue.

## Background create (POST /threads/{id}/runs)

The handler creates the run, enqueues it, and immediately returns the pending run JSON to the client. The client gets back a response within milliseconds. The run executes in the background. The client can later:

- GET /threads/{id}/runs/{run_id} to check status (polling)
- Open SSE connection to /threads/{id}/runs/{run_id}/stream (streaming)
- Both work any time after creation

This is best for: fire-and-forget operations, batch processing, when you will check results later.

## Stream-on-create (POST /threads/{id}/runs/stream)

The handler creates the run, enqueues it, subscribes to the Redis event stream for this run, and keeps the HTTP connection open as SSE. The client immediately starts receiving events as they happen. The connection stays open until the run completes or the client disconnects.

If the client disconnects mid-stream (network issue), the events are still in Redis. The client can reconnect and subscribe from where they left off (or replay from the beginning).

This is best for: real-time UX where you want tokens to appear immediately.

## Wait (POST /threads/{id}/runs/wait)

The handler creates the run, enqueues it, and holds the HTTP connection open until the run reaches a terminal status. Then it returns the FINAL run payload (with output, status, etc.) as a single JSON response. The client blocks for the duration of the run — which could be seconds or minutes depending on the agent.

Internally, the handler subscribes to status updates and blocks until success/error/interrupted/timeout. This is essentially long-polling: the connection stays open, the server responds only when there is something definitive to return.

This is best for: simple scripts, automation, CLI tools where you just want the final answer without managing SSE parsing.

## WebSocket alternative

All three patterns are also available over WebSocket. Instead of separate HTTP endpoints, you open one WebSocket connection and send command messages (create, stream, wait, cancel) and receive event messages. The WebSocket stays open for multiple interactions.

---



# 53. Deep Dive — What Happens Inside the Python Runner

Let me walk through what the Python LangGraph runner does when it receives an assignment.

## Boot

When the runner process starts (`python -m runkite_runner`), it:

1. Discovers langgraph.json in the working directory
2. Loads all graph definitions (imports your Python code, instantiates StateGraphs)
3. Opens a gRPC connection to the control plane (using RUNKITE_URL or LANGGRAPH_URL)
4. Authenticates with RUNNER_TOKEN (sent as metadata on every gRPC call)
5. Enters the main loop: call GetJob, execute, call GetJob again...



## Receiving an assignment

GetJob blocks until work is available. When it returns, the runner has an assignment JSON with:

- run_id, thread_id, graph_id (which graph to load)
- generation (for heartbeat and run-binding)
- input (the user's message/data)
- config (runtime configuration)
- resume_command (if this is a resume from interrupt)
- connector_needs (what connectors this agent uses)
- stream_modes (what events to produce)
- user context (who requested this)
- trace_context (for distributed tracing)



## Execution loop

The runner:

1. Starts a heartbeat background task (calls Heartbeat RPC every 2 seconds)
2. Opens a StreamEvents call (will send events as they happen)
3. Loads the graph for graph_id
4. If resume_command is present, loads the checkpoint and applies the resume
5. Calls the graph's astream/invoke with the input
6. As the graph produces state updates and messages, formats them as events and sends via StreamEvents
7. Monitors the heartbeat response — if superseded=true, cancels execution
8. Monitors WatchCancels — if cancel signal received, cancels execution
9. When execution completes, calls ReportStatus with the final status



## Connector access during execution

If the agent's tools need external services (defined in connector_needs), the runner:

1. Calls POST /internal/connectors/{name}/session on the CP to get a session token
2. Uses that token for subsequent MCP calls to /internal/connectors/{name}/mcp
3. The CP handles auth, policy, and upstream credential injection transparently
4. Helper code in `connectors.py` manages token caching and re-minting on expiry



## A2A calls during execution

If the agent wants to delegate to another agent:

1. The runner calls POST /internal/a2a/runs on the CP
2. Includes the run_id as parent_run_id (for tree tracking)
3. The CP creates a child run through normal createRunCtx + enqueue
4. The runner can poll for the child's completion or continue working

**Code:** `python/runkite_runner/worker.py` (main loop), `heartbeat.py` (heartbeat task), `connectors.py` (connector helpers), `a2a.py` (delegation), `store.py` (KV store access), `checkpoint.py` (direct-mode checkpoints).

---



# 54. Deep Dive — What Makes Each Framework Adapter Different

Runkite supports multiple AI agent frameworks. Each needs an "adapter" that translates between the framework's native execution model and Runkite's Runner Protocol.

## LangGraph (Python) — The reference implementation

LangGraph graphs are Python StateGraphs with nodes (functions), edges (routing), and typed state. The adapter calls `graph.astream()` and maps the yielded chunks to RunEvents for StreamEvents. Interrupt/resume maps directly to LangGraph's interrupt() and checkpoint resume semantics. This is the most mature adapter.

## CrewAI

CrewAI has Crews with Agents and Tasks. The adapter wraps a Crew execution, streams task progress events, and maps completion to ReportStatus. CrewAI does not have native interrupt/resume — so framework HITL (Kind 1) is limited.

## LlamaIndex

LlamaIndex workflows are async Python. The adapter runs the workflow and streams step events. Less standardized event format compared to LangGraph.

## AutoGen

AutoGen has multi-agent conversations. The adapter captures the conversation flow and maps messages to events. AutoGen uses its own internal GenAI span naming (different from runkite.llm/tool).

## LangChain (plain, not LangGraph)

Plain LangChain chains without LangGraph. Limited streaming support. No interrupt/resume. Suitable for simple sequential chains.

## LangGraph.js (TypeScript)

Mirror of the Python LangGraph adapter but in TypeScript. Similar capabilities. One limitation: TypeScript lacks full JSON schema introspection that Python LangGraph provides — so agent schema reporting is a stub in some cases.

## What all adapters share

Regardless of framework, every adapter must:

- Accept a RunAssignment JSON
- Start a heartbeat loop (2s)
- Open StreamEvents and send progress
- Handle WatchCancels (cooperative stop)
- Handle superseded heartbeat response (stop)
- Report final status
- Send correct generation on all calls
- Use run-binding headers for connector/store/vector calls

If you wanted to add a NEW framework (say, a Rust agent framework), you would write an adapter that speaks these five gRPC calls + the HTTP internal APIs. You would NOT need to modify the Go control plane at all.

---



# 55. Deep Dive — The Cron Claims Table and Multi-Replica Election

Let me explain exactly how cron prevents double-firing across replicas.

## The table

Postgres has a `cron_claims` table with columns like:

- schedule_name (text)
- fire_time (timestamp)
- claimed_by (replica identifier)
- claimed_at (timestamp)

There is a UNIQUE constraint on (schedule_name, fire_time).

## The algorithm

Every ~15 seconds, each CP replica does:

```
for each schedule in config:
    next_fire = compute_next_fire_time(schedule)
    if next_fire is in the past (due now or overdue):
        try:
            INSERT INTO cron_claims (schedule_name, fire_time, claimed_by)
            VALUES (schedule.name, next_fire, this_replica_id)
        if INSERT succeeds:
            create_run(schedule.agent_id, schedule.input, schedule.thread_id)
        if INSERT fails (duplicate key):
            pass  # another replica already claimed this fire
```

The UNIQUE constraint is the election mechanism. Only one INSERT can succeed for a given (schedule_name, fire_time). All other replicas' INSERTs fail with a unique violation, which they catch and ignore.

## Edge cases

**All replicas down during a fire time:** The fire is missed entirely. When replicas come back, they compute the NEXT fire time (not a backlog of missed ones). Only the most recent missed fire might be caught up — not every single one.

**Clock skew between replicas:** If replica clocks differ significantly, one replica might claim fires earlier than others. Use NTP synchronization.

**Claim succeeds but run creation fails:** The claim row exists (preventing others from firing), but no run was actually created. This is a rare "at most once" loss. Operators can detect it by comparing cron_claims rows against actual run records.

---



# 56. Deep Dive — Why We Chose BUSL-1.1 Licensing



## What BUSL-1.1 means

Business Source License 1.1. The key terms:

- You CAN self-host Runkite in your own production infrastructure
- You CAN modify the code for internal use
- You CAN build products ON TOP of Runkite (your agents, your SaaS that uses Runkite internally)
- You CANNOT offer Runkite itself as a hosted service to third parties (you cannot sell "Runkite Cloud" as a product)
- After the change date (typically 3-4 years from release), the code converts to Apache 2.0 (fully permissive)



## Why not Apache 2.0 from day one?

The fear: a large cloud provider takes the code, hosts it as a managed service, and captures all the market value without contributing back. BUSL prevents that while still allowing the entire self-hosted use case.

## What users CAN do (the non-restrictive part)

- Run Runkite in production for your company
- Build and sell AI agent products that use Runkite as infrastructure
- Modify the source code and keep modifications private
- Deploy multiple instances across your organization
- Use it for internal tools, customer-facing products, everything

The only restriction: you cannot become a competing control-plane-as-a-service vendor using this codebase.

---



# 57. Deep Dive — The Registry vs Aliases vs Agent Table

These three things sound similar but serve different purposes. Do not conflate them.

## Agent table (state store)

The agent table stores: what agents CAN BE EXECUTED. When you configure agents in langgraph.json and start the server, they are bootstrapped into the agents table in Postgres. createRunCtx does GetAgent to confirm an agent exists here. This is the execution registry.

## Aliases (config)

Aliases are routing rules. They map one name to multiple real agent_ids with weights. They live in the langgraph.json config, loaded at boot time into an in-memory resolver. They are NOT database rows — they do not appear in the agents table. They are resolved before GetAgent.

## Registry (discovery feature)

The registry is a separate feature for publishing "agent cards" — metadata about what an agent does, its capabilities, its input schema. Think of it as a catalog or directory. You can register agents for discovery by other systems. Registry entries are stored in Postgres but are separate from the execution agent table.

An alias can route to an agent that has a registry entry, but these are three independent lookups:

1. Alias config → which real agent_id? (in-memory, boot time)
2. Agent table → does this agent exist for execution? (Postgres, per request)
3. Registry → what metadata does this agent publish? (Postgres, for discovery)

---



# 58. Deep Dive — CORS and Why the Old Bug Was Dangerous



## What CORS is

When a browser on `https://app.example.com` makes a fetch() request to `https://runkite.company.com/api/runs`, the browser checks CORS (Cross-Origin Resource Sharing) rules. The server must respond with headers saying "yes, I allow requests from app.example.com."

## The old bug

The old CORS handler did:

1. Read the `Origin` header from the request (e.g., "[https://evil.com](https://evil.com)")
2. Echo it back as `Access-Control-Allow-Origin: https://evil.com`
3. Also set `Access-Control-Allow-Credentials: true`

This combination is catastrophically bad. It means ANY website can make authenticated requests to your Runkite API using the user's cookies/tokens. An attacker creates a page at evil.com, the user visits it, evil.com's JavaScript can now create runs, read threads, and access everything the user can — because the browser sends credentials and the server says "yes, evil.com is allowed."

## Why literal `*` is actually safer

`Access-Control-Allow-Origin: *` sounds scarier but is actually safer because browsers REFUSE to send credentials (cookies, auth headers) when the server responds with literal `*`. So `*` means "anyone can make anonymous requests" but nobody can make authenticated requests cross-origin.

## The fix

Now: if you configure specific allowed origins (like `https://app.example.com`), ONLY those origins get reflected back with credentials allowed. If you configure `*`, the response is literal `*` without credentials. You cannot have both "any origin" and "with credentials" — the code prevents that combination.

**Code:** `internal/cors/`

---



# 59. Deep Dive — Secure Headers

The control plane sets standard security response headers:

- `X-Content-Type-Options: nosniff` — prevents browsers from MIME-sniffing responses (interpreting a JSON response as HTML and executing script)
- `X-Frame-Options: DENY` — prevents the Admin UI from being embedded in an iframe on another site (clickjacking protection)
- `Strict-Transport-Security` — when TLS is configured, tells browsers to only use HTTPS for this domain

These are defense-in-depth measures. They do not replace proper authentication but they prevent entire classes of browser-based attacks.

**Code:** `internal/secureheaders/`

---



# 60. Deep Dive — What "Govern the Plane, Not the Prompt" Means

This is the philosophical design principle behind Runkite's security approach.

**What we govern (the plane):**

- WHO can run which agents (authn + authz)
- WHICH connectors and tools an agent can use (policy grants)
- WHETHER a dangerous action requires human approval (mandatory HITL)
- WHEN to stop everything (kill switches)
- HOW to audit every decision (audit trail)
- WHO can access what data (tenant isolation, run-binding)

**What we do NOT govern (the prompt):**

- What the LLM says in its output
- Whether the prompt contains injection attempts
- Whether the output contains PII
- Whether the agent's reasoning is correct
- Whether the prompt is ethical or safe

This is a deliberate boundary. Prompt-level safety (injection detection, content moderation, PII scanning) is a different discipline, solved by different vendors (Lakera, Guardrails AI, Azure Content Safety, etc.). Runkite does not compete with them and does not try to solve their problem.

The insight is: because ALL connector access flows through the control plane, we can enforce tool-level policies without understanding prompts. We do not need to know WHY the agent wants to call delete_repo — we just need to know that THIS agent is not ALLOWED to call delete_repo. The policy is structural, not semantic.

If a tool call bypasses the connector proxy entirely (the runner makes a raw HTTP call to Salesforce using credentials it has locally), the plane cannot see it or govern it. This is the honest boundary: governance only works for traffic that flows through the plane. Teach customers this clearly.

---



# 61. Deep Dive — What Happens When You Run `make smoke-governance`

This is the smoke test that proves governance works end-to-end on a Postgres deployment.

The script (`scripts/smoke-governance.sh`) does approximately:

1. Starts a control plane with Postgres + Redis + policy configured (webhook pointing to the example PDP)
2. Creates a run (verifies basic run creation works)
3. Attempts a connector tool call that the PDP denies (e.g., `delete_repo`) — verifies the call is rejected with the correct reason code
4. Attempts a connector tool call that the PDP marks as pending — verifies the pending_actions row appears in the database
5. Approves the pending action via the Admin API — verifies the one-shot capability is created
6. Retries the tool call — verifies it succeeds (capability consumed)
7. Activates a kill switch — verifies subsequent creates fail
8. Mints a break-glass window — verifies bypass works with audit
9. Searches audit events — verifies all decisions are recorded
10. Cleans up

If this script passes on your deployment, you can honestly claim that connector governance works. If it fails, something is misconfigured. This is the minimal proof that governance marketing claims are real.

**Code:** `scripts/smoke-governance.sh`

---



# 62. Deep Dive — Differences Between `runkite dev` and `runkite serve`



## `runkite dev`

- Uses SQLite (file on disk) — no Postgres needed
- Uses in-process transport — no Redis needed
- Runner auth disabled — any process can be a runner
- Client auth disabled — any HTTP request is accepted
- No admission checks — starts immediately regardless of config
- Single process only — no multi-replica support
- Perfect for local development and demos



## `runkite serve`

- Requires a configured state backend (Postgres/MySQL/Mongo)
- Requires a configured transport (Redis/NATS/Kafka)
- Requires runner tokens
- Requires client auth
- Runs admission checks — refuses to start if not configured safely
- Supports multi-replica behind a load balancer
- Production-grade

The two modes share 99% of the same code paths. The difference is: `dev` skips safety checks and uses zero-dependency defaults. `serve` requires explicit configuration and validates it before starting.

This means: if something works in `dev` but not in `serve`, the issue is likely auth/config/network, not a code bug. And if something works in `serve` on one replica but not multi-replica, the issue is likely transport/sticky-routing, not application logic.

---



# 63. Key Numbers Summary (All in One Place)


| Timer/Limit               | Value               | Purpose                         |
| ------------------------- | ------------------- | ------------------------------- |
| Heartbeat interval        | ~2 seconds          | Runner proof of life            |
| Reclaim stale threshold   | ~6 seconds          | Declare runner dead             |
| gRPC keepalive            | ~4 seconds          | Detect dead TCP connections     |
| Reclaim loop tick         | ~2 seconds          | How often reclaim checks        |
| Reclaim leader TTL        | ~3 seconds          | Lock for single-reclaim-process |
| Connector session TTL     | 15 minutes absolute | Credential lifetime limit       |
| Admin session TTL         | 12 hours sliding    | Browser cookie lifetime         |
| Policy overlay poll       | ~15 seconds         | Cross-replica grant sync        |
| Break-glass max duration  | 24 hours            | Cannot create permanent bypass  |
| Cron tick interval        | ~15 seconds         | How often schedules are checked |
| Generation starting value | 1                   | Zero reserved for legacy compat |
| A2A max_depth default     | 10 (configurable)   | Prevents infinite recursion     |
| A2A max_breadth default   | 20 (configurable)   | Prevents fork bombs             |


---



# 64. Final Summary — The Mental Model in One Page

**Architecture:** Client (HTTP) → Control Plane (Go binary) → Runner (Python/TS, gRPC). The CP never runs agent code. The runner never serves end users. The CP is the middleman for everything.

**Data flow for a run:** HTTP create → Postgres (run pending, thread busy) → Redis (job queued) → Runner GetJob (Redis dequeue) → Runner executes (LLM calls) → StreamEvents (Redis publish) → SSE (Redis subscribe, any replica) → ReportStatus (Redis Ack, Postgres success/idle).

**Crash recovery:** Heartbeat every 2s → Redis lease. Stale >6s → reclaim bumps generation → requeue. New runner wins. Old runner gets superseded on next heartbeat, stops, cannot overwrite.

**Multi-replica:** Shared-nothing CP processes. Shared Postgres + Redis. Any CP handles any request. Sticky only for /mcp and Admin-without-Redis.

**Governance:** Policy Decide on every connector tool call. Grants (config + admin SQL overlays) + webhook PDP + mandatory HITL. Fail-closed on errors. Kill switches, break-glass (audited), pending HITL (admin approves, agent retries). SQL backends only for durable trail.

**Two HITLs:** (1) Framework: agent interrupt → user resume → new run loads checkpoint. (2) Connector: policy pending → admin approve → agent retry tool call. Different problem, different approver, different protocol.

**Tenancy:** Flat string tenant_id. Scoped queries. Run-binding prevents forgery on proxy paths. RUNNER_TENANTS allowlists limit blast radius.

**Honest gaps:** Mongo no governance durability. Kafka needs Redis. /mcp needs sticky. Non-terminal events unfenced. A2A breadth race. Direct-mode trust. Memory rate limits wrong on multi-pod.

When you can explain all of the above from memory without looking at notes, you understand the product completely.

---



# 65. Extended Narrative — A Full Reclaim Story Told Slowly

Let me tell the crash recovery story as a slow documentary narration, naming every process and every wire.

## The setup

Runner-A is executing a research agent. It has been happily sending heartbeats every 2 seconds, streaming tokens to the client, and everything is working. The run_id is `run_77`, generation is 1, and it has been running for about 30 seconds.

## The disaster

Runner-A's container is killed by Kubernetes (OOM — out of memory). The process vanishes instantly. There is no graceful shutdown. No final heartbeat. No ReportStatus. Just gone.

## What the system observes

Nothing happens immediately. The last heartbeat renewed the Redis lease 1.5 seconds ago. The lease is fresh. The system does not yet know anything is wrong.

2 seconds later: Runner-A would have sent a heartbeat. It does not. The lease timestamp in Redis is now ~3.5 seconds old. Still within the 6-second threshold.

4 seconds later: Still no heartbeat. The lease is now ~5.5 seconds old. Almost stale but not quite.

6 seconds later: The reclaim loop (running on whichever CP holds the reclaim-leader lock) ticks. It calls `ReclaimStale(maxAge: 6s)`. It finds `run_77` in the inflight set with a lease timestamp older than 6 seconds.

## What reclaim does

The reclaim leader (say CP-2) performs these Redis operations atomically (via Lua script on Redis, or similar mechanism on other transports):

1. Read the current inflight entry for `run_77`. Current generation: 1.
2. Increment generation: 1 → 2.
3. Update the inflight entry with generation 2 and a fresh timestamp.
4. Re-serialize the assignment with `generation: 2` and push it back onto the ready queue for `python-langgraph`.

CP-2 logs: `"reclaimed stale jobs count=1 max_age=6s"`

## A new runner picks up the work

Runner-B has been waiting in GetJob. Its next poll hits CP-3. CP-3 Dequeues from Redis — gets the assignment for `run_77` with generation 2. CP-3 subscribes to the cancel channel for `run_77`. Returns the assignment to Runner-B.

Runner-B starts executing: loads the graph, begins from scratch (or from checkpoint if available), starts heartbeating with generation 2. Everything proceeds normally.

## Runner-B completes successfully

The agent finishes its work. Runner-B calls ReportStatus with `run_id=run_77, status=success, generation=2`. Some CP receives this, calls `queue.Ack(run_77, generation=2)`. Generation matches the current inflight — Ack succeeds. The inflight entry is removed. Postgres updates: run_77 status = success, thread status = idle. Terminal event published to Redis Stream. Client sees success.

## What if Runner-A somehow comes back?

Imagine the OOM was actually just a brief freeze (docker pause, not kill). Runner-A unfreezes and tries to continue.

Runner-A's heartbeat task fires. It calls Heartbeat with `run_id=run_77, generation=1`. The CP calls `queue.Renew(run_77, generation=1)`. Redis checks: current generation for run_77 is 2 (or the job is already Acked and gone). Generation 1 is not current. Returns "not current."

The CP responds to Runner-A: `HeartbeatResponse{ok: true, superseded: true}`.

Runner-A sees `superseded: true`. Its heartbeat code triggers the cancellation of the local graph execution. The agent stops mid-work. Runner-A calls ReportStatus with `run_id=run_77, status=interrupted, generation=1`. The CP calls `queue.Ack(run_77, generation=1)`. Generation 1 is not current — Ack is rejected. The CP responds `ReportStatusResponse{ok: true, superseded: true}`. The status report is IGNORED.

Final state: Postgres has `run_77 = success` (from Runner-B). The stale Runner-A's interrupted status was never recorded. Client sees success. Thread is idle. Everything is correct.

## Why this works without any human intervention

The system healed itself entirely through:

1. Lease expiry (detecting the problem)
2. Generation bumping (preventing the loser from corrupting)
3. Re-queue (giving the work to someone healthy)
4. Superseded response (telling the loser to stop)

No human operator needed to restart anything, check any dashboard, or run any command. The recovery happened within ~8 seconds of the crash (6s stale threshold + one reclaim tick + one GetJob).

---



# 66. Extended Narrative — A Full Pending HITL Story Told Slowly



## The setup

An agent called `finance_bot` is running for tenant "acme." It is processing a user request: "Transfer $5000 from account A to account B." The run_id is `run_88`, and it is executing on Runner-A.

## The tool call

The agent decides it needs to call the banking connector's `transfer_funds` tool. Runner-A constructs an MCP tools/call request:

```json
{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "transfer_funds", "arguments": {"from": "A", "to": "B", "amount": 5000}}}
```

Runner-A sends this to `POST /internal/connectors/bank/mcp` on the control plane, with its run-binding headers (`X-Runkite-Run-Id: run_88`, `X-Runkite-Generation: 1`) and connector session token.

## Policy evaluation

The CP's connector MCP proxy receives the request. It:

1. Validates the session token (correct run, generation, connector — good)
2. Extracts the tool name from the JSON-RPC body: `transfer_funds`
3. Looks up the inflight assignment via run-binding: tenant=acme, agent=finance_bot
4. Calls `Engine.Decide()` with: stage=tool.call, tenant=acme, agent=finance_bot, connector=bank, tool=transfer_funds

The Decide evaluation:

- Check break-glass: none active. Continue.
- Check grants: there IS a grant for (acme, finance_bot, bank) with tools allow list including `transfer_funds`. First layer says allow.
- Check webhook PDP: sends the decision context to the configured webhook. The PDP returns `{"effect": "pending", "reason": "high-value transfer requires approval"}`.
- Webhook says pending. Override allow to pending.
- Check mandatory HITL: there is also a mandatory HITL rule for (acme, bank, transfer_funds). This would have forced pending anyway even if the webhook said allow. But since it is already pending, no change.

Final decision: **pending**.

## What the CP does with a pending decision

The CP does NOT pause the LangGraph execution. It does NOT create a LangGraph interrupt. Instead:

1. Generates an `action_id` (like `act_xyz789`).
2. Inserts a row into `pending_actions` table: `(action_id, tenant=acme, agent=finance_bot, connector=bank, tool=transfer_funds, run_id=run_88, generation=1, status=pending, arguments={from:A, to:B, amount:5000})`.
3. Publishes a `tool_auth` event on the run's event stream: `{"type": "tool_auth", "data": {"effect": "pending", "action_id": "act_xyz789", "connector": "bank", "tool": "transfer_funds"}}`. (This appears on the client's SSE if tool_auth events are enabled.)
4. Returns a JSON-RPC error to the runner: `{"jsonrpc": "2.0", "id": 1, "error": {"code": -32000, "message": "policy pending", "data": {"reason_code": "policy_pending", "action_id": "act_xyz789"}}}`.
5. Writes an audit event recording the pending decision.



## What the runner/agent does

Runner-A receives the JSON-RPC error. The MCP client in the runner surfaces this as a tool call error to the LangGraph graph. What happens next depends on how the agent is written:

- **Well-written agent:** The graph catches the error, notes that the tool is pending approval, and either waits (retries after a delay) or tells the user "Your transfer is awaiting administrator approval" and completes the run gracefully.
- **Poorly-written agent:** The graph does not handle the error, crashes, or silently moves on without the transfer. This is an AGENT bug, not a platform bug. The CP did its job (blocked the dangerous call and recorded it).



## Admin approval

An administrator opens the Admin UI at `/admin/pending`. They see a row:

- Tenant: acme
- Agent: finance_bot
- Connector: bank
- Tool: transfer_funds
- Arguments: from=A, to=B, amount=5000
- Action ID: act_xyz789
- Status: pending

The admin reviews the transfer and clicks "Approve."

The approval handler:

1. Re-runs Decide to check current policy (in case a hard deny rule was added since the pending was created). If it would now be hard denied, approval fails.
2. If still not hard denied: marks the pending_action as approved and creates a **one-shot capability**: `(run_id=run_88, generation=1, connector=bank, tool=transfer_funds) → approved once`.
3. Writes an audit event recording the approval.



## Agent retries the tool call

The agent (if well-written) retries the `transfer_funds` call. Runner-A sends the same MCP tools/call request again.

The CP receives it. Decide runs again. This time, before consulting grants/webhook, it checks: is there a one-shot capability for (run_88, gen=1, bank, transfer_funds)? YES. It consumes (deletes) the capability and short-circuits: **allow this one call**.

The CP now forwards the actual request to the upstream banking service, injecting real credentials. The transfer executes. The response flows back to the runner.

## Third attempt (no more capability)

If the agent calls `transfer_funds` a THIRD time (different transfer), there is no capability remaining. Decide runs normally. The webhook/mandatory HITL will likely return pending again. A new pending_action is created with a new action_id. The cycle repeats.

The one-shot capability is deliberately ONE SHOT to prevent: approve once → agent makes unlimited calls forever. Every dangerous call needs its own approval.

## What if the approval comes after the run is already done?

If the agent did not wait and the run already completed (or errored), the approval still happens in the pending_actions table. But since the run is done, there is no agent to retry the call. The approval is "wasted." This is not a bug — it is the expected outcome when agents do not handle pending gracefully.

---



# 67. Extended Narrative — A Full Cancel Story Across Replicas



## Setup

Run `run_55` is executing on Runner-A. Runner-A's gRPC connection goes to CP-2. The client's SSE connection is on CP-1. An admin decides to cancel the run via the Admin UI, which is on CP-3.

## The cancel flow

1. **Admin clicks cancel on CP-3.** CP-3's admin API handler validates admin auth and calls `PublishCancel(run_55)` on the CancelBroker.
2. **Redis Pub/Sub receives the signal.** CP-3 publishes to Redis channel `cancel:run_55`.
3. **CP-2 receives the signal.** When Runner-A's GetJob was served on CP-2, CP-2 created a subscription to `cancel:run_55`. That subscription goroutine on CP-2 fires now. It calls `notifyWatchers("python-langgraph", "run_55")` — this writes to an in-memory channel that Runner-A's WatchCancels stream is reading from.
4. **Runner-A receives cancel.** Runner-A's WatchCancels goroutine reads the cancel signal from the gRPC stream. It sets a cancellation flag that the heartbeat loop and execution loop check.
5. **Runner-A cooperatively stops.** The agent framework's execution is cancelled (asyncio task cancelled, or similar). Partial results may be discarded. The heartbeat loop stops.
6. **Runner-A reports interrupted.** Runner-A calls ReportStatus with `run_55, status=interrupted, generation=1`. This hits some CP (maybe CP-1 via the LB). CP-1 Acks the inflight record in Redis (generation 1 is current — Ack succeeds). CP-1 updates Postgres: run=interrupted, thread=idle. Publishes terminal event to Redis Stream.
7. **Client sees interruption.** CP-1's SSE connection (which the client has open) receives the terminal event from Redis. Sends the final SSE frame. Client knows the run was cancelled.

Notice: the cancel signal traveled through THREE different replicas and arrived correctly. CP-3 published (admin), CP-2 routed (subscriber from GetJob), and CP-1 finalized (status update from runner). This works because:

- Redis Pub/Sub is the cross-replica signal bus
- The subscription was created at GetJob time on the dispatching CP
- WatchCancels is co-located with the subscription (same gRPC connection)
- Final status update uses shared Postgres (any CP can write)

---



# 68. Extended Narrative — What Exactly the Boot Sequence Looks Like in Logs

When you start `runkite serve`, you will see logs roughly like this (I am writing them in natural language, not literal log format):

```
Starting Runkite v0.2.0 in serve mode
Checking production admission requirements...
  State backend: postgres (POSTGRES_DSN configured)     OK
  Transport: redis (REDIS_URL configured)               OK
  Runner tokens: 2 kind(s) configured                   OK
  Client auth: jwt configured                           OK
All admission checks passed.

Connecting to Postgres... connected
Running database migrations... 47 migrations applied, schema up to date
Connecting to Redis... connected, ping OK

Initializing components:
  Auth providers loaded (jwt + 1 admin key)
  Runner tokens: python-langgraph, typescript-langgraph
  Policy engine: webhook configured (http://opa:8181/decide), 3 static grants loaded
  Connectors: github, salesforce, slack (3 configured)
  Rate limits: redis backend, 5 rules loaded
  Admin sessions: redis-backed

Starting background tasks:
  Reclaim loop started (tick=2s, maxAge=6s)
  Cron scheduler started (tick=15s, 2 schedules registered)
  Retention cleanup started (tick=1h)
  Policy overlay poll started (tick=15s)
  Queue depth metrics poll started (tick=10s)

gRPC bridge registered (RunnerService)
HTTP server listening on :2026
gRPC server listening on :2027
Admin UI available at http://localhost:2026/admin/
Ready.
```

If ANY admission check fails, you instead see:

```
Starting Runkite v0.2.0 in serve mode
Checking production admission requirements...
  State backend: MISSING (no POSTGRES_DSN, MYSQL_DSN, or MONGO_URI)  FAIL
  Transport: MISSING (no REDIS_URL, NATS_URL, or KAFKA_URL)          FAIL
  Runner tokens: MISSING (no RUNNER_TOKEN_* variables)                FAIL
  Client auth: MISSING (no auth section in langgraph.json)            FAIL

FATAL: production admission failed. Configure the above or set RUNKITE_ALLOW_INSECURE_SERVE=1.
Exit 1.
```

The process never starts listening. No traffic is served. This is fail-closed admission in action.

---



# 69. Extended Teaching — Why Tool Idempotency Matters Even With Fencing

Fencing (generation) prevents a stale runner from overwriting the WINNER'S result in Postgres. But fencing cannot prevent a stale runner from calling EXTERNAL APIs before it learns it is superseded.

Here is the scenario:

1. Runner-A is executing run_77, generation 1.
2. Network blip: heartbeats stop reaching the CP for 7 seconds.
3. Reclaim fires: generation bumped to 2, job requeued.
4. Runner-B picks up generation 2 and starts executing.
5. Runner-A's network recovers. It has NOT yet received "superseded" because it has not sent a heartbeat yet (that happens on the next 2-second tick).
6. In this window (before the next heartbeat), Runner-A calls the connector proxy to execute `transfer_funds`.
7. The connector proxy checks run-binding: run_77, generation 1. But wait — the inflight record now has generation 2 (because reclaim bumped it). Generation 1 is stale. The connector call is REJECTED with `run_generation_mismatch`.

Actually, in this specific case, run-binding DOES catch it because generation fencing applies to connector calls too. But what if the runner calls an external API DIRECTLY (bypassing the connector proxy)? Like a raw `requests.post("https://api.bank.com/transfer")` inside agent code? The control plane never sees this call. It cannot fence it.

This is why: even with fencing, external tool calls should be idempotent. If a transfer is repeated, it should detect the duplicate and no-op (using idempotency keys, unique transaction IDs, etc.). The control plane cannot make external APIs safe — only the tools themselves can be designed for safe retry.

---



# 70. Extended Teaching — Event Replay and SSE Reconnection



## The problem

A client is receiving SSE events from CP-1. CP-1 crashes (or the client's network blips). The SSE connection drops. The client reconnects, possibly to a different CP.

## How replay works

Redis Streams have a natural ordering: each entry has an ID (timestamp-based). When the client reconnects and opens a new SSE stream, the client (or the CP's SSE handler) can resume from the last received event ID. The handler does XREAD from that offset, replaying any events that were missed during the disconnection.

If the client reconnects to a DIFFERENT CP (say CP-3), that is fine — CP-3 subscribes to the same Redis Stream from the same offset. Events that were published during the disconnection are still there (until Redis retention expires).

## What if the run already completed?

If the run completed while the client was disconnected, the terminal event is in the Redis Stream. The client's reconnection subscription will receive it. Additionally, the client can always GET the run from Postgres to see the final status (the authoritative truth).

## Teaching point

SSE is not "more reliable" or "less reliable" than WebSocket for event delivery. Both depend on the same Redis fan-out. Both lose the socket when their owning CP dies. Both reconnect and replay. The difference is: SSE is simpler (one-way, standard HTTP), WebSocket is more flexible (bidirectional, commands+events on one channel).

---



# 71. Extended Teaching — Why `runkite dev` Exists Separately



## The developer experience philosophy

When you are building an agent locally, you do not want to:

- Set up Postgres
- Set up Redis
- Configure JWT auth
- Create runner tokens
- Wait for containers to start

You just want to: write your agent code, run it, see if it works.

`runkite dev` gives you this: one command, zero dependencies, instant start. It uses SQLite (file on disk) and in-process transport (everything in one process). No auth required. No containers needed.

The trade-off is clear: `dev` is not production-safe. It cannot do multi-replica HA. It has no security. But it boots in under a second and lets you iterate on agent logic without infrastructure concerns.

When you are ready for production, switch to `serve` — which forces you to configure everything properly (or explicitly opt out of safety checks). The transition is: same code, different deployment config.

---



# 72. Extended Teaching — Why We Support Multiple State and Transport Backends



## State backends: Postgres, MySQL, SQLite, MongoDB

Different organizations have different infrastructure. Some run Postgres. Some run MySQL. Some want to try things locally with SQLite. Some have existing MongoDB deployments.

By supporting multiple backends behind a common interface (`state.Store`), Runkite can be adopted without forcing infrastructure changes. The trade-offs are clear:

- Postgres: Supported tier, all features, recommended for production
- MySQL: Compatible tier, most features, missing some governance edge cases
- SQLite: Local/dev only, single process
- MongoDB: Compatible tier for basic agent execution, but NO governance durability (returns 501 on admin governance routes)



## Transport backends: Redis, NATS, Kafka, In-process

Similarly:

- Redis: Supported tier, full triad (JobQueue + EventBroker + CancelBroker), recommended
- NATS: Compatible tier, full triad with a known Close() semantic gap
- Kafka: Compatible for JobQueue only, needs Redis alongside for EventBroker/CancelBroker/reclaim-leader
- In-process: Dev only, single process, no multi-replica



## Why conformance matters

With multiple backends, you MUST have shared tests that prove they all behave the same. Otherwise "we support Kafka" could mean "it compiles with Kafka" rather than "reclaim, cancel, and event delivery all work correctly on Kafka." Conformance suites are that proof.

---



# 73. The Complete File Map With Explanations

Here is every important path in the repository with a one-sentence explanation of what you will find when you open it.

**Control plane core:**

- `cmd/main.go` — The entry point. Dispatches between dev/serve/db/vector commands.
- `cmd/serve.go` — The production boot sequence. Connects to databases, starts background tasks, begins listening.
- `cmd/dev.go` — The development mode. Zero-dependency local startup.
- `internal/api/server.go` — The HTTP server struct. Holds all dependencies and registers routes.
- `internal/api/runs.go` — The most important file. Contains createRunCtx (the run creation funnel), stream handlers, wait handlers, cancel handlers.
- `internal/api/streaming.go` — SSE implementation details.
- `internal/api/websocket.go` — WebSocket command parsing and event dispatch.

**Runner Protocol:**

- `proto/runner.proto` — The gRPC schema defining the five RPCs.
- `runner-protocol/PROTOCOL.md` — Prose documentation of the protocol.
- `internal/bridge/server.go` — Go implementation of the five RPCs.
- `internal/bridge/interceptors*.go` — gRPC middleware for runner authentication.

**Transport layer:**

- `internal/transport/` — Interface definitions (JobQueue, EventBroker, CancelBroker).
- `internal/transport/redis/` — Redis implementation (Supported).
- `internal/transport/nats/` — NATS implementation (Compatible).
- `internal/transport/kafka/` — Kafka JobQueue (needs Redis for other components).
- `internal/transport/inprocess/` — Single-process implementation (dev mode).
- `internal/transport/conformance/` — Shared test suites all backends must pass.

**State layer:**

- `internal/state/` — Interface definition (Store) and all backend drivers.
- `internal/state/postgres/` — Postgres implementation with migrations.

**Authentication and authorization:**

- `internal/auth/` — Client auth (JWT, API keys, webhook), admin sessions, runner tokens.
- `internal/auth/runbind.go` — Run-binding: derives identity from inflight assignment.
- `internal/auth/adminsession*.go` — Admin session management (cookies, CSRF).

**Policy and governance:**

- `internal/policy/` — Policy engine: Decide function, grants evaluation, webhook client, cache, audit hooks.
- `cmd/policy.go` — Policy engine initialization at boot.
- `cmd/policy_overlay_poll.go` — Background 15s SQL poll for cross-replica sync.
- `internal/api/admin_*.go` — Admin API for grants, kill, break-glass, pending, audit, mandatory HITL.

**Connectors:**

- `internal/connector/` — Session management, MCP proxy, circuit breaker, OAuth token refresh.

**Background tasks:**

- `cmd/reclaim.go` — Stale job recovery (reclaim loop).
- `cmd/cron.go` — Scheduled run creation.
- `cmd/retention.go` — Old data cleanup.
- `cmd/run_timeout.go` — Wall-clock deadline enforcement.

**Other infrastructure:**

- `internal/ratelimit/` — Rate limiting middleware and backends.
- `internal/hooks/` — Preflight and observational webhook hooks.
- `internal/tracing/` — OpenTelemetry setup.
- `internal/cors/` — CORS rules.
- `internal/secureheaders/` — Security response headers.
- `internal/tenant/` — Tenant context helpers.

**Runners:**

- `python/runkite_runner/` — Python runner: worker.py (main loop), heartbeat.py, connectors.py, a2a.py, store.py, checkpoint.py.
- `typescript/runkite-runner/` — TypeScript runner (mirrors Python).
- `python/adapters/` — Framework adapters: CrewAI, LlamaIndex, AutoGen, LangChain.

**Admin UI:**

- `admin-ui/` — React source for the Admin interface.
- `internal/adminui/` — Go embed of the built UI.

**Deployment:**

- `deploy/helm/runkite` — Helm chart for Kubernetes.
- `docker-compose.multi.yml` — Multi-replica HA topology for soak testing.
- `Dockerfile*` — Container images (non-root).

**Documentation:**

- `docs/architecture.md` — Public architecture (tiers, diagrams).
- `docs/trust-governance.md` — Public governance documentation.
- `docs/limitations.md` — Public honest gaps and known limitations.
- `docs/api.md` — API reference.
- `docs/configuration.md` — Configuration reference.
- `examples/policy_webhook/` — Reference PDP implementation for testing.
- `scripts/smoke-governance.sh` — Governance smoke test script.

---



# 74. Closing — What You Should Be Able To Do Now

After reading this complete document, you should be able to:

1. **Explain the architecture** to someone else: "Client speaks HTTP to a Go control plane. Control plane dispatches work via Redis to Python/TS runners over gRPC. Runners execute agent code and stream results back. Control plane fans out events to clients via SSE. Postgres is truth, Redis is coordination."
2. **Trace a run** through the system: which functions are called, which databases are touched, in which order, by which process.
3. **Explain crash recovery** with the specific numbers: 2s heartbeat, 6s stale, generation fencing, superseded response, exactly one winner writes the final status.
4. **Distinguish the two HITLs** without mixing vocabulary: framework interrupt (agent decides, user resumes) vs connector pending (policy decides, admin approves, agent retries).
5. **Describe governance** with a concrete example: this tenant, this agent, this connector, this tool — allow/deny/pending, from where, with what audit trail.
6. **Name the honest gaps** without hedging: Mongo governance, Kafka HA, /mcp sticky, non-terminal non-fencing, A2A breadth race, direct-mode trust.
7. **Find any file** in the codebase given a product concept: "where is run creation?" → internal/api/runs.go. "Where is reclaim?" → cmd/reclaim.go. "Where is policy?" → internal/policy/.
8. **Read production logs** and understand what each key message means.
9. **Draw the HA diagram** on a whiteboard: three CPs, one Postgres, one Redis, one LB, runners. Label what flows over each arrow.

If any of the above is still fuzzy after reading this document, re-read the specific chapter and let me know what needs more clarity. I will expand that section further.

---



# 75. Extended Analogy — The Airport

The airport analogy helps connect many concepts at once. Return to it whenever something feels abstract.

**Postgres is the airline reservation system.** It knows: which flights exist (agents), which passengers are booked (runs), which gates are occupied (threads busy/idle), the history of all past flights (audit trail). If the airport burns down and you rebuild, this database tells you everything that ever happened.

**Redis is the departures board and the gate assignment system.** It shows: which flights are boarding NOW (inflight leases), which passengers are waiting at which gate (job queue), which announcements are currently being broadcast (event streams), and which flights were just cancelled (cancel pub/sub). If the departures board crashes and restarts empty, people are confused for a few minutes but the airline reservation system still knows the truth.

**The load balancer is the airport entrance.** Passengers (clients, runners) enter through any door and get directed to an available desk (CP replica). It does not matter which door you use — all desks have access to the same reservation system.

**A CP replica is a check-in desk.** Each desk can handle any passenger for any flight. Desks do not own specific flights. If a desk closes (pod dies), passengers go to another desk and their booking is still there.

**A runner is a pilot.** Pilots sit in the crew room (GetJob long-poll) waiting for a flight assignment. When assigned, they go to the gate (start execution), fly the plane (run the agent), report back on arrival (ReportStatus). If a pilot collapses mid-flight (crash), the airline reassigns the flight to another pilot (reclaim + new generation).

**Generation is the version number on a boarding pass.** When a flight is reassigned (reclaim), new boarding passes are issued with a higher version number. If the old pilot shows up with version 1 pass, they cannot board — version 2 is current. The old pass is "superseded."

**A heartbeat is the pilot checking in with ATC.** Every few seconds, the pilot radios "still here, still flying." If radio silence exceeds the timeout, ATC assumes the plane is lost and assigns a rescue flight.

**Cancel is the airline deciding to stop a flight mid-air.** The signal goes from the airline operations center (client/admin) through the radio tower (Redis Pub/Sub) to the ATC desk monitoring that flight (the subscribed CP) to the pilot (WatchCancels). The pilot cooperatively returns to the nearest airport (stops execution).

**Governance is airport security.** Before a pilot can access the fuel depot (connector), security (Decide) checks: does this pilot (agent) have a pass (grant) for this fuel type (connector+tool)? If not, denied. If suspicious, held for manual review (pending HITL). Security logs every check (audit).

**Break-glass is an emergency runway override.** The fire chief (admin) declares an emergency and planes can land without normal clearance — but every landing is recorded on camera (audit). The override expires after a set time and normal rules resume.

---



# 76. Extended Analogy — The Restaurant Kitchen

**The client is a diner** placing an order with the hostess.

**The control plane is the hostess + order system + kitchen management.** It takes orders (creates runs), writes them to the ticket system (Postgres), puts tickets on the rail (Redis queue), and manages which cook gets which ticket.

**The runner is a cook** at a specific station (runner_kind). The cook takes a ticket from the rail (GetJob), prepares the dish (executes the agent), calls out progress to the pass (StreamEvents), and signals when plating is done (ReportStatus).

**Fan-out is the pass window.** One cook puts a plate on the pass. Any server (SSE on any replica) walking by the pass can pick it up and deliver it to the correct table (client).

**Reclaim is the expeditor.** If a cook disappears from their station and a ticket has been sitting too long, the expeditor takes the ticket, assigns it to a new cook with a new ticket number (generation), and the dish gets made by someone else. If the original cook returns and tries to plate their version, the expeditor says "too late, someone already plated this order" (superseded).

**Kill switch is 86ing an item.** "86 the salmon" means no more salmon orders accepted tonight. Every new order for salmon gets refused.

---



# 77. Extended Teaching — Reading `docs/limitations.md` As a Feature

Most products hide their gaps. Runkite lists them publicly. This is a deliberate trust signal.

When an engineer evaluates Runkite, they will search for limitations. Finding a clear, honest document that says "Kafka needs Redis, Mongo lacks governance, /mcp needs sticky" builds MORE trust than finding nothing (which makes them suspicious of hidden problems).

Every time you discover a new limitation during development (like the A2A breadth race), the practice is:

1. Fix it if practical
2. If not practical to fix now, add a row to limitations.md
3. Reference it in architecture.md if it affects tier claims
4. Mention it in this study document for founder awareness

Never quietly hope reviewers will not notice. They will, and the discovery will damage trust more than the limitation itself.

---



# 78. Extended Teaching — Why Five gRPC RPCs Are Not Six or Four



## Why not four (removing WatchCancels)?

Without WatchCancels, the only way to cancel a running agent is: wait for the next heartbeat to inform the runner (up to 2 seconds), or rely on some other polling mechanism. Two seconds is too long for user-facing cancel UX. WatchCancels provides immediate signal delivery.

## Why not six (adding a separate CheckpointSync RPC)?

Checkpoints go through the store HTTP API or direct Postgres. Adding a gRPC RPC for checkpoints would mean duplicating the store abstraction in proto, handling all the backend variations in the gRPC layer, and running checkpoint data through the bridge. The HTTP path is simpler and already works. Not everything needs to be gRPC.

## Why not merge Heartbeat into StreamEvents?

You could imagine: "the act of sending events proves you are alive." Early versions did this (StreamEvents renewal). But the problem: LLM calls can be SILENT for 30+ seconds (thinking, long context processing). No events means no heartbeat means false reclaim. The separate Heartbeat RPC lets the runner prove liveness even during silent computation phases.

---



# 79. Extended Teaching — What "Supported" vs "Compatible" vs "Experimental" Actually Means for Decisions

When choosing your stack:

**Supported (Postgres + Redis):** You can deploy this with confidence. Multi-replica HA is soak-tested. All features work. Governance is fully operational. The team tests fixes against this combination first.

**Compatible (NATS triad, MySQL, SQLite governance, Kafka+Redis, Helm):** Works correctly based on conformance tests and limited operational evidence. May have documented behavioral differences (like NATS Close gap). You can use it in production if you understand and accept the documented caveats. Less tested under extreme conditions than Supported.

**Experimental (Kafka without Redis):** Compiles. May pass some tests. Has known issues with multi-replica correctness (double-dispatch, missing fan-out). Use only for evaluation or with heavy caveats. Not production-recommended.

The promotion path: Experimental → Compatible → Supported requires: conformance green, multi-replica live proof, residual list empty enough for production, documentation updated, soak automation added. Wanting a tier upgrade is not a criterion — evidence is.

---



# 80. Extended Teaching — The Four Questions to Ask Any New Feature

When anyone (including yourself) proposes a new feature for the control plane, ask:

1. **Does this need to exist at all?** (YAGNI — maybe the use case is imagined, not real)
2. **Does it touch the hot path?** (createRunCtx, GetJob, StreamEvents, Heartbeat, Decide — adding latency here affects every single run)
3. **Does it work with only Postgres + Redis?** (If it requires a new dependency, justify it strongly)
4. **What happens when it fails?** (Fail-closed or fail-open? What is the blast radius? Does it leave zombie state?)

Features that cannot answer these clearly should not be built yet.

---



# 81. Deep Dive — Factory Graphs and ServerRuntime



## What problem factory graphs solve

In a typical LangGraph setup, you define a graph (a StateGraph with nodes and edges), compile it, and the runner executes it. This compiled graph is a singleton — one instance shared across all runs. That works fine for stateless agents.

But some agents need per-run isolation. Imagine an agent that uses middleware instances (rate limiters, session caches, auth tokens) that MUST be separate per user. If two users are running the agent simultaneously and they share the same middleware instances, one user's rate limit state leaks into another user's context. That is a correctness bug and a security issue.

Factory graphs solve this: instead of a pre-compiled singleton graph, you export a FACTORY FUNCTION that creates a fresh graph per run. Each run gets its own isolated instance.

## The three export styles

When you write `graph.py` for your agent, your export can be one of three things:

**Style 1: A compiled graph (or StateGraph).** The runner detects it is already compiled, calls `.astream()` on it directly. One instance shared across all concurrent runs. This is what most simple agents use.

**Style 2: A zero-argument callable.** Called ONCE at worker startup. Returns a compiled graph. Use case: you need one-time setup (like connecting to an MCP server) that should happen once and be reused. The returned graph is then shared like Style 1.

**Style 3: A factory function accepting** `config`**,** `runtime`**, or both.** Called FRESHLY PER RUN. The function inspects the user's identity (from `runtime.user`), creates a per-user graph instance, and returns it. Each run gets its own isolated graph. This is the pattern for multi-user agents with per-user middleware.

## How the runner decides which style you are using

The runner inspects your function's parameter names (using Python's `inspect` module). If your function takes parameters named `config` and/or `runtime`, it is treated as a factory. Otherwise it is treated as Style 1 or 2.

This is not a Runkite invention — it is LangGraph's own documented convention for their Platform product. Any `graph.py` written for LangGraph Platform works here unchanged because Runkite implements the same ServerRuntime API.

## ServerRuntime — what it provides to your factory

When your factory function receives a `runtime` parameter, it is a `ServerRuntime` object with:

- `runtime.user` — The authenticated user making this run request (identity, display_name, permissions, extra fields from auth). Populated from the RunAssignment's user field, which came from the CP's auth middleware. If no auth is configured, `runtime.user` is None.
- `runtime.ensure_user()` — Raises an error if user is None. For factories that REQUIRE authentication.
- `runtime.config` — The RunnableConfig with run-specific settings.
- `runtime.store` — Access to the Runkite KV store (Chapter 32).
- `runtime.a2a` — Agent-to-agent delegation helper (Chapter 9).



## Example: a factory graph that uses per-user credentials

```python
async def graph(config, runtime):
    user = runtime.ensure_user()
    # Create a middleware instance scoped to THIS user's OAuth token
    github_client = GitHubClient(token=user.extra["github_token"])
    # Build a graph with this user-specific client
    builder = StateGraph(AgentState)
    builder.add_node("search", make_search_node(github_client))
    builder.add_node("summarize", summarize_node)
    builder.add_edge("search", "summarize")
    return builder.compile()
```

Each run gets its own `GitHubClient` with the specific user's token. No cross-user leakage.

## Known limitation

`runtime.user` carries whatever the configured auth provider resolved. With no auth configured (dev mode), it is None. A factory that calls `runtime.ensure_user()` will crash in dev mode unless you configure at least API key auth. This matches LangGraph Platform's documented behavior.

**Code:** `python/runkite_runner/factory_graph.py`, `typescript/runkite-runner/src/factoryGraph.ts`, `docs/factory-graphs.md`

---



# 82. Deep Dive — LLM Response Cache



## What it does

Some agents produce deterministic outputs for identical inputs. If you send "What is 2+2?" to a math agent three times, you get the same answer each time. Without caching, each invocation dispatches a full run (queue, runner, LLM call, billing). With caching, the second and third requests get an instant response from the cache — no runner involved, no LLM call billed.

## How it works

The control plane (NOT the runner) implements this cache. During `createRunCtx`:

1. The CP computes a cache key: `hash(agent_id + input + relevant_config)`.
2. It looks up this key in the `cached_run_results` table (SQL state backend).
3. If a valid (not expired) entry exists: return the cached output IMMEDIATELY. No run is created, no job is queued, no runner is involved. The client gets a response as if the run completed instantly.
4. If no cache entry: proceed normally (create run, dispatch, execute).
5. When the run completes successfully, the CP writes the result to the cache table with the configured TTL.



## Why this lives at the control plane level, not the runner level

The CP never sees inside a runner's execution (it is framework-agnostic). It cannot cache individual LLM calls within a complex graph. But it CAN cache the whole-run input→output mapping, which is sufficient for the common case of repeated identical queries.

If an agent has side effects (sends emails, creates tickets, transfers money), caching would be dangerous — replaying a cached "success" without actually performing the action means the action never happens. Therefore: caching is OFF by default and must be explicitly opted-in per agent.

## Configuration

In `langgraph.json`:

```json
{
  "llm_cache": {
    "my-math-agent": { "ttl_minutes": 60 },
    "my-search-agent": { "ttl_minutes": 5 }
  }
}
```

Each agent gets its own TTL (or is absent = no caching). The cache is stored in the SQL state backend (Postgres/MySQL/SQLite) alongside other run data.

## When to use

- Agents that answer reference questions (FAQ bots, documentation assistants)
- Agents with expensive LLM calls and predictable outputs
- Development/testing (avoid burning API credits on repeated manual tests)



## When NOT to use

- Agents with side effects (email, tickets, payments, tool calls that mutate external state)
- Agents whose output depends on external real-time data (stock prices, live feeds)
- Agents where freshness matters more than cost (news summarizers)

**Code:** `internal/models/models.go` (CachedRunResult type), `internal/api/runs.go` (cache check in createRunCtx), `internal/state/*/` (SQL storage), config in `internal/config/loader.go`

---



# 83. Deep Dive — mTLS (Mutual TLS)



## What TLS does (one direction)

Normal TLS (what HTTPS uses) proves the SERVER's identity to the client. When your browser connects to `https://runkite.company.com`, the server presents a certificate saying "I am runkite.company.com, signed by a trusted CA." Your browser verifies this and encrypts traffic. But the server has no proof of who the CLIENT is — any client can connect.

## What mTLS adds (both directions)

Mutual TLS (mTLS) adds the reverse: the CLIENT also presents a certificate to the server. The server verifies it against a trusted CA. Now BOTH sides have cryptographic proof of each other's identity.

For Runkite, this means: a runner connecting to the gRPC port must present a client certificate signed by a CA that the control plane trusts. Without a valid certificate, the TCP connection is refused before any application-level code runs. This is defense-in-depth on top of runner tokens.

## Why you would want this

Runner tokens (Chapter 2) are bearer tokens — anyone who has the token string can authenticate. If a token leaks (logged accidentally, exposed in a config file, intercepted), an attacker can impersonate a runner.

mTLS adds a second factor: you need the token AND a valid client certificate (which requires the corresponding private key). Compromising either one alone is not enough. The private key is typically stored in a hardware security module, Kubernetes secret, or certificate-issuing infrastructure (cert-manager, Vault) — much harder to exfiltrate than a string in an environment variable.

## How Runkite implements it

The same `cmd/tls.go` function (`serverTLSConfig`) handles both HTTP and gRPC server TLS. Three env vars control it:

- `RUNKITE_TLS_CERT` — Server certificate file (enables TLS)
- `RUNKITE_TLS_KEY` — Server private key file
- `RUNKITE_TLS_CLIENT_CA` — Client CA file (enables mTLS)

If only cert+key are set: regular TLS (server auth only). If client CA is also set: mTLS — any client (runner or otherwise) must present a certificate signed by that CA.

The runner side needs corresponding configuration: the runner's gRPC client is configured with a client certificate that the server's CA will accept. Python runners use `tls_utils.py`, TypeScript runners use `tls.ts`.

## Deployment pattern

In a Kubernetes environment with cert-manager:

1. Create a CA (self-signed or from your PKI)
2. Issue a server certificate for the control plane (mounted as a Secret)
3. Issue client certificates for each runner kind (mounted as Secrets into runner pods)
4. Set the three env vars on the CP deployment
5. Configure runners to present their client cert

In the Helm chart (`deploy/helm/runkite/values-tls.yaml`), all of this is pre-wired — you just provide the Secret names.

**Code:** `cmd/tls.go` (builds `*tls.Config`), `python/runkite_runner/tls_utils.py`, `typescript/runkite-runner/src/tls.ts`, `deploy/helm/runkite/values-tls.yaml`

---



# 84. Deep Dive — Agent Versioning and Rollback



## What versioning means

Every time you update an agent's configuration (change its graph code, update its model, modify its tools), the control plane creates a new version of that agent. The old version is not deleted — it stays in Postgres. You can list all versions, inspect any previous version, and roll back to it.

## How it works

The agents table in Postgres stores agents with a `version` integer. When you start the control plane and it bootstraps agents from `langgraph.json`, it compares the current configuration hash against the last stored version. If they differ, a new version is created (version number incremented). If they are identical, no new version — idempotent.

API endpoints:

- `GET /agents/{id}/versions` — List all versions of an agent (newest first)
- `GET /agents/{id}/versions/{version}` — Get a specific version's config
- `POST /agents/{id}/rollback` — Revert to a previous version



## What "rollback" actually does

Rollback does NOT rewind time or undo past runs. It copies the old version's configuration into a NEW version entry (with a higher version number). So after rollback, the version history looks like: v1 → v2 → v3 (which is the same config as v1). Future runs use the latest version.

Existing in-flight runs are not affected — they were dispatched with a snapshot of the config at create time. Only NEW runs pick up the rolled-back configuration.

## Why this matters

In production, you deploy a new agent version and discover it is performing worse (hallucinating more, taking longer, using wrong tools). Without versioning, you have to manually reconstruct the old config and redeploy. With versioning, you call one API endpoint and the old behavior is restored in seconds.

**Code:** `internal/api/agents.go` (version/rollback handlers), `internal/state/` (agent version storage)

---



# 85. Deep Dive — MCP Server Mode (Exposing Agents AS MCP Tools)



## What this feature does

Connectors (Chapter 12) let Runkite agents CONSUME external MCP tools (agents call out to GitHub, Salesforce, etc). MCP Server mode is the REVERSE: it exposes Runkite's own agents AS MCP tools, so external MCP clients can call them.

Think of it this way: if you have an IDE (like Cursor, VS Code with Continue, or Zed) that speaks MCP, you can point it at Runkite's `/mcp` endpoint. Each configured agent appears as an MCP tool. Calling that tool dispatches a real Runkite run and returns the result.

## How it works

The control plane mounts a standard MCP server at `POST /mcp` (and `GET /mcp` for SSE, `DELETE /mcp` for session teardown). This endpoint goes through the SAME auth middleware as every other client-facing endpoint — API keys, JWT, the works.

When an MCP client connects:

1. It calls `tools/list` — the control plane returns one tool per configured agent (agent name as tool name, description from the agent config)
2. It calls `tools/call` with a tool name and arguments (typically a `message` string) — the control plane:
  a. Maps the tool name to an agent_id
   b. Calls `createRunCtx` (same path as a normal HTTP run creation)
   c. Waits for the run to complete (same as the synchronous wait endpoint)
   d. Returns the agent's output as the MCP tool result

From the MCP client's perspective, it is just calling tools. It has no idea there is a queue, a gRPC runner, a Redis Stream, or any of the internal machinery. It just sees: "I called a tool and got a response."

## Input schema

Every Runkite-agent-as-MCP-tool accepts the same input shape:

- `message` (string) — wrapped into a single-user-turn input for the agent
- `input` (object, optional) — escape hatch for callers that know the agent's native input format
- `thread_id` (string, optional) — for multi-turn conversations



## Why it is request/response only (no streaming)

MCP tool calls are inherently request/response: you call a tool and get back a result. There is no streaming variant in the MCP protocol for tool calls. So even if the underlying agent streams tokens internally, the MCP response waits until the run completes and returns the final output. This is a protocol limitation, not a Runkite limitation.

## Why /mcp needs sticky routing in multi-replica

MCP sessions store state in process memory (which MCP tools are registered, session ID tracking). The first request (`initialize`) creates a session on one CP — say CP1. CP1 stores the session in an in-memory map and returns a `Mcp-Session-Id` header. Every subsequent request for that session MUST reach CP1, or it fails with 403 "session unknown."

**How the LB ensures stickiness:** The nginx config uses `hash $remote_addr consistent` for the `/mcp` upstream. This means the same client IP always maps to the same CP. As long as the client's IP doesn't change, every request lands on the same replica.

**Security: session-to-caller binding.** The CP records WHO created each session (tenant + identity). If a different caller presents a stolen session ID with their own valid credentials, the CP rejects it — "session belongs to a different caller." This closes a real cross-tenant hijack confirmed live during testing (the MCP SDK's own session protection doesn't work with Runkite's auth providers).

**Session lifecycle:** Sessions have a 30-minute TTL (refreshed on every request). The MCP SDK's own idle timeout is set to 25 minutes (30min minus one sweep interval), ensuring the SDK closes the session BEFORE the ownership record expires. DELETE /mcp explicitly closes a session.

## Who would use MCP Server mode?

MCP is becoming the standard protocol for AI tools. Practical users:

- **IDE users:** Cursor, VS Code + Copilot, Claude Desktop — all speak MCP natively. Point them at Runkite's `/mcp` endpoint and your agents appear as tools in the IDE. No custom UI needed.
- **AI orchestration systems:** Another AI system can call your agents programmatically over a standard protocol instead of building custom API integrations.
- **Cross-company integration:** A partner's AI system calls your agents as MCP tools without needing to understand your internal API.

## Does multi-replica work for MCP?

Yes. The sticky routing pins each client session to one CP. Inside that CP, tool calls go through the same `createRunCtx → waitForRunResult` path. Runs dispatch to runners, events flow through Redis, reclaim works, everything functions identically. The ONLY difference is that MCP is request/response (call tool → block → get complete result), so it internally uses the "wait" handler, not the "stream" handler.

## Can you use all Runkite features via MCP?

Almost all. What works (because `createRunCtx` is the same funnel): auth, multi-tenancy, rate limiting, policy enforcement, A2A delegation, agent aliases, governance, kill switches, admission limits, audit trail.

What's limited (MCP protocol constraints, not Runkite limitations):
- **No streaming** — MCP tools/call is request/response. The client gets the complete answer after the run finishes, not token-by-token.
- **No HITL/interrupt** — MCP has no mechanism for "pause and ask the user a question mid-run." If the agent hits an interrupt, the MCP tool call returns "interrupted" as an error.
- **Thread management is simplified** — auto-generated thread IDs by default. Pass `thread_id` explicitly for multi-turn conversations.

## The two MCP directions (avoid confusing them)

```
Direction 1: Connectors (agent CONSUMES external MCP tools)
  Agent code → Runner → CP proxy (/internal/connectors/*/mcp/*) → External MCP server (GitHub, Slack)
  CP enforces policy, injects credentials. Runner never has direct access to external service.

Direction 2: MCP Server mode (agent IS EXPOSED as MCP tool)
  External MCP client (IDE, desktop app) → CP (/mcp) → createRunCtx → Runner executes agent
  External client gets agent's output as a tool result.
```

These are independent features. You can use one, both, or neither. An agent can simultaneously consume external MCP tools (via connectors) AND be exposed as an MCP tool itself (via /mcp).

**Code:** `internal/api/mcpserver.go` (MCP Server mode), `internal/connector/mcpproxy.go` (Connectors MCP proxy), `deploy/nginx-multi.conf` (sticky routing for /mcp), `docs/mcp-server.md`

---



# 86. Deep Dive — Deployment: Helm, Docker Compose, and kind



## How Runkite ships

Runkite is a single Go binary. The compiled binary (with the Admin UI embedded) is published as a container image at `ghcr.io/getrunkite/runkite:<version>`. You can run it anywhere: bare metal, a VM, Docker, Kubernetes.

## Docker Compose (development and testing)

For local development and CI testing, docker-compose files orchestrate the full stack:

`docker-compose.yml` — Minimal single-replica setup: one CP, one Postgres, one Redis, one Python runner. Good for "does it work?" validation.

`docker-compose.multi.yml` — Multi-replica HA topology: THREE control plane instances, shared Postgres, shared Redis, a load balancer (nginx), and runner(s). This is used for soak testing: proving that multi-replica coordination (fan-out, cancel across replicas, reclaim) works under sustained load. The `bench/` directory has load generators that pound this topology.

Key detail in the multi-replica compose: `stop_grace_period: 30s` ensures that when you `docker compose down`, the containers get a graceful shutdown window (drain connections, finish inflight) before SIGKILL. Without this, in-flight runs would be interrupted mid-execution during deployments.

## Helm Chart (production Kubernetes)

The Helm chart at `deploy/helm/runkite/` is the production deployment mechanism. It creates:

- A Deployment with configurable `replicaCount` (default: 2)
- A Service exposing both HTTP (2026) and gRPC (50051) ports
- Optional Ingress with annotations for sticky routing on `/mcp`
- ServiceAccount with minimal RBAC
- Pod security context (non-root, read-only root fs, drop all capabilities)
- Configurable resource limits and health probes

**Three values files ship out of the box:**

1. `values.yaml` — Minimal production defaults. You MUST provide `postgresDsn` and `redisUrl` (or an `existingSecret` with those keys). The chart does NOT bundle Postgres or Redis — point it at your managed infrastructure.
2. `values-supported.yaml` — Example overrides for the Supported tier (Postgres + Redis, 3 replicas, stricter resource limits, production-grade settings).
3. `values-tls.yaml` — Adds mTLS configuration: server cert/key, client CA, gRPC TLS port. References Kubernetes Secrets for certificate material.

**What the chart does NOT do:** It does not deploy runners. Runners are your responsibility — they run your agent code (LangGraph, CrewAI, etc.) and are typically separate Deployments with different resource profiles (GPU for LLM inference, or CPU-only for tool-use agents). The chart only deploys the control plane.

## kind (local Kubernetes testing)

For testing the Helm chart locally without a cloud cluster, `deploy/kind/` has a kind (Kubernetes IN Docker) configuration. This spins up a local Kubernetes cluster on your laptop, deploys the Helm chart into it, and lets you validate the full Kubernetes deployment flow (probes, services, ingress) without touching AWS/GCP/Azure.

## Typical production topology

```
Internet
    │
    ▼
[Load Balancer / Ingress Controller]
    │
    ├── /mcp (sticky) ──→ CP Pod (any one, session-pinned)
    │
    ├── /threads, /runs, /agents, /admin-api ──→ CP Pod (round-robin)
    │
    └── gRPC :50051 ──→ CP Pod (round-robin, runners connect here)
    
CP Pods (2-3 replicas) ──→ Postgres (managed RDS/CloudSQL)
                       ──→ Redis (managed ElastiCache/MemoryStore)
                       
Runner Pods (autoscaled) ──→ gRPC to CP Service
                         ──→ Direct Postgres (checkpoints, if direct mode)
```

**Code:** `deploy/helm/runkite/` (chart), `docker-compose.multi.yml` (multi-replica testing), `deploy/kind/` (local K8s)

---



# 87. Deep Dive — Agent Registry (The Marketplace)



## What the registry is

The registry is a searchable catalog of "agent cards" — metadata about agents that other systems (or humans) can discover. Think of it like npm for agent definitions: you publish a package (agent card) with a name, description, version, input schema, and a source reference (where to get the code). Others can search for it, inspect it, and decide to deploy it themselves.

## How it differs from the agents table

These are three SEPARATE concepts (Chapter 57 introduced them briefly, here in full):

1. **Agents table** — What agents CAN BE EXECUTED right now on this control plane. Bootstrapped from `langgraph.json` at startup. createRunCtx checks this table: "does agent X exist?" If not, 404.
2. **Aliases** — Routing indirection for A/B testing. An alias resolves to one or more agent_ids with weights. Not a catalog — just a routing rule.
3. **Registry** — A metadata catalog for discovery. Publishing to the registry does NOT make an agent executable. It just makes it findable. To actually run a registered agent, someone still needs to deploy it (add it to their langgraph.json, configure runners, start the CP).



## Why this separation exists

Security: you do not want "publishing an agent card" to automatically make arbitrary code executable on your infrastructure. Publishing is a safe operation (just metadata). Deploying is a separate, deliberate act with its own trust boundaries (code review, sandboxing, etc.).

## Registry API

- `PUT /registry/entries/{name}` — Publish a new entry or new version. Requires `source_type` and `source_ref`.
- `GET /registry/entries/{name}` — Get an entry by name
- `GET /registry/entries/{name}/versions` — Version history
- `GET /registry/entries` — List/search all entries (with pagination)
- `DELETE /registry/entries/{name}` — Remove an entry



## What a registry entry contains

```json
{
  "name": "customer-support-agent",
  "description": "Handles customer inquiries with Salesforce and Zendesk integration",
  "source_type": "git",
  "source_ref": "https://github.com/acme/support-agent.git",
  "version": 3,
  "metadata": {
    "framework": "langgraph",
    "input_schema": {"type": "object", "properties": {"message": {"type": "string"}}},
    "connectors_needed": ["salesforce", "zendesk"],
    "author": "platform-team"
  }
}
```

`source_type` can be: `git` (a repository URL), `url` (a downloadable archive), or `inline` (a langgraph.json snippet embedded directly). `source_ref` is where a human or automated system goes to actually get the agent code.

## Versioning

Every time you PUT with a different configuration hash, the version increments. If the content is identical to the existing version, no new version is created (idempotent). This matches the same version-bump semantics as the agents table.

## Scoped by what

Registry entries are NOT tenant-scoped in the current implementation. They are control-plane-global. If you need per-tenant agent catalogs, you would run separate Runkite instances or add registry tenancy in the future.

**Code:** `internal/api/registry.go`, `internal/state/` (PublishRegistryEntry, SearchRegistry, GetRegistryEntry), `docs/registry.md`

---



# 88. Deep Dive — How the Admin UI Is Embedded in the Go Binary

## The question

The Admin UI is written in React + TypeScript. The control plane is a Go binary. How does a JavaScript application get served from a compiled Go program without Node.js on the server?

## The mechanism: Go's `//go:embed`

Go has a built-in feature (since Go 1.16) that lets you bake files into the compiled binary. One line of code:

```go
//go:embed all:dist
var distFS embed.FS
```

This tells the Go compiler: "take every file in the `dist/` directory and include them as raw bytes inside the binary." After compilation, the binary IS the frontend — HTML, JS, CSS, images are all embedded inside it.

## The build flow

1. **UI developers** make changes to `admin-ui/` (React source code)
2. They run `npm run build` → Vite compiles TypeScript/React into static files → outputs to `internal/adminui/dist/`
3. The `dist/` folder is committed to the repo
4. When anyone runs `go build`, the `//go:embed` directive bakes those files into the binary
5. The result: ONE Go binary contains the complete frontend

Nobody deploying Runkite needs Node.js. The binary is self-contained.

## How it serves dynamic data

"Static files" does NOT mean "static content." The embedded HTML/JS is the APPLICATION (like installing a mobile app). The DATA is fetched live at runtime:

1. Browser requests `GET /admin/` → Go serves embedded `index.html` (a shell with a `<script>` tag)
2. Browser loads `app.js` → React application starts in the browser
3. React immediately calls `GET /admin-api/runs?status=running` → Go queries the database, returns fresh JSON
4. React renders the data into tables, charts, live-updating views
5. For real-time updates: React opens SSE/WebSocket to receive events as they happen

The embedded JS knows HOW to display data. The data itself comes fresh from the backend on every page load.

## When does the UI need rebuilding?

Only when the UI CODE changes (new page, layout fix, new chart). NOT when data changes (new runs, status updates, admin actions) — those are just API responses.

**Code:** `internal/adminui/adminui.go` (embed directive + HTTP handler), `admin-ui/` (React source), `admin-ui/vite.config.ts` (build output → `internal/adminui/dist/`)

---

# 89. Deep Dive — How Rate Limiting Actually Works (Token Bucket)

## The algorithm

Rate limiting uses a **token bucket**. Imagine a physical bucket:

- The bucket has a maximum capacity (called `burst`) — say 20 tokens
- Every second, `rps` new tokens drip into the bucket (say 10 per second)
- Each request costs 1 token
- If the bucket has tokens: take one, request proceeds
- If the bucket is empty: reject with 429 "Too Many Requests"

This allows short bursts (up to 20 requests instantly) while enforcing a sustained rate (10/second average). After a burst, the bucket refills at 10/second until full again.

## How it's shared across CPs (Redis Lua script)

In multi-replica mode, all CPs must share the SAME bucket. This is implemented as an atomic Lua script that runs INSIDE Redis:

```lua
-- Runs atomically inside Redis (no race conditions)
local tokens = current_tokens + (elapsed_time * rps)  -- refill
tokens = min(tokens, burst)                             -- cap at burst
if tokens >= 1 then
    tokens = tokens - 1                                 -- consume
    return 1  -- ALLOWED
else
    return 0  -- DENIED
end
```

Because this runs as a single atomic operation in Redis, two CPs checking the same bucket simultaneously cannot both see "1 token left" and both allow — Redis serializes the Lua execution.

## The four scoping dimensions

```json
{
  "rate_limit": {
    "backend": "redis",
    "global":     { "rps": 100, "burst": 200 },
    "per_user":   { "rps": 10,  "burst": 20 },
    "per_agent":  { "rps": 50,  "burst": 100 },
    "per_tenant": { "rps": 30,  "burst": 60 }
  }
}
```

Each scope has its own bucket in Redis (keyed like `rk:rl:user:alice`, `rk:rl:agent:research_bot`). A request is checked against ALL configured scopes — if ANY bucket is empty, denied.

## Fail-open on Redis errors

If Redis is unreachable (timeout after 100ms), the rate limiter ALLOWS the request. Reasoning: rate limiting protects availability (prevent overload), not security. Briefly allowing all traffic during a Redis blip is better than blocking everything. This is the OPPOSITE of policy Decide (which fails closed = deny when uncertain).

**Code:** `internal/ratelimit/redis.go` (Lua token bucket), `internal/ratelimit/memory.go` (in-process fallback for dev)

---

# 90. Deep Dive — Why Governance Requires SQL (The FOR UPDATE Mechanism)

## The problem governance must solve

Security-critical operations need guarantees like: "this one-shot approval can be consumed EXACTLY ONCE." If two requests race, only one should succeed. This requires pessimistic locking.

## How SQL solves it (FOR UPDATE)

```sql
BEGIN;
SELECT * FROM capabilities WHERE run_id = 'run_9f' FOR UPDATE;
-- Row is now LOCKED. Other transactions trying to read it with FOR UPDATE are PAUSED.
DELETE FROM capabilities WHERE id = 'cap_xyz';
INSERT INTO audit_events (...) VALUES (...);
COMMIT;
-- Lock released. Waiting transactions can proceed (but the row is gone).
```

`FOR UPDATE` puts an invisible lock on the row. Any other transaction trying to touch that row BLOCKS (waits) until the first transaction commits. This is pessimistic concurrency: "lock first, then operate."

## How the lock works internally

Every row in Postgres has a hidden `xmax` field. When you `FOR UPDATE`, the database writes your transaction ID into that field. Other transactions see this and wait. When you commit, `xmax` clears and waiters proceed. The check is essentially free — it's one integer comparison on data you already loaded.

## Why MongoDB cannot do this

MongoDB has no `FOR UPDATE` equivalent for reads. Two requests can both `findOne()` the same document simultaneously — neither blocks the other. By the time one deletes the document, the other has already read it and may have acted on that stale read.

MongoDB's alternative (optimistic concurrency via transactions) detects conflicts AFTER they happen and retries. For security-critical operations, preventing the conflict is safer than detecting it after the fact.

## What this means concretely

| Operation | SQL approach | MongoDB problem |
|-----------|-------------|-----------------|
| Consume one-shot approval | Lock row → delete → commit (exactly once) | Both read before either deletes (used twice) |
| Admission limit (max 10 runs) | Locked COUNT → INSERT (atomic) | Both count 9, both insert → 11 |
| Approve pending + create capability | Multi-table transaction | No multi-document atomic guarantee |

This is why governance tables (audit, grants, pending, kill, break-glass, mandatory HITL) exist only in Postgres/MySQL/SQLite — not MongoDB.

## Thread claiming DOES work on MongoDB

Thread claiming (`TryClaimThread`) is a SINGLE document atomic update:

```javascript
db.threads.updateOne(
    { thread_id: "thr_42", status: { $ne: "busy" } },  // condition
    { $set: { status: "busy" } }                         // update
)
```

MongoDB guarantees single-document atomicity. If two CPs race, one gets `modifiedCount: 1` (won) and the other gets `modifiedCount: 0` (lost). No lock needed — the conditional update IS atomic at the document level.

The distinction: single-document operations work on MongoDB. Multi-document operations with read-then-write patterns need SQL's pessimistic locks.

## A note on Row-Level Security (RLS) — defense in depth (shipped in v0.3)

Multi-tenancy is still primarily enforced at the application layer: every query includes `WHERE tenant_id = ?` threaded from the request context. This works across ALL backends (Postgres, MySQL, SQLite, MongoDB) and remains the mandatory baseline.

As of v0.3, Postgres deployments can turn on a SECOND enforcement layer: `RUNKITE_POSTGRES_RLS=true`. When set, `Init` creates a `runkite_app` role (`NOINHERIT NOBYPASSRLS`) and stamps `FORCE ROW LEVEL SECURITY` plus a `runkite_tenant_isolation` policy on every tenant-scoped table — 21 tables in total (`internal/state/postgres/rls.go`'s `rlsTables`), from `agents` and `runs` down to `usage_events`/`usage_holds` for the FinOps tables added in the same release. `vector_items` is on the list too even though it's created by the separate pgvector package's own `Init` — it has a real `tenant_id` column and deserves the same coverage, and `ensureRLS` tolerates "does not exist" per-statement so a deployment that never touches pgvector just skips that one table instead of failing.

On every pool checkout, `applyTenantGUC` sets two Postgres session variables (`app.tenant_id`, `app.is_system`) and, for a real tenant request (not a system/admin context), issues `SET ROLE runkite_app`. The RLS policy itself is one condition: `current_setting('app.is_system') = 'true' OR tenant_id = current_setting('app.tenant_id')`. System/admin contexts (migrations, `db upgrade`, cross-tenant Admin UI reads) reset to the login role and see every row; tenant contexts see only their own, even if the application query itself forgot the `WHERE tenant_id = ?` clause. After `ensureRLS` has run once, a `SET ROLE` failure is fail-closed — an error, not a silent bypass — because by that point the role is expected to exist.

Why `SET ROLE` instead of just relying on the RLS policy directly under the DSN's own login role? Because the DSN in CI/dev/many self-hosted setups is a Postgres **superuser**, and superusers bypass RLS entirely regardless of `FORCE ROW LEVEL SECURITY` — that flag only forces RLS onto the table owner, and superuser bypass is a separate, stronger privilege. `runkite_app` is deliberately created `NOBYPASSRLS` so even a superuser DSN gets real enforcement once it `SET ROLE`s into it for a tenant request.

This is still opt-in and off by default: (1) it requires the DSN role to have `CREATEROLE` (or be superuser) so `Init` can bootstrap `runkite_app`, (2) it's Postgres-only — useless on MySQL/SQLite/MongoDB, (3) it adds a `SET ROLE` round-trip per tenant acquire. Turning the flag back off and restarting runs `disableRLS`, which drops the policies and un-forces RLS — otherwise a prior `FORCE ROW LEVEL SECURITY` would linger as a silent deny-all for any role that isn't `runkite_app` or a bypass-capable superuser. Configured via `RUNKITE_POSTGRES_RLS`; see `docs/configuration.md` and `docs/ops-runbook.md`.

---

# 91. Deep Dive — The Two Checkpoint Systems (Often Confused)

## Why people get confused

The word "checkpoint" appears in two completely different contexts in Runkite. They are SEPARATE systems solving different problems.

## System 1: The control plane's state store (`thread_checkpoints` table)

This is part of Runkite's state interface. It stores opaque blobs — the control plane can save/load checkpoints for any framework through its proxy API. It works on ALL backends:

- Postgres ✓
- MySQL ✓
- SQLite ✓
- MongoDB ✓

This system is rarely used directly today. It exists for the proxy-mode checkpoint path and for future universal checkpoint support.

## System 2: LangGraph's own checkpoint tables (runner-side, Postgres only)

This is LangGraph's `AsyncPostgresSaver`. It writes to tables named `checkpoints`, `checkpoint_blobs`, `checkpoint_writes` — LangGraph-specific tables managed by LangGraph's own library. The runner connects directly to Postgres and uses LangGraph's native API.

This is what powers multi-turn conversations for LangGraph agents. It stores the full graph state: all messages, node outputs, channel values, interrupt state. It ONLY works on Postgres because that's what LangGraph's library supports.

## The one-sentence summary

**Everything the control plane does → any DB. LangGraph's conversation memory (direct mode) → Postgres only.**

## What happens on non-Postgres backends for LangGraph specifically

If your control plane uses MySQL or MongoDB:
- Runs, threads, governance, rate limits, store, registry — all work fine
- LangGraph runners fall back to `MemorySaver` (in-memory, lost on restart)
- LangGraph multi-turn conversations are ephemeral (not persisted between runner restarts) — this residual is specific to LangGraph's own direct-Postgres checkpointer, not System 1

## System 3 (shipped in v0.3): universal opaque checkpoints for every non-LangGraph adapter

The gap above — "multi-turn only works if you're LangGraph on Postgres" — is now closed for every OTHER adapter (CrewAI, LlamaIndex, AutoGen, plain LangChain, and the TypeScript/LangGraph.js runner) using System 1's existing opaque proxy path, no new database feature required.

The mechanism (`python/runkite_runner/opaque_checkpoint.py` + `adapter_checkpoint.py`): each of these frameworks doesn't have LangGraph's node/channel graph state to persist — it just needs "what did we say to each other last time." So the adapter GETs and PUTs a single small JSON blob per thread through the same `/internal/checkpoints` proxy endpoint every LangGraph proxy-mode runner already uses, at a **fixed, well-known checkpoint id: `"adapter-state"`** — deliberately not `.../latest` (which is ordered by checkpoint_id DESC and, on a thread that's ever had a LangGraph run on it too, could return a LangGraph UUID-keyed blob instead of this framework's own). The blob itself is opaque to the control plane: `{"v": 1, "messages": [{"role": ..., "content": ...}, ...]}`.

On the next run on that thread, the adapter GETs `adapter-state`, decodes the prior messages, and merges them with whatever the client sent this time. The merge rule matters: if the client already sent a full history at least as long as the checkpoint (a client that keeps its own conversation state and replays it), the client's version wins — no double-prepending. If the client only sent the new turn(s) (the common case — client doesn't want to resend the whole conversation every time), the restored history is prepended in front.

Because this rides System 1 (the opaque `thread_checkpoints`/`opaque_checkpoints` proxy store), it works on **every** state backend — Postgres, MySQL, SQLite, MongoDB — not just Postgres. The write side reuses the same CAS-with-retry pattern as `ProxyCheckpointSaver` (`_CAS_MAX_ATTEMPTS = 8`; see Chapter 33) so two concurrent runs on the same thread don't clobber each other's history.

**Code:** `python/runkite_runner/opaque_checkpoint.py` (the HTTP client), `python/runkite_runner/adapter_checkpoint.py` (the blob encode/decode/merge helpers), wired into each adapter's `adapter.py` in `python/adapters/*/`. TypeScript equivalent: `typescript/runkite-runner/src/proxyCheckpoint.ts`.

---

# 92. Deep Dive — The LLM Cache Mechanism (Hash-Based, Not Field Comparison)

## How the lookup works without comparing every field

The cache does NOT compare input, agent, and config field by field. Instead:

1. Compute a SHA-256 hash of `tenant_id + agent_id + input_bytes + config_bytes`
2. This produces a single 64-character string (e.g., `"a3f8b2c1d9e7..."`)
3. Look up that string as a primary key in the `cached_run_results` table:
   ```sql
   SELECT * FROM cached_run_results WHERE cache_key = 'a3f8...' AND expires_at > NOW()
   ```
4. One indexed primary key lookup. Sub-millisecond. Done.

## How B-tree indexes work with random-looking hashes

A common misconception: "hash keys look random, so they can't be indexed efficiently." This is wrong. B-tree indexes require SORTABILITY, not similarity. Strings (even random ones) sort lexicographically: `a3f8 < b1c4 < e7a2 < f9d3`. The B-tree organizes them into a balanced tree, and any lookup is O(log n) — about 4 hops for 10 million rows.

The B-tree doesn't care WHAT the strings contain. It just sorts them and builds a tree. Whether keys are `alice, bob, charlie` or `a3f8, e7a2, f9d3` — the structure works identically.

## Expiry and cleanup

- **Reads:** The `AND expires_at > NOW()` filter ensures expired entries are never returned (immediate cache miss)
- **Cleanup:** The retention background loop periodically runs `DELETE FROM cached_run_results WHERE expires_at < NOW()`, sweeping expired rows in bulk. The table stays bounded.

## When caching is checked

Only if the agent has `llm_cache` configured with a TTL > 0 in `langgraph.json`. Agents without caching configured skip the check entirely (zero overhead). The SHA-256 computation + one DB lookup adds ~0.1ms — negligible compared to the LLM call it might save (2-10 seconds, $0.01-0.10).

---

# 93. Practice Questions — Test Your Understanding

Answer these without looking at previous chapters. If you cannot answer confidently, go back and re-read.

1. What are the three types of processes in a Runkite deployment?
2. Name the five gRPC RPCs and what each one does in one sentence.
3. What is the difference between Postgres and Redis in this system? What does each store?
4. What happens if a runner crashes mid-execution? Walk through the timeline with specific numbers.
5. How does cancel reach a runner that is connected to a different CP than the one receiving the cancel request?
6. What are the two kinds of HITL? Who approves each one? Which protocol does each use?
7. What is run-binding and why does it exist?
8. What is generation fencing and why does it matter?
9. Name three things that need sticky routing in multi-replica.
10. What does "fail-closed" mean for serve admission vs policy Decide vs break-glass audit?
11. Why cannot MongoDB provide full governance? What specific features are missing?
12. What is the reclaim leader lock and why is it needed?
13. How does the policy engine know which tool an agent is calling?
14. What is the difference between rate limits and admission limits?
15. What happens if you run `runkite serve` without configuring auth?
16. How do agents communicate with each other? Do they open sockets to each other?
17. What is the product claim boundary sentence and what does each part mean?
18. Name two security findings that were fixed and explain what was wrong.
19. What is a connector session token? Why is it minted at use time, not at dispatch?
20. How long after a runner crashes will reclaim fire? Show the math.
21. What is a factory graph and when would you use one instead of a compiled graph?
22. What does the LLM response cache do, and why is it OFF by default?
23. What is the difference between the registry and the agents table?
24. How does mTLS differ from just TLS? What extra security does it provide for runners?
25. Name the three types of hooks and explain which ones block the run and which do not.
26. What is MCP Server mode and how does it differ from the Connectors feature?
27. Why does the Helm chart NOT deploy runners?
28. How is the Admin UI embedded in the Go binary? Does it need Node.js to run?
29. Explain how the token bucket rate limiter works across multiple CP replicas.
30. Why does governance require SQL but thread claiming works on MongoDB?
31. What are the two completely separate checkpoint systems and which databases does each support?
32. How does the LLM cache avoid comparing every field — what mechanism makes lookup instant?

If you can answer all 32 confidently in your own words without copying text, you deeply understand Runkite.

---

# Part Two: What v0.3 Added

Everything above was written against the v0.2 cut. Between v0.2 and v0.3, four genuinely new subsystems shipped (FinOps, the run manifest, universal opaque checkpoints, and Postgres RLS — the last two you already read about in Chapters 90-91 above, corrected in place), the Admin UI grew a live playground and a catalog editor, and a pre-release review pass found and fixed several real bugs, including one where the FIX itself introduced a new bug (Chapter 21, above). This part covers the two big pieces that didn't fit as edits to existing chapters: FinOps, and the run manifest.

---

# 94. Deep Dive — FinOps: Cost Governance, Not a Billing Platform

## What problem this solves

Before v0.3, Runkite could tell you an agent ran and roughly how many tokens it used (via the OTel/usage plumbing in Chapter 19), but there was no way to say "stop this tenant if they spend more than $50/day" or "warn me before anyone hits their cap" or "show me a dashboard of who's spending what." FinOps is that layer: pricing, budgets, alerts, and — the part that makes it more than a dashboard — the ability to actually refuse or cancel work when a cap is hit.

The name is deliberately narrow. This is **cost governance for a self-hosted control plane you already run**, not a multi-tenant SaaS billing platform. It doesn't generate invoices, doesn't handle payment methods, and doesn't claim to be "100% accurate" down to the last input token some provider silently retried internally. It answers a narrower, achievable question: "is spend inside the bounds we configured, and can we act on it in real time?"

## The four moving pieces

**1. The pricebook** (`internal/finops/pricebook.go`). A map from model id to USD-per-1000-tokens (input and output priced separately, since output tokens are typically 3-5x more expensive across providers). `EstimateUSD(model, tokensIn, tokensOut, costUSD)` has one deliberate rule that resolves the "how accurate can this actually be" question: **if the runner or gateway reported a real `cost_usd` (LiteLLM, Portkey, and similar gateways return this on the response), that number wins outright — the pricebook is only a fallback for when nobody reported a real cost.** This matters because a static per-1k-token pricebook cannot know about provider-side prompt caching discounts, batch-API pricing, or a model's actual list price changing after your pricebook was written. Where a real reported cost exists, use it; where it doesn't, estimate from tokens and be honest that it's an estimate.

`HasModel()` exists for a specific UX reason (see the Spend page below): a model with a $0/$0 row in the pricebook (intentionally free-tier) needs to be distinguishable from a model that's simply MISSING from the pricebook (an admin forgot to price it). Same numeric result — $0 estimated cost — completely different meaning.

**2. Budgets** (`internal/finops/budget.go`). Daily caps, scoped to a tenant or to a specific `tenant_id/agent_id` pair, on three independent dimensions: `max_usd_per_day`, `max_tokens_per_day`, `max_runs_per_day`. Each cap is either **hard** (default — new run creation is refused once breached) or **soft** (`soft: true` — creation is still allowed, but a `budget_soft` audit event fires so you can see it happening without it blocking anyone). `UTCDayWindow()` computes the day boundary in UTC consistently, so "daily" means the same thing regardless of which replica or which admin's timezone is asking.

**3. Reservation holds** (optimistic admission control). Before v0.3's FinOps work, admission was purely reactive — you'd only find out a tenant blew through their daily cap AFTER their usage was ingested from a completed run, by which point they may have already started five more runs. Reservation holds fix the ordering: at run CREATE time, before the agent has done any work at all, the system provisionally reserves an estimated USD and/or token amount against that tenant/agent's day cap (`ReservationFor()` — global amounts, with optional per-agent overrides that fully replace the global for that agent). If the reservation would push the tenant over a hard cap, the run is refused before a single token is spent. When the run finishes and its REAL usage is ingested, the hold is released and replaced by the actual number. A `HoldTTL` exists specifically for the case where a runner crashes and never reports completion — an un-released hold that lived forever would eventually pin a tenant's entire daily budget on phantom in-flight runs that no longer exist; the TTL expires it automatically.

**4. Routing and cancel-on-hard-breach.** Two optional behaviors layered on top of the caps: **routing** can rewrite an agent alias to a cheaper fallback agent once spend crosses a soft threshold near the cap (`RoutingConfig.Aliases`, threshold defaults to the same `soft_pct` as alerts, 80% by default) — the idea being "keep serving requests, just route them somewhere cheaper as you approach the ceiling," rather than an all-or-nothing cutoff. **`on_hard_breach: cancel_inflight`** is the more aggressive option: rather than just refusing NEW runs once a hard cap is breached, it also actively cancels runs that are already pending or running in that scope. This reuses the exact same drain mechanism as a kill switch (`drainKillScope`, Chapter 11) — a hard budget breach is treated as equivalent in urgency to an operator hitting the kill switch, with its own `ReasonBudgetKill` audit reason code so you can tell the two apart in the audit trail afterward.

## Why this lives in SQL, and why every FinOps table is on the RLS list

Budget evaluation needs the same atomicity guarantee as governance capabilities (Chapter 90's `FOR UPDATE` explanation): checking "is today's spend already at the cap" and then admitting or refusing a new reservation has to be one atomic operation, or two concurrent run-creates can both see "just under the cap" and both get admitted, blowing through it together. `usage_events` (the ledger of what actually happened) and `usage_holds` (the provisional reservations) are both Postgres/MySQL/SQLite tables for this reason, and both are on the RLS-covered table list added in the same release (Chapter 90) — FinOps data is exactly the kind of thing a tenant should never be able to see across tenant boundaries even if an application-layer query forgot its `WHERE tenant_id` clause.

## Configuration layering — same three-layer model as Chapter 12's governance

FinOps config follows the identical pattern already established for policy grants: a deploy-time baseline in `langgraph.json`'s `finops` section, and a runtime Postgres overlay editable live from the Admin UI without a restart. `internal/finops/merge.go`'s `Merge(base, overlay)` combines them the same way grants merge — the overlay wins per-key. `DecodeOverlayPayload`/`EncodeOverlayPayload` handle the JSON round-trip for the Admin UI's live editor (Chapter 95).

**Code:** `internal/finops/` (pricebook, budget, merge, validate — all backend-agnostic pure logic), `internal/api/admin_finops.go` (the three Admin API routes), `internal/api/admin_usage.go` and `internal/api/usage.go` (ingesting real usage from completed runs), `internal/api/budget_kill.go` (the cancel-on-hard-breach drain).

---

# 95. Deep Dive — The FinOps Admin UX, and Why "100% Accurate Cost" Isn't a Real Target

## The Spend page

The Admin UI's Spend page (`admin-ui/src/pages/Spend.tsx`) shows per-tenant and per-agent spend against configured caps, plus a specific warning state: **"Est. USD is $0 because pricing did not match."** This fires when tokens were genuinely recorded but the estimated USD came out to zero — which (per the pricebook's `HasModel()` distinction above) means either the model is intentionally free-tier (a $0/$0 pricebook row exists) or the model is simply missing from the pricebook (admin oversight) and the page cannot tell you which just from the number, so it flags both cases for a human to check.

## The live overlay editor (`FinOpsConfigPanel.tsx`)

Three admin operations, each behind a proper confirmation `Dialog` rather than a browser `window.confirm()` (a v0.3 fix — the earlier version used `window.confirm`, which breaks the UX pattern every other destructive action in the Admin UI follows and doesn't respect the app's own theming/accessibility):

- **Save pricebook + budgets.** `PUT /admin-api/finops` replaces the live overlay with a validated document. If the admin is about to save a model at literal $0/$0 rates, a confirm dialog appears first ("Save as free tier?") — this is the UI surfacing exactly the ambiguity described above, at the moment it matters, instead of silently accepting what might be a typo.
- **Clear overlay.** `DELETE /admin-api/finops` drops the live overlay entirely, falling back to whatever `langgraph.json` says. Also gated behind a confirm dialog, since this is a real "undo my last N live edits" action.
- **View current effective config.** `GET /admin-api/finops` returns the merged (baseline + overlay) view — what's actually in effect right now, not just what's in one layer or the other.

## The accuracy question, answered honestly

A natural question when reviewing this system: can FinOps report "100% accurate" cost? No, and understanding exactly why is more useful than pretending otherwise:

- **When a gateway reports real cost** (LiteLLM, Portkey, similar), that number is used directly — this IS accurate, to the precision the gateway itself reports.
- **When nothing reports a real cost**, the pricebook estimates from token counts using a static per-1k rate. This is necessarily an approximation: it can't reflect prompt-caching discounts, batch-pricing tiers, or a provider changing list prices after the pricebook was last updated. The system is honest about this being an estimate — the field is called `usd_estimate`, not `usd_actual`, in the underlying data.
- **Recursive/nested calls (tool calls, A2A sub-runs, MCP round-trips) are attributed at the level where usage was actually reported** — each run's own usage event captures its own tokens; a parent run's cost figure is its own LLM calls, not an automatic roll-up of every child run's cost. If you want total cost for a multi-agent tree, you sum the tree yourself from the audit/usage trail rather than the platform silently rolling it up (rolling up silently would hide exactly which layer of a deep call tree drove the spend, which defeats the point of a governance tool).
- **Pricebook staleness is a real, acknowledged limitation**, not a bug: providers change prices; a self-hosted pricebook you configure once doesn't auto-update. This is why the Spend page's "$0 because pricing did not match" warning exists — it's the system's way of surfacing exactly this staleness the moment it would otherwise produce a silently wrong number, rather than hiding it behind a confident-looking $0.00.

The honest framing: FinOps gives you real-time admission control and a governance-grade audit trail of what was allowed and why, using the best cost signal available at the moment (reported cost when present, estimate when not). It is not, and does not claim to be, a substitute for your cloud provider's actual billing statement at the end of the month.

---

# 96. Deep Dive — The Run Manifest ("Blueprint-lite"): Freezing Intent at Dispatch Time

## The problem this solves

By the time a run actually executes, several decisions have already been made: which agent version was resolved, what alias was requested, which connectors the agent might need, whether an `allowed_tools` filter applies, who (if anyone) authenticated the request, and — for an Agent-to-Agent child run — how deep in the delegation chain this run sits. None of that was previously captured anywhere durable. If someone asks "under what authority did THIS specific run execute, exactly as it was at the moment it was dispatched" six months later, the honest answer was "we'd have to reconstruct it from logs and hope nothing changed in between."

The run manifest is a frozen, point-in-time snapshot of exactly that authorized intent, captured at the moment a run is actually dispatched (or served from cache — more on that below) and stored permanently in two places: the run's own metadata (queryable later, shown in the Admin UI's Run Detail page) and the `RunAssignment` sent to the runner (so the runner itself can see, if it wants to, exactly what it was authorized to do).

## What's in it

```go
type RunManifest struct {
    SchemaVersion    int
    CapturedAt       time.Time
    TenantID         string
    AgentID          string
    RequestedAlias   string                 // what the client asked for, before alias resolution
    AgentVersion     int                     // which version actually got resolved
    RunnerKind       string                  // e.g. "python-langgraph"
    ConnectorNeeds   []string                // pre-warm hints from agent metadata
    AllowedTools     *[]string               // nil = no filter; non-nil (incl. empty) = enforced allowlist
    PolicyFailClosed bool
    Principal        *RunManifestPrincipal   // who authenticated this, if auth is configured
    ParentRunID      *string                 // set for an A2A child run
    Depth            int                     // A2A delegation depth at dispatch time
}
```

`SchemaVersion` exists from day one specifically so a future field addition doesn't silently break anyone parsing old manifests — readers can branch on version rather than assume every manifest ever written has every field.

## Where it gets built, and the two dispatch paths that both need it

`buildRunManifest()` (`internal/api/run_manifest.go`) is called from `createRunCtx`'s normal dispatch path — the run goes through the full pipeline (alias resolution, policy checks, enqueue), and the manifest is stamped with the REAL resolved values (actual agent version, actual runner kind from agent metadata, actual authenticated principal).

The subtler part: a **cache hit** (Chapter 82's LLM response cache — an identical `(tenant, agent, input, config)` tuple that already ran) short-circuits most of that pipeline and returns the cached result immediately, without going through the normal dispatch path at all. Before this was fixed, a cache-hit run got NO manifest — an inconsistency where identical inputs sometimes had an audit trail and sometimes didn't, depending on whether they happened to be a cache hit. The fix threads the same manifest-building call into `tryServeCachedRun` too, so every run gets a manifest regardless of which path served it — audit completeness shouldn't depend on a performance optimization.

## The A2A guard this surfaced

While wiring the cache-hit manifest fix, a second, more serious latent issue surfaced: an Agent-to-Agent CHILD run (one with `ParentRunID` set) could ALSO be served from cache — and the cache-hit path never enforced the `max_depth`/`max_breadth` checks that the normal dispatch path enforces (Chapter 9). In principle, a cached child run could bypass A2A recursion limits entirely, since it never touched the code that checks them. The fix: `tryServeCachedRun` is now only even attempted when `req.ParentRunID == nil` — an A2A child run is unconditionally forced through the full dispatch path, where depth/breadth limits are enforced, every time, with no cache shortcut available to skip that gate. This turned "A2A children hitting the cache-hit path are rare in practice" into "structurally impossible" — a stronger guarantee earned by removing the shortcut rather than trying to duplicate the depth/breadth check inside the cache-hit path too.

## How it reaches the runner

The manifest is serialized and copied into `RunAssignment.RunManifest` (`json.RawMessage` — the assignment struct doesn't need to understand the manifest's shape, it just carries the bytes), and the `run_assignment.json` schema documents the `run_manifest` field with `additionalProperties: true` so schema validation doesn't need updating every time a manifest field is added. A runner that wants to introspect its own authorization (for logging, for a custom policy double-check, for anything) can read this straight off its assignment instead of re-deriving it.

## Where you can see it

The Admin UI's Run Detail page renders a Run Manifest card — a formatted summary of the frozen intent, plus a cache-hit badge when the run was actually served from cache rather than freshly dispatched, so an operator looking at a run's detail page can immediately tell which path served it and exactly what was authorized.

**Code:** `internal/models/models.go` (`RunManifest`/`RunManifestPrincipal` structs), `internal/api/run_manifest.go` (`buildRunManifest`/`runManifestToMetadata`/`runManifestToRaw`), `internal/api/runs.go` (wiring into both `createRunCtx` and `tryServeCachedRun`), `internal/transport/types.go` (`RunAssignment.RunManifest`), `runner-protocol/schemas/run_assignment.json`, `admin-ui/src/pages/RunDetail.tsx` (the card + cache-hit badge).

---

# 97. Deep Dive — Admin UI Additions Since v0.2: Try-Agent and the Catalog Editor

## Try-Agent: a live playground inside the Admin UI

Before v0.3, exercising a registered agent meant using `curl` or a script against the Agent Protocol API directly — functionally complete, but nobody was going to demo the product that way. The Try-Agent page (`admin-ui/src/pages/TryAgent.tsx`) is a real chat-style playground embedded in the Admin UI: pick a registered agent, send it a message, and watch the actual SSE protocol stream render live — assistant text extracted and displayed as a normal chat bubble, but with a raw event log alongside it (`RawLine` entries: timestamp, event type, a preview of the payload) so you can see the actual wire-level Agent Protocol events, not just the pretty version. Token usage, when reported, is extracted and shown per turn (`extractUsage`).

This matters for two audiences at once: it's a genuinely useful debugging tool (see exactly what your agent streamed, without instrumenting your own client), and it's the fastest way to demo "this is a real protocol server, not a mockup" to someone evaluating the product — five minutes from opening the Admin UI to watching a real multi-framework agent stream tokens.

## The catalog editor: agent detail and registry editing from the Admin UI

Two related but distinct additions:

- **Agent detail editing** (`admin-ui/src/pages/AgentDetail.tsx`) — viewing and, where applicable, editing an agent's configuration and schema straight from the Admin UI rather than only through `langgraph.json` and a redeploy.
- **Registry entry editing** (`admin-ui/src/pages/RegistryEntry.tsx`) — the Agent Registry (Chapter 87) gained a real editor, not just a read-only catalog view. `handleSave()` lets an admin publish or update a registry entry's metadata directly.

Both pages are explicit about what they DON'T do: editing an agent's underlying *graph* (the actual LangGraph/CrewAI/etc. code) from the Admin UI is not supported — these pages edit metadata, schemas, and registry entries, which live in the control plane's own database, not the compiled graph a runner loads from its own source. Wiring a registry entry's `source_ref` into an actual running runner still requires a deploy or restart; the registry entry itself is discoverable metadata, not a live code-push mechanism. (An earlier draft of this UI copy called this boundary "Phase C.2" — an internal planning label with no meaning outside this project's own tracking. It's been rewritten to just state the boundary plainly, since anyone reading it from GitHub has no context for what "Phase C.2" ever meant.)

**Code:** `admin-ui/src/pages/TryAgent.tsx`, `admin-ui/src/lib/agentMessages.ts` (the SSE-shape-normalizing helpers — `extractAssistantText`, `messageRole`, `messageContent`, `extractUsage`, all written to handle the different event shapes each framework's runner actually emits, since LangGraph.js, Python LangGraph, and Gemini's adapter all shape their SSE payloads slightly differently), `admin-ui/src/pages/AgentDetail.tsx`, `admin-ui/src/pages/RegistryEntry.tsx`.

---

# 98. Deep Dive — Cutting a Release: What Actually Happens for a Version Bump

This is worth documenting precisely because it's exactly the kind of thing that's easy to do inconsistently across releases without noticing — and inconsistency here is how you end up with the wrong version string burned into a shipped Docker image.

## The four places a version number lives, independently

There is no single "the version" — there are four files, each read by a different toolchain, and a release needs all four bumped together:

1. **`VERSION`** — the root-level file the Go binary itself reads to embed its own version string.
2. **`deploy/helm/runkite/Chart.yaml`** — `version` and `appVersion`, read by Helm when someone installs the chart.
3. **`python/pyproject.toml`**'s `[project].version`, plus `python/runkite_runner/__init__.py`'s `__version__` — both need bumping because `pip install runkite-runner==X` and `import runkite_runner; runkite_runner.__version__` are two different things a user might check, and they'd better agree.
4. **`typescript/runkite-runner/package.json`**'s `version` — read by `npm install runkite-runner@X` and `npm view`.

A release commit's whole job, version-wise, is bumping all four in one atomic commit so there's never a moment where, say, the Helm chart says 0.2.0 while PyPI already has 0.3.0.

## The lockfile trap

`package-lock.json` is a FIFTH place a version string technically lives, and it's the one that's easy to forget: `npm install` regenerates it from `package.json`, but if you only ever hand-edit `package.json`'s version field (never re-running `npm install` locally), the lockfile silently keeps whatever version it had from whenever it was first generated — potentially the `npm init` default of `1.0.0`, forever, through every subsequent release. This doesn't break anything functionally (`npm publish` reads `package.json`, not the lockfile, for package metadata; `npm ci` was already resolving the correct dependencies) — but it's a real, if harmless, drift that accumulates silently for exactly the reason nobody notices it: nothing fails when it happens.

## What the release workflow actually does (once the tag is pushed)

Four independent CI jobs, all triggered by the same tag push:

- **`goreleaser`** builds the Go binary for four platform/arch combinations (Darwin/Linux × amd64/arm64), embedding `main.Version`/`main.GitCommit`/`main.BuildTime` via `-ldflags`, and creates the GitHub Release with those four tarballs plus a `checksums.txt` and the OpenAPI spec files as attached assets. `GitCommit` here comes from GoReleaser's own `{{.ShortCommit}}` — a 7-character short SHA.
- **`docker`** builds and pushes the control-plane and runner images to `ghcr.io`, multi-arch (amd64+arm64). The control-plane image's `GIT_COMMIT` build-arg is fed from a workflow step that computes `${GITHUB_SHA:0:7}` — deliberately truncated to match GoReleaser's convention, so the Docker image's version banner and the binary release's version banner report the identical short SHA rather than one being a 7-char short form and the other the full 40-char SHA (an inconsistency that existed briefly during the v0.3 hardening pass and was caught and fixed before the tag).
- **`pypi`** builds and publishes the Python package.
- **`npm`** publishes the TypeScript package.

All four ran and succeeded independently for the actual v0.3.0 tag; verified after the fact by pulling both Docker images, downloading a release binary, and checking PyPI/npm directly — not just trusting that the CI run showed green, since "the workflow succeeded" and "the artifact is actually correct" are two different claims worth checking separately when it's cheap to do so.

## The `reviews/` and `explain_in_details/` disclosure-timing pattern

One more thing worth internalizing about how this project handles pre-release material: external review write-ups (`reviews/`, `CODE_REVIEW_ANALYSIS*.md`) are gitignored specifically because an itemized list of "here's exactly what's broken/insecure right now" is a real disclosure-timing risk if it's public before the findings are actually fixed — the fix and the disclosure of the problem should land together, not the problem first and the fix later. The same logic applies to unpublished announcement copy (`docs/announce-v0.3-draft.md`) for a different reason (it's just not-yet-used marketing material, not a security document) — but the mechanism is identical: `git rm --cached` plus a `.gitignore` entry, keeping the file on disk for whoever's writing it while keeping it out of the tracked tree until it's actually meant to be there. Removing a file from tracking this way does NOT remove it from git history if it was ever previously committed and pushed — that requires an actual history rewrite (`git filter-repo` + force-push), a much bigger and riskier operation reserved for when the content is actually sensitive (a leaked credential), not just premature.

---

# 99. Practice Questions — Part Two (v0.3 Additions)

1. In the pricebook's `EstimateUSD`, what wins when both a real reported `cost_usd` AND a pricebook entry for that model exist? Why?
2. What's the difference between a model missing from the pricebook and a model with an explicit $0/$0 pricebook row — and why does the Spend page need to tell them apart?
3. What is a reservation hold, and what specific ordering problem does it fix that pure post-hoc usage ingestion couldn't?
4. Why does an un-released reservation hold need a TTL? What real-world failure does that TTL protect against?
5. What does `on_hard_breach: cancel_inflight` reuse from an entirely different feature, and why is that reuse appropriate rather than a coincidence?
6. Why are `usage_events` and `usage_holds` on the RLS-covered table list?
7. What is a run manifest, and what specific question does it answer that logs alone couldn't reliably answer six months later?
8. Why did a cache-hit run originally have no run manifest, and why did that matter?
9. What A2A safety issue got fixed as a side effect of the cache-hit manifest fix, and what's the exact guard that closes it?
10. What are the four independent files that all encode a project version number, and what's the fifth one that's easy to forget?
11. Why does the Docker image's `GIT_COMMIT` deliberately get truncated to 7 characters instead of using the full SHA?
12. The admin login rate limiter's first version trusted `X-Forwarded-For`. What made that trust misplaced for THIS project specifically, and what's the one-line fix?
13. A shared conformance test caught a partial fix that skipped one backend on purpose. Why did the test not "know" to skip that backend too, and was that the test's fault?
14. What's the actual mechanical difference between "removed from git tracking" and "removed from git history" — and which one did untracking the announce draft actually do?

If you can answer all 14 of these plus the original 32 from Chapter 93, you understand Runkite as of v0.3.0, not just v0.2.

---

End of document.