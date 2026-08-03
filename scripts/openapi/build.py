#!/usr/bin/env python3
"""Build three OpenAPI 3.1.0 JSON spec files from the existing base spec
plus programmatic additions for every registered route.

Outputs:
  spec/openapi.json          — PUBLIC client API (Agent Protocol + Runkite extensions)
  spec/openapi-admin.json    — /admin-api/* Admin API
  spec/openapi-internal.json — /internal/* Runner/A2A surface
"""

import json
import copy
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SPEC_DIR = REPO_ROOT / "spec"

# ---------------------------------------------------------------------------
# Reusable fragments
# ---------------------------------------------------------------------------

_ERR_404 = {
    "description": "Not Found",
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}},
}
_ERR_409 = {
    "description": "Conflict",
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}},
}
_ERR_422 = {
    "description": "Validation Error",
    "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ErrorResponse"}}},
}


def _path_param(name: str, desc: str, fmt: str = "uuid") -> dict:
    schema: dict = {"type": "string", "title": name.replace("_", " ").title()}
    if fmt:
        schema["format"] = fmt
    return {
        "description": desc,
        "required": True,
        "schema": schema,
        "name": name,
        "in": "path",
    }


def _json_response(status: str, desc: str, schema: dict) -> dict:
    return {status: {"description": desc, "content": {"application/json": {"schema": schema}}}}


def _ref(name: str) -> dict:
    return {"$ref": f"#/components/schemas/{name}"}


def _array_of(ref: dict) -> dict:
    return {"type": "array", "items": ref}


_THREAD_ID_PARAM = _path_param("thread_id", "The ID of the thread.")
_RUN_ID_PARAM = _path_param("run_id", "The ID of the run.")
_AGENT_ID_PARAM = _path_param("agent_id", "The ID of the agent.", fmt="")
_NAME_PARAM = _path_param("name", "The registry entry name.", fmt="")
_VERSION_PARAM = _path_param("version", "The version number.", fmt="")

# ---------------------------------------------------------------------------
# New component schemas to add
# ---------------------------------------------------------------------------


def _new_schemas() -> dict:
    return {
        "AgentVersion": {
            "type": "object",
            "required": ["agent_id", "version", "name", "capabilities", "created_at"],
            "properties": {
                "agent_id": {"type": "string"},
                "version": {"type": "integer"},
                "name": {"type": "string"},
                "description": {"type": "string"},
                "metadata": {"type": "object"},
                "capabilities": {"type": "object"},
                "created_at": {"type": "string", "format": "date-time"},
            },
            "title": "AgentVersion",
            "description": "One immutable snapshot of an agent definition at a specific version.",
        },
        "Assistant": {
            "type": "object",
            "required": ["assistant_id", "graph_id", "config", "metadata", "name", "version"],
            "properties": {
                "assistant_id": {"type": "string", "description": "Same as agent_id."},
                "graph_id": {"type": "string"},
                "config": {"type": "object"},
                "metadata": {"type": "object"},
                "name": {"type": "string"},
                "description": {"type": "string"},
                "version": {"type": "integer"},
                "created_at": {"type": "string", "format": "date-time"},
                "updated_at": {"type": "string", "format": "date-time"},
            },
            "title": "Assistant",
            "description": "LangGraph SDK compatibility view of an agent.",
        },
        "GraphSchema": {
            "type": "object",
            "required": ["graph_id", "input_schema", "output_schema"],
            "properties": {
                "graph_id": {"type": "string"},
                "input_schema": {"type": "object"},
                "output_schema": {"type": "object"},
                "state_schema": {"type": "object"},
                "config_schema": {"type": "object"},
            },
            "title": "GraphSchema",
            "description": "LangGraph SDK graph schema view.",
        },
        "RegistryEntry": {
            "type": "object",
            "required": ["name", "source_type", "source_ref", "version", "created_at", "updated_at"],
            "properties": {
                "name": {"type": "string"},
                "display_name": {"type": "string"},
                "description": {"type": "string"},
                "author": {"type": "string"},
                "tags": {"type": "array", "items": {"type": "string"}},
                "source_type": {"type": "string", "enum": ["git", "url", "inline"]},
                "source_ref": {"type": "string"},
                "metadata": {"type": "object"},
                "version": {"type": "integer"},
                "created_at": {"type": "string", "format": "date-time"},
                "updated_at": {"type": "string", "format": "date-time"},
            },
            "title": "RegistryEntry",
        },
        "RegistryEntryVersion": {
            "type": "object",
            "required": ["name", "version", "source_type", "source_ref", "created_at"],
            "properties": {
                "name": {"type": "string"},
                "version": {"type": "integer"},
                "display_name": {"type": "string"},
                "description": {"type": "string"},
                "author": {"type": "string"},
                "tags": {"type": "array", "items": {"type": "string"}},
                "source_type": {"type": "string"},
                "source_ref": {"type": "string"},
                "metadata": {"type": "object"},
                "created_at": {"type": "string", "format": "date-time"},
            },
            "title": "RegistryEntryVersion",
        },
        "RegistrySearchRequest": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Substring match, case-insensitive."},
                "tags": {"type": "array", "items": {"type": "string"}, "description": "Entry must have ALL listed tags."},
                "author": {"type": "string", "description": "Exact match."},
                "limit": {"type": "integer", "default": 10, "minimum": 1},
                "offset": {"type": "integer", "default": 0, "minimum": 0},
            },
            "title": "RegistrySearchRequest",
        },
        "ThreadUpdateStateResponse": {
            "type": "object",
            "required": ["checkpoint"],
            "properties": {
                "checkpoint": {"$ref": "#/components/schemas/ThreadCheckpoint"},
            },
            "title": "ThreadUpdateStateResponse",
        },
        "ThreadHistoryRequest": {
            "type": "object",
            "properties": {
                "limit": {"type": "integer", "default": 10},
                "before": {"type": "object", "description": "Checkpoint to paginate before."},
            },
            "title": "ThreadHistoryRequest",
        },
        "VectorUpsertRequest": {
            "type": "object",
            "required": ["namespace", "id", "embedding"],
            "properties": {
                "namespace": {"type": "string"},
                "id": {"type": "string"},
                "content": {"type": "string"},
                "embedding": {"type": "array", "items": {"type": "number"}},
                "metadata": {"type": "object"},
            },
            "title": "VectorUpsertRequest",
        },
        "VectorDeleteRequest": {
            "type": "object",
            "required": ["namespace", "id"],
            "properties": {
                "namespace": {"type": "string"},
                "id": {"type": "string"},
            },
            "title": "VectorDeleteRequest",
        },
        "VectorSearchRequest": {
            "type": "object",
            "required": ["namespace", "embedding"],
            "properties": {
                "namespace": {"type": "string"},
                "embedding": {"type": "array", "items": {"type": "number"}},
                "top_k": {"type": "integer", "default": 10},
                "filter": {"type": "object"},
            },
            "title": "VectorSearchRequest",
        },
        "VectorSearchResponse": {
            "type": "object",
            "required": ["results"],
            "properties": {
                "results": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "item": {
                                "type": "object",
                                "properties": {
                                    "namespace": {"type": "string"},
                                    "id": {"type": "string"},
                                    "content": {"type": "string"},
                                    "metadata": {"type": "object"},
                                    "created_at": {"type": "string", "format": "date-time"},
                                    "updated_at": {"type": "string", "format": "date-time"},
                                },
                            },
                            "score": {"type": "number"},
                        },
                    },
                },
            },
            "title": "VectorSearchResponse",
        },
        "RunCostSummary": {
            "type": "object",
            "required": ["root_run_id", "run_count", "usage", "runs"],
            "properties": {
                "root_run_id": {"type": "string"},
                "run_count": {"type": "integer"},
                "usage": {"$ref": "#/components/schemas/RunUsage"},
                "runs": {"type": "array", "items": {"$ref": "#/components/schemas/RunCostDetail"}},
            },
            "title": "RunCostSummary",
        },
        "RunUsage": {
            "type": "object",
            "properties": {
                "prompt_tokens": {"type": "integer"},
                "completion_tokens": {"type": "integer"},
                "total_tokens": {"type": "integer"},
                "cost_usd": {"type": "number"},
            },
            "title": "RunUsage",
        },
        "RunCostDetail": {
            "type": "object",
            "required": ["run_id", "agent_id", "depth", "usage"],
            "properties": {
                "run_id": {"type": "string"},
                "agent_id": {"type": "string"},
                "depth": {"type": "integer"},
                "usage": {"$ref": "#/components/schemas/RunUsage"},
            },
            "title": "RunCostDetail",
        },
    }


# ---------------------------------------------------------------------------
# PUBLIC spec builder
# ---------------------------------------------------------------------------


def _build_public_spec() -> dict:
    base_path = SPEC_DIR / "openapi.json"
    spec = json.loads(base_path.read_text())

    # -- info --
    spec["info"]["description"] = (
        "Agent Protocol v0.1.6 plus Runkite platform extensions. "
        "Path parameter names in this document use snake_case (e.g. {agent_id}); "
        "Go net/http mux uses camelCase (e.g. {agentID}). Wire paths are identical."
    )

    # -- tags --
    existing_tag_names = {t["name"] for t in spec.get("tags", [])}
    for tag in [
        {"name": "Assistants", "description": "LangGraph SDK compatibility endpoints (/assistants/*)."},
        {"name": "Registry", "description": "Agent marketplace / registry: publish, discover, version agent definitions."},
        {"name": "Vectors", "description": "Semantic vector store for embeddings-based retrieval."},
        {"name": "Health", "description": "Liveness, readiness, and legacy health probes."},
        {"name": "Cost", "description": "A2A delegation cost attribution."},
        {"name": "Platform", "description": "Infrastructure: MCP server, custom routes, metrics, pprof, admin UI."},
    ]:
        if tag["name"] not in existing_tag_names:
            spec["tags"].append(tag)

    # -- securitySchemes --
    # Matches internal/auth/apikey.go: Authorization Bearer OR X-API-Key.
    # JWT auth uses Authorization Bearer as well.
    spec.setdefault("components", {})
    spec["components"]["securitySchemes"] = {
        "BearerAuth": {
            "type": "http",
            "scheme": "bearer",
            "description": (
                "JWT (auth.type=jwt) or opaque API key (auth.type=api_key) "
                "presented as Authorization: Bearer <token>."
            ),
        },
        "ApiKeyAuth": {
            "type": "apiKey",
            "in": "header",
            "name": "X-API-Key",
            "description": (
                "Opaque API key via X-API-Key header (auth.type=api_key). "
                "Accepted as an alternative to Authorization: Bearer."
            ),
        },
    }
    # Either scheme satisfies auth when configured; listed as alternatives.
    spec["security"] = [{"BearerAuth": []}, {"ApiKeyAuth": []}]

    # -- add new component schemas --
    for name, schema in _new_schemas().items():
        spec["components"]["schemas"][name] = schema

    # -- fix if_match_version on ThreadPatch --
    tp = spec["components"]["schemas"].get("ThreadPatch", {}).get("properties", {})
    if "if_match_version" not in tp:
        tp["if_match_version"] = {
            "type": "integer",
            "title": "If Match Version",
            "description": "Optimistic concurrency: update only if the thread's current version equals this value.",
        }

    # -- extend Run schema with A2A fields --
    run_extra_props = spec["components"]["schemas"].get("Run", {})
    # Run is allOf — find the second element's properties
    if "allOf" in run_extra_props:
        for part in run_extra_props["allOf"]:
            props = part.get("properties", {})
            if "run_id" in props:
                if "parent_run_id" not in props:
                    props["parent_run_id"] = {"type": "string", "format": "uuid", "description": "Parent run in an A2A delegation chain."}
                if "root_run_id" not in props:
                    props["root_run_id"] = {"type": "string", "format": "uuid", "description": "Root of the delegation tree."}
                if "depth" not in props:
                    props["depth"] = {"type": "integer", "description": "Delegation depth (0 for top-level runs)."}
                if "assistant_id" not in props:
                    props["assistant_id"] = {"type": "string", "description": "SDK compat: mirrors agent_id."}
                break

    # -- extend RunCreate with run_id, assistant_id --
    rc = spec["components"]["schemas"].get("RunCreate", {}).get("properties", {})
    if "run_id" not in rc:
        rc["run_id"] = {"type": "string", "description": "Client-supplied run ID for idempotent creation."}
    if "assistant_id" not in rc:
        rc["assistant_id"] = {"type": "string", "description": "SDK compat alias for agent_id."}

    # -- correct cancel_run responses + honest rollback wording --
    # Implementation returns 200+Run when an updated run is available, else 204.
    cancel_op = spec["paths"].get("/runs/{run_id}/cancel", {}).get("post")
    if cancel_op is not None:
        cancel_op["description"] = (
            "Cancel a background run. `wait` controls whether the HTTP response waits for "
            "the post-cancel grace window; `action=interrupt` (default) cancels only; "
            "`action=rollback` also deletes the run record. Checkpoints are NOT deleted on "
            "rollback — they are keyed by thread_id with no per-run attribution, and in "
            "direct mode live in LangGraph's own tables outside this control plane's state store."
        )
        for param in cancel_op.get("parameters", []):
            if param.get("name") == "action":
                param["description"] = (
                    "interrupt: cancel only. rollback: cancel and delete the run record "
                    "(does not delete checkpoints — see operation description)."
                )
        cancel_op["responses"] = {
            **_json_response("200", "Cancelled; returns the updated run when available.", _ref("Run")),
            "204": {"description": "Cancelled; no run body to return (e.g. already terminal / deleted)."},
            "404": _ERR_404,
            "422": _ERR_422,
        }

    # -- fix WebSocket path --
    # Remove GET from /threads/{thread_id}/stream if present
    thread_stream = spec["paths"].get("/threads/{thread_id}/stream", {})
    ws_op = thread_stream.pop("get", None)

    # Add /threads/{thread_id}/websocket with the ws operation
    if ws_op:
        ws_op["operationId"] = "open_thread_websocket_stream"
        ws_op["description"] = (
            "Upgrade to a WebSocket connection for a thread. "
            "After the connection is upgraded, clients send streaming protocol commands in-band "
            "and receive command responses plus unsolicited events on the same connection."
        )
        spec["paths"]["/threads/{thread_id}/websocket"] = {"get": ws_op}

    # -- add missing PUBLIC paths --
    paths = spec["paths"]

    # Thread runs
    paths.setdefault("/threads/{thread_id}/runs", {})["post"] = {
        "tags": ["Runs"],
        "summary": "Create Thread Run",
        "operationId": "create_thread_run",
        "parameters": [_THREAD_ID_PARAM],
        "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("RunCreate")}}},
        "responses": {**_json_response("200", "Success", _ref("Run")), "404": _ERR_404, "409": _ERR_409, "422": _ERR_422},
    }
    paths.setdefault("/threads/{thread_id}/runs", {})["get"] = {
        "tags": ["Runs"],
        "summary": "List Thread Runs",
        "operationId": "list_thread_runs",
        "parameters": [_THREAD_ID_PARAM],
        "responses": {**_json_response("200", "Success", _array_of(_ref("Run"))), "404": _ERR_404},
    }
    paths["/threads/{thread_id}/runs/stream"] = {
        "post": {
            "tags": ["Runs"],
            "summary": "Create Thread Run, Stream Output",
            "operationId": "create_and_stream_thread_run",
            "parameters": [_THREAD_ID_PARAM],
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("RunStream")}}},
            "responses": {
                "200": {"description": "SSE event stream", "content": {"text/event-stream": {"schema": {"type": "string"}}}},
                "404": _ERR_404, "409": _ERR_409, "422": _ERR_422,
            },
        }
    }
    paths["/threads/{thread_id}/runs/wait"] = {
        "post": {
            "tags": ["Runs"],
            "summary": "Create Thread Run, Wait for Output",
            "operationId": "create_and_wait_thread_run",
            "parameters": [_THREAD_ID_PARAM],
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("RunCreate")}}},
            "responses": {**_json_response("200", "Success", _ref("RunWaitResponse")), "404": _ERR_404, "409": _ERR_409, "422": _ERR_422},
        }
    }
    paths["/threads/{thread_id}/runs/{run_id}"] = {
        "get": {
            "tags": ["Runs"],
            "summary": "Get Thread Run",
            "operationId": "get_thread_run",
            "parameters": [_THREAD_ID_PARAM, _RUN_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("Run")), "404": _ERR_404},
        },
        "delete": {
            "tags": ["Runs"],
            "summary": "Delete Thread Run",
            "operationId": "delete_thread_run",
            "parameters": [_THREAD_ID_PARAM, _RUN_ID_PARAM],
            "responses": {"204": {"description": "Success"}, "404": _ERR_404},
        },
    }
    paths["/threads/{thread_id}/runs/{run_id}/stream"] = {
        "get": {
            "tags": ["Runs"],
            "summary": "Stream Thread Run Output",
            "operationId": "stream_thread_run",
            "parameters": [_THREAD_ID_PARAM, _RUN_ID_PARAM],
            "responses": {
                "200": {"description": "SSE event stream", "content": {"text/event-stream": {"schema": {"type": "string"}}}},
                "404": _ERR_404,
            },
        }
    }
    paths["/threads/{thread_id}/runs/{run_id}/wait"] = {
        "get": {
            "tags": ["Runs"],
            "summary": "Wait for Thread Run Output",
            "operationId": "wait_thread_run",
            "parameters": [_THREAD_ID_PARAM, _RUN_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("RunWaitResponse")), "404": _ERR_404},
        }
    }
    paths["/threads/{thread_id}/runs/{run_id}/cancel"] = {
        "post": {
            "tags": ["Runs"],
            "summary": "Cancel Thread Run",
            "description": (
                "Cancel a run on a thread. Query params match POST /runs/{run_id}/cancel: "
                "`wait` controls whether the HTTP response waits for the post-cancel grace "
                "window; `action=interrupt` (default) cancels only; `action=rollback` also "
                "deletes the run record. Checkpoints are NOT deleted on rollback — they are "
                "keyed by thread_id with no per-run attribution, and in direct mode live in "
                "LangGraph's own tables outside this control plane's state store."
            ),
            "operationId": "cancel_thread_run",
            "parameters": [
                _THREAD_ID_PARAM, _RUN_ID_PARAM,
                {
                    "name": "wait",
                    "in": "query",
                    "required": False,
                    "schema": {"type": "boolean", "default": False},
                    "description": "If true, wait for the post-cancel grace window before responding.",
                },
                {
                    "name": "action",
                    "in": "query",
                    "required": False,
                    "schema": {"type": "string", "enum": ["interrupt", "rollback"], "default": "interrupt"},
                    "description": (
                        "interrupt: cancel only. rollback: cancel and delete the run record "
                        "(does not delete checkpoints — see operation description)."
                    ),
                },
            ],
            "responses": {
                **_json_response("200", "Cancelled; returns the updated run when available.", _ref("Run")),
                "204": {"description": "Cancelled; no run body to return (e.g. already terminal / deleted)."},
                "404": _ERR_404,
            },
        }
    }

    # Thread state/history
    paths["/threads/{thread_id}/state"] = {
        "get": {
            "tags": ["Threads"],
            "summary": "Get Thread State",
            "operationId": "get_thread_state",
            "parameters": [_THREAD_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("ThreadState")), "404": _ERR_404},
        },
        "post": {
            "tags": ["Threads"],
            "summary": "Update Thread State",
            "operationId": "update_thread_state",
            "parameters": [_THREAD_ID_PARAM],
            "requestBody": {
                "required": True,
                "content": {"application/json": {"schema": {
                    "type": "object",
                    "properties": {
                        "values": {"type": "object"},
                        "as_node": {"type": "string"},
                        "checkpoint_id": {"type": "string"},
                    },
                }}},
            },
            "responses": {**_json_response("200", "Success", _ref("ThreadUpdateStateResponse")), "404": _ERR_404, "422": _ERR_422},
        },
    }

    # POST /threads/{thread_id}/history
    hist = paths.get("/threads/{thread_id}/history", {})
    hist["post"] = {
        "tags": ["Threads"],
        "summary": "Search Thread History",
        "description": "Search thread history with a request body (limit, before checkpoint).",
        "operationId": "search_thread_history",
        "parameters": [_THREAD_ID_PARAM],
        "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("ThreadHistoryRequest")}}},
        "responses": {**_json_response("200", "Success", _array_of(_ref("ThreadState"))), "404": _ERR_404, "422": _ERR_422},
    }
    paths["/threads/{thread_id}/history"] = hist

    # Agent versions
    paths["/agents/{agent_id}/versions"] = {
        "get": {
            "tags": ["Agents"],
            "summary": "List Agent Versions",
            "operationId": "list_agent_versions",
            "parameters": [_AGENT_ID_PARAM],
            "responses": {**_json_response("200", "Success", _array_of(_ref("AgentVersion"))), "404": _ERR_404},
        }
    }
    paths["/agents/{agent_id}/versions/{version}/rollback"] = {
        "post": {
            "tags": ["Agents"],
            "summary": "Rollback Agent to Version",
            "operationId": "rollback_agent_version",
            "parameters": [_AGENT_ID_PARAM, _VERSION_PARAM],
            "responses": {**_json_response("200", "Success", _ref("Agent")), "404": _ERR_404},
        }
    }

    # Assistants (LangGraph SDK aliases). Go mux param is {agentID} →
    # document as {agent_id} for coverage parity; value is the assistant/agent id.
    # Drop any stale {assistant_id} paths from earlier builds (duplicate operationIds).
    paths.pop("/assistants/{assistant_id}", None)
    paths.pop("/assistants/{assistant_id}/schemas", None)
    paths["/assistants/search"] = {
        "post": {
            "tags": ["Assistants"],
            "summary": "Search Assistants",
            "description": "LangGraph SDK compatibility alias for POST /agents/search. Returns Assistant views.",
            "operationId": "search_assistants",
            "requestBody": {"required": True, "content": {"application/json": {"schema": {
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "metadata": {"type": "object"},
                    "limit": {"type": "integer", "default": 10},
                    "offset": {"type": "integer", "default": 0},
                },
            }}}},
            "responses": {**_json_response("200", "Success", _array_of(_ref("Assistant")))},
        }
    }
    paths["/assistants/{agent_id}"] = {
        "get": {
            "tags": ["Assistants"],
            "summary": "Get Assistant",
            "description": "LangGraph SDK compatibility alias for GET /agents/{agent_id}.",
            "operationId": "get_assistant",
            "parameters": [_AGENT_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("Assistant")), "404": _ERR_404},
        }
    }
    paths["/assistants/{agent_id}/schemas"] = {
        "get": {
            "tags": ["Assistants"],
            "summary": "Get Assistant Schemas",
            "description": "LangGraph SDK compatibility alias for GET /agents/{agent_id}/schemas.",
            "operationId": "get_assistant_schemas",
            "parameters": [_AGENT_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("GraphSchema")), "404": _ERR_404},
        }
    }

    # Registry
    paths["/registry/entries/{name}"] = {
        "put": {
            "tags": ["Registry"],
            "summary": "Publish Registry Entry",
            "operationId": "publish_registry_entry",
            "parameters": [_NAME_PARAM],
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("RegistryEntry")}}},
            "responses": {**_json_response("200", "Success", _ref("RegistryEntry")), "422": _ERR_422},
        },
        "get": {
            "tags": ["Registry"],
            "summary": "Get Registry Entry",
            "operationId": "get_registry_entry",
            "parameters": [_NAME_PARAM],
            "responses": {**_json_response("200", "Success", _ref("RegistryEntry")), "404": _ERR_404},
        },
        "delete": {
            "tags": ["Registry"],
            "summary": "Delete Registry Entry",
            "operationId": "delete_registry_entry",
            "parameters": [_NAME_PARAM],
            "responses": {"204": {"description": "Success"}, "404": _ERR_404},
        },
    }
    paths["/registry/search"] = {
        "post": {
            "tags": ["Registry"],
            "summary": "Search Registry",
            "operationId": "search_registry",
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("RegistrySearchRequest")}}},
            "responses": {**_json_response("200", "Success", _array_of(_ref("RegistryEntry")))},
        }
    }
    paths["/registry/entries/{name}/versions"] = {
        "get": {
            "tags": ["Registry"],
            "summary": "List Registry Entry Versions",
            "operationId": "list_registry_entry_versions",
            "parameters": [_NAME_PARAM],
            "responses": {**_json_response("200", "Success", _array_of(_ref("RegistryEntryVersion"))), "404": _ERR_404},
        }
    }
    paths["/registry/entries/{name}/versions/{version}"] = {
        "get": {
            "tags": ["Registry"],
            "summary": "Get Registry Entry Version",
            "operationId": "get_registry_entry_version",
            "parameters": [_NAME_PARAM, _VERSION_PARAM],
            "responses": {**_json_response("200", "Success", _ref("RegistryEntryVersion")), "404": _ERR_404},
        }
    }

    # Vectors
    paths["/vectors/items"] = {
        "put": {
            "tags": ["Vectors"],
            "summary": "Upsert Vector Item",
            "operationId": "upsert_vector_item",
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("VectorUpsertRequest")}}},
            "responses": {"204": {"description": "Success"}, "422": _ERR_422},
        },
        "delete": {
            "tags": ["Vectors"],
            "summary": "Delete Vector Item",
            "operationId": "delete_vector_item",
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("VectorDeleteRequest")}}},
            "responses": {"204": {"description": "Success"}, "404": _ERR_404},
        },
    }
    paths["/vectors/search"] = {
        "post": {
            "tags": ["Vectors"],
            "summary": "Search Vectors",
            "operationId": "search_vectors",
            "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("VectorSearchRequest")}}},
            "responses": {**_json_response("200", "Success", _ref("VectorSearchResponse"))},
        }
    }

    # Health
    paths["/health"] = {
        "get": {
            "tags": ["Health"],
            "summary": "Health Check",
            "operationId": "health_check",
            "security": [],
            "responses": {**_json_response("200", "Success", {"type": "object", "properties": {"status": {"type": "string"}}})},
        }
    }
    paths["/livez"] = {
        "get": {
            "tags": ["Health"],
            "summary": "Liveness Probe",
            "operationId": "liveness_probe",
            "security": [],
            "responses": {**_json_response("200", "Success", {"type": "object", "properties": {"status": {"type": "string"}}})},
        }
    }
    paths["/readyz"] = {
        "get": {
            "tags": ["Health"],
            "summary": "Readiness Probe",
            "description": "Verifies connectivity to all backend dependencies (store, queue, broker). Returns 503 if any check fails.",
            "operationId": "readiness_probe",
            "security": [],
            "responses": {
                "200": {"description": "Ready", "content": {"application/json": {"schema": {
                    "type": "object",
                    "properties": {
                        "status": {"type": "string"},
                        "checks": {"type": "object", "additionalProperties": {"type": "string"}},
                    },
                }}}},
                "503": {"description": "Not Ready", "content": {"application/json": {"schema": {
                    "type": "object",
                    "properties": {
                        "status": {"type": "string"},
                        "checks": {"type": "object", "additionalProperties": {"type": "string"}},
                    },
                }}}},
            },
        }
    }

    # Cost
    paths["/runs/{run_id}/cost"] = {
        "get": {
            "tags": ["Cost"],
            "summary": "Get Run Cost Summary",
            "description": "Aggregate token usage and cost across an entire A2A delegation tree. Any run in the tree resolves to the same root and the same rollup.",
            "operationId": "get_run_cost",
            "parameters": [_RUN_ID_PARAM],
            "responses": {**_json_response("200", "Success", _ref("RunCostSummary")), "404": _ERR_404},
        }
    }

    # Platform: metrics
    paths["/metrics"] = {
        "get": {
            "tags": ["Platform"],
            "summary": "Prometheus Metrics",
            "description": "Prometheus-format metrics endpoint. Served outside client auth middleware. When RUNKITE_METRICS_TOKEN is set, requires Authorization: Bearer <token> or X-Runkite-Metrics-Token.",
            "operationId": "get_metrics",
            "security": [],
            "responses": {"200": {"description": "Prometheus text exposition format", "content": {"text/plain": {"schema": {"type": "string"}}}}},
        }
    }
    paths["/metrics/"] = {
        "get": {
            "tags": ["Platform"],
            "summary": "Prometheus Metrics (trailing slash)",
            "operationId": "get_metrics_slash",
            "security": [],
            "responses": {"200": {"description": "Prometheus text exposition format", "content": {"text/plain": {"schema": {"type": "string"}}}}},
        }
    }

    # Platform: MCP
    paths["/mcp"] = {
        "get": {
            "tags": ["Platform"],
            "summary": "MCP SSE Stream",
            "description": "MCP Streamable HTTP transport — optional Server-Sent Events stream for server-initiated notifications.",
            "operationId": "mcp_sse",
            "responses": {"200": {"description": "SSE stream", "content": {"text/event-stream": {"schema": {"type": "string"}}}}},
        },
        "post": {
            "tags": ["Platform"],
            "summary": "MCP JSON-RPC Request",
            "description": "MCP Streamable HTTP transport — POST for JSON-RPC requests to Runkite's MCP server surface (agents exposed as MCP tools).",
            "operationId": "mcp_rpc",
            "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}},
            "responses": {**_json_response("200", "Success", {"type": "object"})},
        },
        "delete": {
            "tags": ["Platform"],
            "summary": "MCP Session Close",
            "description": "MCP Streamable HTTP transport — DELETE to close an MCP session.",
            "operationId": "mcp_close",
            "responses": {"204": {"description": "Session closed"}},
        },
    }

    # Platform: custom routes
    paths["/custom/"] = {
        "get": {
            "tags": ["Platform"],
            "summary": "Custom Routes (proxy)",
            "description": "Reverse-proxy prefix for user-defined HTTP endpoints (default mount /custom; configurable via custom_routes.mount). All methods are proxied; GET is representative. Identity is injected as X-Runkite-* headers. 404 if custom_routes is not configured.",
            "operationId": "custom_routes_proxy",
            "responses": {"200": {"description": "Proxied response"}, "404": _ERR_404},
        }
    }

    # Platform: admin UI
    paths["/admin/"] = {
        "get": {
            "tags": ["Platform"],
            "summary": "Admin UI",
            "description": "Serves the embedded React admin dashboard. Public at the HTTP layer; the dashboard's own login screen gates access via /admin-api/*.",
            "operationId": "admin_ui",
            "security": [],
            "responses": {"200": {"description": "HTML page", "content": {"text/html": {"schema": {"type": "string"}}}}},
        }
    }

    # Platform: pprof
    for pprof_path, op_id, summary in [
        ("/debug/pprof/", "pprof_index", "pprof Index"),
        ("/debug/pprof/cmdline", "pprof_cmdline", "pprof Cmdline"),
        ("/debug/pprof/profile", "pprof_profile", "pprof CPU Profile"),
        ("/debug/pprof/symbol", "pprof_symbol", "pprof Symbol Lookup"),
        ("/debug/pprof/trace", "pprof_trace", "pprof Execution Trace"),
    ]:
        paths[pprof_path] = {
            "get": {
                "tags": ["Platform"],
                "summary": summary,
                "description": f"Go runtime profiling endpoint. Only registered when RUNKITE_PPROF=1.",
                "operationId": op_id,
                "security": [],
                "responses": {"200": {"description": "Profile data"}},
            }
        }

    return spec


# ---------------------------------------------------------------------------
# ADMIN spec builder
# ---------------------------------------------------------------------------


def _admin_agent_view() -> dict:
    return {
        "type": "object",
        "properties": {
            "agent_id": {"type": "string"},
            "name": {"type": "string"},
            "description": {"type": "string"},
            "metadata": {"type": "object"},
            "capabilities": {"type": "object"},
            "version": {"type": "integer"},
            "tenant_id": {"type": "string"},
        },
        "title": "AdminAgentView",
    }


def _admin_thread_view() -> dict:
    return {
        "type": "object",
        "properties": {
            "thread_id": {"type": "string", "format": "uuid"},
            "created_at": {"type": "string", "format": "date-time"},
            "updated_at": {"type": "string", "format": "date-time"},
            "metadata": {"type": "object"},
            "status": {"type": "string"},
            "values": {"type": "object"},
            "version": {"type": "integer"},
            "tenant_id": {"type": "string"},
        },
        "title": "AdminThreadView",
    }


def _admin_run_view() -> dict:
    return {
        "type": "object",
        "properties": {
            "run_id": {"type": "string", "format": "uuid"},
            "thread_id": {"type": "string"},
            "agent_id": {"type": "string"},
            "status": {"type": "string"},
            "created_at": {"type": "string", "format": "date-time"},
            "updated_at": {"type": "string", "format": "date-time"},
            "metadata": {"type": "object"},
            "error": {"type": "string"},
            "tenant_id": {"type": "string"},
        },
        "title": "AdminRunView",
    }


def _build_admin_spec() -> dict:
    a_id = _path_param("agent_id", "Agent ID.", fmt="")
    t_id = _path_param("thread_id", "Thread ID.")
    r_id = _path_param("run_id", "Run ID.")
    n_param = _path_param("name", "Registry entry name.", fmt="")
    dl_id = _path_param("id", "Dead letter ID.", fmt="")
    # Matches adminListPaging in internal/api/admin.go (default 50, max 200).
    admin_page = (
        {"name": "limit", "in": "query", "required": False, "schema": {"type": "integer", "default": 50, "maximum": 200}},
        {"name": "offset", "in": "query", "required": False, "schema": {"type": "integer", "default": 0, "minimum": 0}},
    )

    return {
        "openapi": "3.1.0",
        "info": {"title": "Runkite Admin API", "version": "0.1.0", "description": "Cross-tenant administrative API for the Runkite control plane. Requires admin permission."},
        "tags": [{"name": "Admin", "description": "Administrative endpoints for managing the Runkite deployment."}],
        "security": [{"BearerAuth": []}, {"ApiKeyAuth": []}],
        "components": {
            "securitySchemes": {
                "BearerAuth": {
                    "type": "http",
                    "scheme": "bearer",
                    "description": "Admin key (auth.admin_keys) or primary auth credential with admin permission.",
                },
                "ApiKeyAuth": {
                    "type": "apiKey",
                    "in": "header",
                    "name": "X-API-Key",
                    "description": "Admin API key via X-API-Key (alternative to Bearer).",
                },
            },
            "schemas": {
                "AdminOverview": {
                    "type": "object",
                    "properties": {
                        "total_agents": {"type": "integer"},
                        "total_threads": {"type": "integer"},
                        "threads_by_status": {"type": "object", "additionalProperties": {"type": "integer"}},
                        "total_runs": {"type": "integer"},
                        "runs_by_status": {"type": "object", "additionalProperties": {"type": "integer"}},
                        "connector_count": {"type": "integer"},
                        "cron_schedule_count": {"type": "integer"},
                    },
                    "title": "AdminOverview",
                },
                "AdminAgentView": _admin_agent_view(),
                "AdminThreadView": _admin_thread_view(),
                "AdminRunView": _admin_run_view(),
                "AdminRegistryEntryView": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "display_name": {"type": "string"},
                        "description": {"type": "string"}, "author": {"type": "string"},
                        "tags": {"type": "array", "items": {"type": "string"}},
                        "source_type": {"type": "string"}, "source_ref": {"type": "string"},
                        "metadata": {"type": "object"}, "version": {"type": "integer"},
                        "created_at": {"type": "string", "format": "date-time"},
                        "updated_at": {"type": "string", "format": "date-time"},
                        "tenant_id": {"type": "string"},
                    },
                    "title": "AdminRegistryEntryView",
                },
                "RedeliverWebhookResponse": {
                    "type": "object",
                    "properties": {
                        "delivered": {"type": "boolean"},
                        "status_code": {"type": "integer"},
                        "error": {"type": "string"},
                    },
                    "title": "RedeliverWebhookResponse",
                },
                "ErrorResponse": {"type": "object", "properties": {"message": {"type": "string"}}, "title": "ErrorResponse"},
                "ConnectorInfo": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "type": {"type": "string"},
                        "mcp": {"type": "string"}, "circuit_breaker_state": {"type": "string"},
                    },
                    "title": "ConnectorInfo",
                },
                "CronSchedule": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "agent_id": {"type": "string"},
                        "expression": {"type": "string"}, "timezone": {"type": "string"},
                        "enabled": {"type": "boolean"},
                        "created_at": {"type": "string", "format": "date-time"},
                        "updated_at": {"type": "string", "format": "date-time"},
                    },
                    "title": "CronSchedule",
                },
                "WebhookDeadLetter": {
                    "type": "object",
                    "properties": {
                        "id": {"type": "string"}, "tenant_id": {"type": "string"},
                        "url": {"type": "string"}, "event_type": {"type": "string"},
                        "run_id": {"type": "string"}, "payload": {"type": "object"},
                        "error": {"type": "string"}, "attempts": {"type": "integer"},
                        "failed_at": {"type": "string", "format": "date-time"},
                    },
                    "title": "WebhookDeadLetter",
                },
            },
        },
        "paths": {
            "/admin-api/session": {
                "post": {
                    "tags": ["Admin"],
                    "summary": "Create Admin Session",
                    "description": "Exchange an API key/JWT for an httpOnly session cookie + CSRF token. The credential is not retained by the browser afterward.",
                    "operationId": "admin_create_session",
                    "requestBody": {
                        "required": True,
                        "content": {
                            "application/json": {
                                "schema": {
                                    "type": "object",
                                    "properties": {"credential": {"type": "string"}},
                                }
                            }
                        },
                    },
                    "responses": {
                        **_json_response(
                            "200",
                            "Session created (Set-Cookie: runkite_admin_session)",
                            {
                                "type": "object",
                                "properties": {
                                    "csrf_token": {"type": "string"},
                                    "identity": {"type": "string"},
                                    "auth_required": {"type": "boolean"},
                                },
                            },
                        ),
                        "401": {"description": "Unauthorized", "content": {"application/json": {"schema": _ref("ErrorResponse")}}},
                        "403": {"description": "Forbidden", "content": {"application/json": {"schema": _ref("ErrorResponse")}}},
                    },
                },
                "get": {
                    "tags": ["Admin"],
                    "summary": "Admin Session Status",
                    "description": "Bootstrap probe: returns whether a session cookie is active (and its CSRF token), or whether auth is required at all.",
                    "operationId": "admin_session_status",
                    "responses": {
                        **_json_response(
                            "200",
                            "Success",
                            {
                                "type": "object",
                                "properties": {
                                    "authenticated": {"type": "boolean"},
                                    "auth_required": {"type": "boolean"},
                                    "csrf_token": {"type": "string"},
                                    "identity": {"type": "string"},
                                },
                            },
                        )
                    },
                },
                "delete": {
                    "tags": ["Admin"],
                    "summary": "Destroy Admin Session",
                    "description": "Logout. Requires the session cookie and X-CSRF-Token (unlike Bearer machine clients).",
                    "operationId": "admin_destroy_session",
                    "parameters": [
                        {
                            "name": "X-CSRF-Token",
                            "in": "header",
                            "required": True,
                            "schema": {"type": "string"},
                        }
                    ],
                    "responses": {
                        "204": {"description": "Logged out"},
                        "403": {"description": "Invalid or missing CSRF token", "content": {"application/json": {"schema": _ref("ErrorResponse")}}},
                    },
                },
            },
            "/admin-api/overview": {"get": {"tags": ["Admin"], "summary": "Admin Overview", "operationId": "admin_overview", "responses": {**_json_response("200", "Success", _ref("AdminOverview"))}}},
            "/admin-api/agents": {"get": {"tags": ["Admin"], "summary": "List All Agents", "operationId": "admin_list_agents", "parameters": list(admin_page), "responses": {**_json_response("200", "Success", _array_of(_ref("AdminAgentView")))}}},
            "/admin-api/agents/{agent_id}": {"get": {"tags": ["Admin"], "summary": "Get Agent (admin)", "operationId": "admin_get_agent", "parameters": [a_id, {"name": "tenant_id", "in": "query", "required": False, "schema": {"type": "string"}}], "responses": {**_json_response("200", "Success", _ref("AdminAgentView")), "404": _ERR_404}}},
            "/admin-api/registry": {"get": {"tags": ["Admin"], "summary": "List Registry Entries", "operationId": "admin_list_registry", "parameters": list(admin_page), "responses": {**_json_response("200", "Success", _array_of(_ref("AdminRegistryEntryView")))}}},
            "/admin-api/registry/{name}": {"get": {"tags": ["Admin"], "summary": "Get Registry Entry (admin)", "operationId": "admin_get_registry_entry", "parameters": [n_param, {"name": "tenant_id", "in": "query", "required": False, "schema": {"type": "string"}}], "responses": {**_json_response("200", "Success", _ref("AdminRegistryEntryView")), "404": _ERR_404}}},
            "/admin-api/registry/{name}/versions": {"get": {"tags": ["Admin"], "summary": "List Registry Versions (admin)", "operationId": "admin_list_registry_versions", "parameters": [n_param, {"name": "tenant_id", "in": "query", "required": False, "schema": {"type": "string"}}], "responses": {**_json_response("200", "Success", _array_of({"type": "object"}))}}},
            "/admin-api/threads": {"get": {"tags": ["Admin"], "summary": "List All Threads", "operationId": "admin_list_threads", "parameters": [{"name": "status", "in": "query", "required": False, "schema": {"type": "string"}}, *admin_page], "responses": {**_json_response("200", "Success", _array_of(_ref("AdminThreadView")))}}},
            "/admin-api/threads/{thread_id}": {
                "get": {"tags": ["Admin"], "summary": "Get Thread (admin)", "operationId": "admin_get_thread", "parameters": [t_id], "responses": {**_json_response("200", "Success", _ref("AdminThreadView")), "404": _ERR_404}},
                "delete": {"tags": ["Admin"], "summary": "Delete Thread (admin)", "operationId": "admin_delete_thread", "parameters": [t_id], "responses": {"204": {"description": "Success"}, "404": _ERR_404}},
            },
            "/admin-api/threads/{thread_id}/runs": {"get": {"tags": ["Admin"], "summary": "List Thread Runs (admin)", "operationId": "admin_list_thread_runs", "parameters": [t_id, *admin_page], "responses": {**_json_response("200", "Success", _array_of(_ref("AdminRunView")))}}},
            "/admin-api/runs": {"get": {"tags": ["Admin"], "summary": "List All Runs", "operationId": "admin_list_runs", "parameters": [
                {"name": "status", "in": "query", "required": False, "schema": {"type": "string"}},
                {"name": "agent_id", "in": "query", "required": False, "schema": {"type": "string"}},
                {"name": "thread_id", "in": "query", "required": False, "schema": {"type": "string"}},
                *admin_page,
            ], "responses": {**_json_response("200", "Success", _array_of(_ref("AdminRunView")))}}},
            "/admin-api/runs/{run_id}": {"get": {"tags": ["Admin"], "summary": "Get Run (admin)", "operationId": "admin_get_run", "parameters": [r_id], "responses": {**_json_response("200", "Success", _ref("AdminRunView")), "404": _ERR_404}}},
            "/admin-api/runs/{run_id}/stream": {"get": {"tags": ["Admin"], "summary": "Stream Run (admin)", "operationId": "admin_stream_run", "parameters": [r_id], "responses": {"200": {"description": "SSE event stream", "content": {"text/event-stream": {"schema": {"type": "string"}}}}}}},
            "/admin-api/runs/{run_id}/cancel": {"post": {"tags": ["Admin"], "summary": "Cancel Run (admin)", "operationId": "admin_cancel_run", "parameters": [r_id], "responses": {"204": {"description": "Success"}, "404": _ERR_404}}},
            "/admin-api/connectors": {"get": {"tags": ["Admin"], "summary": "List Connectors (admin)", "operationId": "admin_list_connectors", "responses": {**_json_response("200", "Success", _array_of(_ref("ConnectorInfo")))}}},
            "/admin-api/connectors/{name}": {"get": {"tags": ["Admin"], "summary": "Get Connector (admin)", "operationId": "admin_get_connector", "parameters": [n_param], "responses": {**_json_response("200", "Success", _ref("ConnectorInfo")), "404": _ERR_404}}},
            "/admin-api/cron": {"get": {"tags": ["Admin"], "summary": "List Cron Schedules (admin)", "operationId": "admin_list_cron", "responses": {**_json_response("200", "Success", _array_of(_ref("CronSchedule")))}}},
            "/admin-api/webhooks/dead-letters": {"get": {"tags": ["Admin"], "summary": "List Dead Letters", "operationId": "admin_list_dead_letters", "parameters": [{"name": "limit", "in": "query", "required": False, "schema": {"type": "integer", "default": 50}}], "responses": {**_json_response("200", "Success", _array_of(_ref("WebhookDeadLetter")))}}},
            "/admin-api/webhooks/dead-letters/{id}/redeliver": {"post": {"tags": ["Admin"], "summary": "Redeliver Dead Letter", "operationId": "admin_redeliver_dead_letter", "parameters": [dl_id], "responses": {**_json_response("200", "Success", _ref("RedeliverWebhookResponse")), "404": _ERR_404}}},
        },
    }


# ---------------------------------------------------------------------------
# INTERNAL spec builder
# ---------------------------------------------------------------------------


def _build_internal_spec() -> dict:
    r_id = _path_param("run_id", "Run ID.")
    a_id = _path_param("agent_id", "Agent ID.", fmt="")
    n_param = _path_param("name", "Connector name.", fmt="")

    return {
        "openapi": "3.1.0",
        "info": {"title": "Runkite Internal (Runner) API", "version": "0.1.0", "description": "Runner-facing internal API. Authenticated via X-Runner-Kind + X-Runner-Token headers."},
        "tags": [
            {"name": "Runner", "description": "Endpoints consumed by runner processes."},
            {"name": "Store", "description": "Proxy-mode store access for runners."},
            {"name": "Vectors", "description": "Proxy-mode vector store access for runners."},
        ],
        "security": [{"RunnerAuth": []}],
        "components": {
            "securitySchemes": {
                "RunnerAuth": {
                    "type": "apiKey",
                    "in": "header",
                    "name": "X-Runner-Token",
                    "description": "Runner authentication token. Must also include X-Runner-Kind header.",
                },
            },
            "schemas": {
                "A2ARunRequest": {
                    "type": "object",
                    "required": ["agent_id", "parent_run_id"],
                    "properties": {
                        "agent_id": {"type": "string"},
                        "thread_id": {"type": "string"},
                        "input": {"type": "object"},
                        "config": {"type": "object"},
                        "parent_run_id": {"type": "string"},
                        "on_behalf_of": {"type": "object", "description": "Auth context propagation for the sub-run."},
                        "wait": {"type": "boolean", "default": False},
                    },
                    "title": "A2ARunRequest",
                },
                "AgentSchema": {
                    "type": "object",
                    "required": ["agent_id", "input_schema", "output_schema"],
                    "properties": {
                        "agent_id": {"type": "string"},
                        "input_schema": {"type": "object"},
                        "output_schema": {"type": "object"},
                        "state_schema": {"type": "object"},
                        "config_schema": {"type": "object"},
                    },
                    "title": "AgentSchema",
                },
                "ConnectorInfo": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "type": {"type": "string"},
                        "mcp": {"type": "string"}, "circuit_breaker_state": {"type": "string"},
                    },
                    "title": "ConnectorInfo",
                },
                "ConnectorDetail": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "type": {"type": "string"},
                        "mcp": {"type": "string"}, "errors": {"type": "object"},
                        "tools": {"type": "object"}, "circuit_breaker_state": {"type": "string"},
                    },
                    "title": "ConnectorDetail",
                },
                "CronSchedule": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"}, "agent_id": {"type": "string"},
                        "expression": {"type": "string"}, "timezone": {"type": "string"},
                        "enabled": {"type": "boolean"},
                        "created_at": {"type": "string", "format": "date-time"},
                        "updated_at": {"type": "string", "format": "date-time"},
                    },
                    "title": "CronSchedule",
                },
                "WebhookDeadLetter": {
                    "type": "object",
                    "properties": {
                        "id": {"type": "string"}, "url": {"type": "string"},
                        "event_type": {"type": "string"}, "run_id": {"type": "string"},
                        "payload": {"type": "object"}, "error": {"type": "string"},
                        "attempts": {"type": "integer"},
                        "failed_at": {"type": "string", "format": "date-time"},
                    },
                    "title": "WebhookDeadLetter",
                },
                "Run": {"type": "object", "properties": {"run_id": {"type": "string"}, "status": {"type": "string"}}, "title": "Run"},
                "RunWaitResponse": {"type": "object", "properties": {"run": {"$ref": "#/components/schemas/Run"}, "values": {"type": "object"}}, "title": "RunWaitResponse"},
                "ErrorResponse": {"type": "object", "properties": {"message": {"type": "string"}}, "title": "ErrorResponse"},
            },
        },
        "paths": {
            "/internal/runs/{run_id}/status": {"get": {"tags": ["Runner"], "summary": "Get Run Status", "operationId": "get_run_status", "parameters": [r_id], "responses": {**_json_response("200", "Success", {"type": "object", "properties": {"status": {"type": "string"}}}), "404": _ERR_404}}},
            "/internal/agents/{agent_id}/schema": {"put": {"tags": ["Runner"], "summary": "Report Agent Schema", "operationId": "report_agent_schema", "parameters": [a_id], "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("AgentSchema")}}}, "responses": {"204": {"description": "Success"}}}},
            "/internal/connectors": {"get": {"tags": ["Runner"], "summary": "List Connectors", "operationId": "list_connectors", "responses": {**_json_response("200", "Success", _array_of(_ref("ConnectorInfo")))}}},
            "/internal/connectors/{name}": {"get": {"tags": ["Runner"], "summary": "Get Connector", "operationId": "get_connector", "parameters": [n_param], "responses": {**_json_response("200", "Success", _ref("ConnectorDetail")), "404": _ERR_404}}},
            "/internal/connectors/{name}/session": {"post": {"tags": ["Runner"], "summary": "Get Connector Session", "operationId": "get_connector_session", "parameters": [n_param], "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"user_context": {"type": "object"}}}}}}, "responses": {**_json_response("200", "Success", {"type": "object"}), "404": _ERR_404, "502": {"description": "Bad Gateway", "content": {"application/json": {"schema": _ref("ErrorResponse")}}}}}},
            "/internal/connectors/{name}/mcp": {"post": {"tags": ["Runner"], "summary": "Proxy MCP Request", "description": "Proxy one JSON-RPC request to a connector's downstream MCP server, enforcing tool allow/deny lists.", "operationId": "proxy_mcp_request", "parameters": [n_param], "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {**_json_response("200", "Success", {"type": "object"}), "404": _ERR_404, "502": {"description": "Bad Gateway", "content": {"application/json": {"schema": _ref("ErrorResponse")}}}}}},
            "/internal/cron": {"get": {"tags": ["Runner"], "summary": "List Cron Schedules", "operationId": "list_cron_schedules", "responses": {**_json_response("200", "Success", _array_of(_ref("CronSchedule")))}}},
            "/internal/store/items": {
                "put": {"tags": ["Store"], "summary": "Put Store Item (internal)", "operationId": "internal_put_item", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {"204": {"description": "Success"}}},
                "get": {"tags": ["Store"], "summary": "Get Store Item (internal)", "operationId": "internal_get_item", "parameters": [{"name": "key", "in": "query", "required": True, "schema": {"type": "string"}}], "responses": {**_json_response("200", "Success", {"type": "object"}), "404": _ERR_404}},
                "delete": {"tags": ["Store"], "summary": "Delete Store Item (internal)", "operationId": "internal_delete_item", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {"204": {"description": "Success"}}},
            },
            "/internal/store/items/search": {"post": {"tags": ["Store"], "summary": "Search Store Items (internal)", "operationId": "internal_search_items", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {**_json_response("200", "Success", {"type": "object"})}}},
            "/internal/store/namespaces": {"post": {"tags": ["Store"], "summary": "List Namespaces (internal)", "operationId": "internal_list_namespaces", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {**_json_response("200", "Success", {"type": "array", "items": {"type": "array", "items": {"type": "string"}}})}}},
            "/internal/vectors/items": {
                "put": {"tags": ["Vectors"], "summary": "Upsert Vector Item (internal)", "operationId": "internal_upsert_vector", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {"204": {"description": "Success"}}},
                "delete": {"tags": ["Vectors"], "summary": "Delete Vector Item (internal)", "operationId": "internal_delete_vector", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {"204": {"description": "Success"}}},
            },
            "/internal/vectors/search": {"post": {"tags": ["Vectors"], "summary": "Search Vectors (internal)", "operationId": "internal_search_vectors", "requestBody": {"required": True, "content": {"application/json": {"schema": {"type": "object"}}}}, "responses": {**_json_response("200", "Success", {"type": "object"})}}},
            "/internal/webhooks/dead-letters": {"get": {"tags": ["Runner"], "summary": "List Dead Letters", "operationId": "list_dead_letters", "parameters": [{"name": "limit", "in": "query", "required": False, "schema": {"type": "integer", "default": 50}}], "responses": {**_json_response("200", "Success", _array_of(_ref("WebhookDeadLetter")))}}},
            "/internal/a2a/runs": {"post": {"tags": ["Runner"], "summary": "Create A2A Delegated Run", "description": "Agent-to-Agent delegation: create a sub-run on behalf of a parent run. Requires parent_run_id for recursion limit enforcement.", "operationId": "create_a2a_run", "requestBody": {"required": True, "content": {"application/json": {"schema": _ref("A2ARunRequest")}}}, "responses": {**_json_response("200", "Success", _ref("Run")), "400": {"description": "Bad Request (depth exceeded)", "content": {"application/json": {"schema": _ref("ErrorResponse")}}}}}},
        },
    }


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _count_ops(spec: dict) -> int:
    count = 0
    for path_item in spec.get("paths", {}).values():
        for method in ("get", "put", "post", "delete", "patch", "head", "options", "trace"):
            if method in path_item:
                count += 1
    return count


def _write_spec(spec: dict, path: Path) -> int:
    path.parent.mkdir(parents=True, exist_ok=True)
    text = json.dumps(spec, indent=2, ensure_ascii=False) + "\n"
    path.write_text(text)
    return _count_ops(spec)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> None:
    public = _build_public_spec()
    admin = _build_admin_spec()
    internal = _build_internal_spec()

    n_pub = _write_spec(public, SPEC_DIR / "openapi.json")
    n_admin = _write_spec(admin, SPEC_DIR / "openapi-admin.json")
    n_internal = _write_spec(internal, SPEC_DIR / "openapi-internal.json")

    print(f"spec/openapi.json          — {n_pub} operations, {len(public['paths'])} paths")
    print(f"spec/openapi-admin.json    — {n_admin} operations, {len(admin['paths'])} paths")
    print(f"spec/openapi-internal.json — {n_internal} operations, {len(internal['paths'])} paths")


if __name__ == "__main__":
    main()
