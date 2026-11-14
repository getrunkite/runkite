#!/usr/bin/env python3
"""Verify every Go route extracted by extract.py appears in the appropriate
OpenAPI spec (after path-param normalization).

Exit 0 if complete; exit 1 with a missing-route listing otherwise.
"""

import json
import re
import sys
from pathlib import Path

# Allow importing extract from the same directory
sys.path.insert(0, str(Path(__file__).resolve().parent))
from extract import extract_routes  # noqa: E402

REPO_ROOT = Path(__file__).resolve().parent.parent.parent
SPEC_DIR = REPO_ROOT / "spec"


def _spec_methods_paths(spec: dict) -> set[str]:
    """Return a set of 'METHOD /path' from an OpenAPI spec."""
    result: set[str] = set()
    for path, item in spec.get("paths", {}).items():
        for method in ("get", "put", "post", "delete", "patch", "head", "options", "trace"):
            if method in item:
                result.add(f"{method.upper()} {path}")
    return result


def _load_spec(name: str) -> dict:
    p = SPEC_DIR / name
    if not p.exists():
        print(f"ERROR: {p} not found — run `python3 scripts/openapi/build.py` first", file=sys.stderr)
        sys.exit(1)
    return json.loads(p.read_text())


def _collect_refs(obj, acc: set[str] | None = None) -> set[str]:
    if acc is None:
        acc = set()
    if isinstance(obj, dict):
        if "$ref" in obj and isinstance(obj["$ref"], str):
            acc.add(obj["$ref"])
        for v in obj.values():
            _collect_refs(v, acc)
    elif isinstance(obj, list):
        for v in obj:
            _collect_refs(v, acc)
    return acc


def _check_spec_integrity(name: str, spec: dict) -> list[str]:
    """Return human-readable integrity problems (duplicate operationIds, broken $refs)."""
    problems: list[str] = []
    seen: dict[str, str] = {}
    for path, item in spec.get("paths", {}).items():
        for method, op in item.items():
            if method not in ("get", "put", "post", "delete", "patch", "head", "options", "trace"):
                continue
            if not isinstance(op, dict):
                continue
            oid = op.get("operationId")
            if not oid:
                problems.append(f"{name}: {method.upper()} {path} missing operationId")
                continue
            prev = seen.get(oid)
            if prev:
                problems.append(f"{name}: duplicate operationId {oid!r} ({prev} and {method.upper()} {path})")
            else:
                seen[oid] = f"{method.upper()} {path}"

    schemas = set(spec.get("components", {}).get("schemas", {}))
    responses = set(spec.get("components", {}).get("responses", {}))
    for ref in _collect_refs(spec):
        if ref.startswith("#/components/schemas/"):
            n = ref.rsplit("/", 1)[-1]
            if n not in schemas:
                problems.append(f"{name}: broken $ref {ref}")
        elif ref.startswith("#/components/responses/"):
            n = ref.rsplit("/", 1)[-1]
            if n not in responses:
                problems.append(f"{name}: broken $ref {ref}")
    return problems


def _schema_shape(spec: dict, schema: dict, seen: frozenset[str] = frozenset()) -> tuple[set[str], set[str]]:
    """Collect object properties and requirements through local refs/allOf."""
    fields = set(schema.get("properties", {}))
    required = set(schema.get("required", []))
    ref = schema.get("$ref", "")
    if ref.startswith("#/components/schemas/") and ref not in seen:
        target = spec.get("components", {}).get("schemas", {}).get(ref.rsplit("/", 1)[-1], {})
        nested_fields, nested_required = _schema_shape(spec, target, seen | {ref})
        fields.update(nested_fields)
        required.update(nested_required)
    for part in schema.get("allOf", []):
        nested_fields, nested_required = _schema_shape(spec, part, seen)
        fields.update(nested_fields)
        required.update(nested_required)
    return fields, required


def _check_model_contract(spec: dict) -> list[str]:
    """Catch field drift that route coverage cannot detect."""
    source = (REPO_ROOT / "internal" / "models" / "models.go").read_text()
    schemas = spec.get("components", {}).get("schemas", {})
    problems = []
    for model in ("RunCreate", "Run"):
        match = re.search(r"^type " + model + r" struct \{\n(.*?)^\}", source, re.M | re.S)
        if match is None:
            problems.append(f"cannot find Go model {model}")
            continue
        wire = set(re.findall(r'`json:"([^",]+)', match.group(1))) - {"-"}
        fields, _ = _schema_shape(spec, schemas.get(model, {}))
        for field in sorted(wire - fields):
            problems.append(f"openapi.json: {model} missing Go JSON field {field}")

    # Validate the actual endpoint request schema, including inherited fields.
    paths = (
        "/runs",
        "/runs/stream",
        "/runs/wait",
        "/threads/{thread_id}/runs",
        "/threads/{thread_id}/runs/stream",
        "/threads/{thread_id}/runs/wait",
    )
    expected, _ = _schema_shape(spec, schemas.get("RunCreate", {}))
    for path in paths:
        request = spec.get("paths", {}).get(path, {}).get("post", {}).get("requestBody", {})
        body = request.get("content", {}).get("application/json", {}).get("schema", {})
        fields, required = _schema_shape(spec, body)
        for field in sorted(expected - fields):
            problems.append(f"openapi.json: POST {path} request missing {field}")
        for field in sorted(required & {"created_at", "updated_at", "status", "run_id"}):
            problems.append(f"openapi.json: POST {path} requires server-assigned field {field}")
    return problems


def _check_run_manifest_contract(spec: dict, run_model: str = "Run") -> list[str]:
    """Keep the authenticated principal marker visible in actual response metadata."""
    source = (REPO_ROOT / "internal/models/models.go").read_text()
    schemas = spec.get("components", {}).get("schemas", {})
    problems = []
    for model in ("RunManifest", "RunManifestPrincipal"):
        match = re.search(r"^type " + model + r" struct \{\n(.*?)^\}", source, re.M | re.S)
        if not match:
            problems.append(f"cannot find Go model {model}")
            continue
        tags = re.findall(r'`json:"([^"`]+)"', match.group(1))
        expected = {tag.split(",", 1)[0] for tag in tags} - {"-"}
        expected_required = {tag.split(",", 1)[0] for tag in tags if "omitempty" not in tag.split(",")[1:]} - {"-"}
        schema = schemas.get(model, {})
        fields, required = _schema_shape(spec, schema)
        if fields != expected:
            problems.append(f"{model} fields differ from Go model")
        if required != expected_required:
            problems.append(f"{model} required fields differ from Go model")
        if schema.get("additionalProperties") is not False:
            problems.append(f"{model} must exclude undeclared authority fields")
    verified = schemas.get("RunManifestPrincipal", {}).get("properties", {}).get("verified_identity", {})
    if verified.get("type") != "boolean":
        problems.append("RunManifestPrincipal verified_identity must remain an optional boolean")
    principal = schemas.get("RunManifest", {}).get("properties", {}).get("principal", {})
    if principal.get("$ref") != "#/components/schemas/RunManifestPrincipal":
        problems.append("RunManifest principal must use the typed principal contract")

    def response_metadata(schema: dict, seen: frozenset[str] = frozenset()) -> list[dict]:
        result = [schema["properties"]["metadata"]] if "metadata" in schema.get("properties", {}) else []
        ref = schema.get("$ref", "")
        if ref.startswith("#/components/schemas/") and ref not in seen:
            result.extend(response_metadata(schemas.get(ref.rsplit("/", 1)[-1], {}), seen | {ref}))
        for part in schema.get("allOf", []):
            result.extend(response_metadata(part, seen))
        return result

    metadata = response_metadata(schemas.get(run_model, {}))
    if not any(item.get("properties", {}).get("run_manifest", {}).get("$ref") == "#/components/schemas/RunManifest" for item in metadata):
        problems.append(f"{run_model} response metadata must expose the typed RunManifest")
    return problems


def _check_connector_action_contract(spec: dict) -> list[str]:
    """Check metadata response coverage against Go tags, including required fields."""
    root = REPO_ROOT
    model_source = (root / "internal/models/connector_action.go").read_text()
    detail_source = (root / "internal/api/admin_connector_actions.go").read_text()
    schemas = spec.get("components", {}).get("schemas", {})
    problems = []
    for model, schema_name, source in (
        ("ConnectorAction", "ConnectorAction", model_source),
        ("ConnectorActionEvent", "ConnectorActionEvent", model_source),
        ("adminConnectorActionDetail", "AdminConnectorActionDetail", detail_source),
    ):
        match = re.search(r"^type " + model + r" struct \{\n(.*?)^\}", source, re.M | re.S)
        if match is None:
            problems.append(f"cannot find Go model {model}")
            continue
        tags = re.findall(r'`json:"([^"`]+)"', match.group(1))
        wire = {tag.split(",", 1)[0] for tag in tags} - {"-"}
        expected_required = {tag.split(",", 1)[0] for tag in tags if "omitempty" not in tag.split(",")[1:]} - {"-"}
        schema = schemas.get(schema_name, {})
        fields, required = _schema_shape(spec, schema)
        for field in sorted(wire - fields):
            problems.append(f"openapi-admin.json: {schema_name} missing Go JSON field {field}")
        for field in sorted(expected_required - required):
            problems.append(f"openapi-admin.json: {schema_name} must require Go JSON field {field}")
        for field in sorted(required - expected_required):
            problems.append(f"openapi-admin.json: {schema_name} incorrectly requires optional field {field}")
        if schema.get("additionalProperties") is not False:
            problems.append(f"openapi-admin.json: {schema_name} must exclude undeclared payload fields")
        if schema_name != "AdminConnectorActionDetail":
            forbidden = fields & {"args", "arguments", "payload", "request_body", "response_body", "credentials", "access_token", "refresh_token", "authorization"}
            for field in sorted(forbidden):
                problems.append(f"openapi-admin.json: {schema_name} exposes forbidden payload field {field}")
    states = set(re.findall(r'ConnectorAction\w+\s*=\s*"([^"\n]+)"', model_source))
    for name in ("ConnectorAction", "ConnectorActionEvent"):
        documented = set(schemas.get(name, {}).get("properties", {}).get("state", {}).get("enum", []))
        if documented != states:
            problems.append(f"openapi-admin.json: {name} states differ from Go constants")
    if "incarnation_id" not in schemas.get("AdminRunView", {}).get("properties", {}):
        problems.append("openapi-admin.json: AdminRunView missing SQL incarnation_id")
    return problems


def _check_review_contract(spec: dict) -> list[str]:
    """Guard the scoped response projection and credential boundary."""
    schemas = spec.get("components", {}).get("schemas", {})
    problems: list[str] = []
    for model, schema_name, filename in (
        ("reviewPendingAction", "ReviewPendingAction", "internal/api/review.go"),
        ("reviewConnectorActionDetail", "ReviewConnectorActionDetail", "internal/api/review.go"),
        ("reviewSessionStatus", "ReviewSessionStatus", "internal/auth/reviewsession_http.go"),
    ):
        source = (REPO_ROOT / filename).read_text()
        match = re.search(r"^type " + model + r" struct \{\n(.*?)^\}", source, re.M | re.S)
        if not match:
            problems.append(f"cannot find Go review model {model}")
            continue
        tags = re.findall(r'`json:"([^"`]+)"', match.group(1))
        wire = {tag.split(",", 1)[0] for tag in tags} - {"-"}
        expected_required = {tag.split(",", 1)[0] for tag in tags if "omitempty" not in tag.split(",")[1:]} - {"-"}
        schema = schemas.get(schema_name, {})
        fields, required = _schema_shape(spec, schema)
        if fields != wire:
            problems.append(f"{schema_name} fields differ from Go review projection")
        if required != expected_required:
            problems.append(f"{schema_name} required fields differ from Go review projection")
        if schema.get("additionalProperties") is not False:
            problems.append(f"{schema_name} must exclude undeclared fields")
        if fields & {"reason", "extra", "permissions", "credentials", "response_body", "run_available"}:
            problems.append(f"{schema_name} exposes privileged or free-form fields")
    for path, methods in spec.get("paths", {}).items():
        if not path.startswith("/review-api/") or path == "/review-api/session":
            continue
        for method, operation in methods.items():
            if method not in {"get", "post", "put", "delete", "patch", "head"}:
                continue
            security = operation.get("security", spec.get("security", []))
            if not security:
                problems.append(f"{method.upper()} {path} must require scoped review authentication")
            for requirement in security:
                names = set(requirement)
                if not names or not names <= {"ReviewBearerAuth", "ReviewApiKeyAuth", "ReviewSessionAuth", "ReviewCSRF"}:
                    problems.append(f"{method.upper()} {path} permits unscoped authentication")
                if method in {"post", "put", "delete", "patch"} and "ReviewSessionAuth" in names and "ReviewCSRF" not in names:
                    problems.append(f"{method.upper()} {path} cookie mutation is missing CSRF")
    session = schemas.get("ReviewSessionStatus", {}).get("properties", {})
    if session.get("auth_required", {}).get("const") is not True:
        problems.append("ReviewSessionStatus must never document default-open review")
    if spec.get("components", {}).get("securitySchemes", {}).get("ReviewSessionAuth", {}).get("name") != "runkite_review_session":
        problems.append("Review must use its separate cookie name")
    return problems



def _check_security_completion_contract(spec: dict) -> list[str]:
    """Keep new credential/export responses metadata-only and source aligned."""
    schemas = spec.get("components", {}).get("schemas", {})
    problems = []
    contracts = (
        ("PendingAction", "PendingAction", "internal/models/pending.go"),
        ("PendingActionApproval", "PendingActionApproval", "internal/models/pending.go"),
        ("Owner", "CredentialLeaseOwner", "internal/credentials/types.go"),
        ("Record", "CredentialLease", "internal/credentials/types.go"),
        ("ActionDelivery", "ActionDelivery", "internal/state/action_delivery.go"),
        ("ActionDeliveryPayload", "ActionDeliveryPayload", "internal/state/action_delivery.go"),
    )
    for model, name, filename in contracts:
        source = (REPO_ROOT / filename).read_text()
        match = re.search(r"^type " + model + r" struct \{\n(.*?)^\}", source, re.M | re.S)
        if not match:
            problems.append(f"cannot find security model {model}")
            continue
        tags = re.findall(r'`json:"([^"`]+)"', match.group(1))
        wire = {tag.split(",", 1)[0] for tag in tags} - {"-"}
        expected_required = {tag.split(",", 1)[0] for tag in tags if "omitempty" not in tag.split(",")[1:]} - {"-"}
        schema = schemas.get(name, {})
        fields, required = _schema_shape(spec, schema)
        if fields != wire:
            problems.append(f"{name} fields differ from Go security model")
        if required != expected_required:
            problems.append(f"{name} required fields differ from Go security model")
        if schema.get("additionalProperties") is not False:
            problems.append(f"{name} must exclude undeclared security fields")
        forbidden = fields & {"protected_handle", "claim_token", "password", "username", "handle", "token", "credentials", "provider_error", "authorization"}
        for field in sorted(forbidden):
            problems.append(f"{name} exposes forbidden secret/authority field {field}")
    expected_refs = {("ActionDelivery", "payload"): "ActionDeliveryPayload", ("ActionDeliveryPayload", "action"): "ConnectorAction", ("ActionDeliveryPayload", "event"): "ConnectorActionEvent", ("CredentialLease", "owner"): "CredentialLeaseOwner"}
    for (name, field), target in expected_refs.items():
        if schemas.get(name, {}).get("properties", {}).get(field) != {"$ref": f"#/components/schemas/{target}"}:
            problems.append(f"{name}.{field} must use bounded typed {target} metadata")
    for name in ("ConnectorAction", "PendingAction", "ReviewPendingAction"):
        props = schemas.get(name, {}).get("properties", {})
        if props.get("target_revision", {}).get("pattern") != "^target-v[12]-[0-9a-f]{64}$":
            problems.append(f"{name} target revision must preserve v1 and accept only supported v2")
        if props.get("policy_revision", {}).get("pattern") != "^policy-v1-[0-9a-f]{64}$":
            problems.append(f"{name} policy revision must bind the configured v1 policy")
        if props.get("tool_revision", {}).get("pattern") != "^(?:|tool-v1-[0-9a-f]{64})$":
            problems.append(f"{name} tool revision must be optional exact v1 schema identity")
    for name in ("PendingAction", "ReviewPendingAction"):
        props = schemas.get(name, {}).get("properties", {})
        quorum = props.get("required_approvals", {})
        if quorum.get("minimum") != 1 or quorum.get("maximum") != 5:
            problems.append(f"{name} quorum must remain bounded one through five")
    lease_source = (REPO_ROOT / "internal/credentials/types.go").read_text()
    states = set(re.findall(r'State\w+\s+State\s*=\s*"([^"\n]+)"', lease_source))
    if set(schemas.get("CredentialLease", {}).get("properties", {}).get("state", {}).get("enum", [])) != states:
        problems.append("CredentialLease lifecycle states differ from Go constants")
    delivery_states = {"pending", "delivered", "dead_letter"}
    if set(schemas.get("ActionDelivery", {}).get("properties", {}).get("status", {}).get("enum", [])) != delivery_states:
        problems.append("ActionDelivery states differ from Go constants")
    for path, methods in spec.get("paths", {}).items():
        is_admin = path.startswith(("/admin-api/action-deliveries", "/admin-api/credential-leases"))
        is_review_lease = path.startswith("/review-api/credential-leases")
        for method, operation in methods.items():
            if method not in {"get", "post", "put", "delete", "patch"}:
                continue
            if is_review_lease and method != "get":
                problems.append(f"{method.upper()} {path} must not grant Review lifecycle mutation")
            if not is_admin:
                continue
            if not any(p.get("in") == "query" and p.get("name") == "tenant_id" and p.get("required") is True for p in operation.get("parameters", [])):
                problems.append(f"{method.upper()} {path} requires an explicit tenant parameter")
            for requirement in operation.get("security", []):
                if method != "get" and "AdminSessionAuth" in requirement and "AdminCSRF" not in requirement:
                    problems.append(f"{method.upper()} {path} Admin cookie mutation missing CSRF")
    query = spec.get("paths", {}).get("/admin-api/action-deliveries", {}).get("get", {}).get("parameters", [])
    bounds = {p.get("name"): p.get("schema", {}) for p in query}
    if bounds.get("limit", {}).get("maximum") != 100 or bounds.get("offset", {}).get("maximum") != 10000:
        problems.append("Action delivery pagination must remain bounded")
    return problems

def main() -> None:
    public = _load_spec("openapi.json")
    admin = _load_spec("openapi-admin.json")
    internal = _load_spec("openapi-internal.json")

    pub_routes = _spec_methods_paths(public)
    admin_routes = _spec_methods_paths(admin)
    internal_routes = _spec_methods_paths(internal)
    all_spec_routes = pub_routes | admin_routes | internal_routes

    go_routes = extract_routes()
    missing: list[str] = []

    for route in go_routes:
        parts = route.split(" ", 1)
        method, path = parts[0], parts[1]

        if method == "*":
            # Wildcard (Handle without method prefix): check that at least
            # one method for this path exists in any spec. The catch-all
            # "/" handler is the mux fallback, not a real route to document.
            if path == "/":
                continue
            found = any(
                r.endswith(f" {path}") for r in all_spec_routes
            )
            if not found:
                missing.append(route)
            continue

        key = f"{method} {path}"
        if key not in all_spec_routes:
            missing.append(route)

    integrity: list[str] = []
    for name, spec in (
        ("openapi.json", public),
        ("openapi-admin.json", admin),
        ("openapi-internal.json", internal),
    ):
        integrity.extend(_check_spec_integrity(name, spec))
    integrity.extend(_check_model_contract(public))
    integrity.extend(_check_run_manifest_contract(public))
    integrity.extend(_check_run_manifest_contract(admin, "AdminRunView"))
    integrity.extend(_check_connector_action_contract(admin))
    integrity.extend(_check_review_contract(admin))
    integrity.extend(_check_security_completion_contract(admin))

    if missing or integrity:
        if missing:
            print(f"FAIL: {len(missing)} Go route(s) not found in any OpenAPI spec:\n", file=sys.stderr)
            for r in missing:
                print(f"  {r}", file=sys.stderr)
        if integrity:
            print(f"FAIL: {len(integrity)} OpenAPI integrity issue(s):\n", file=sys.stderr)
            for p in integrity:
                print(f"  {p}", file=sys.stderr)
        print(f"\nPublic spec ops : {len(pub_routes)}", file=sys.stderr)
        print(f"Admin spec ops  : {len(admin_routes)}", file=sys.stderr)
        print(f"Internal spec ops: {len(internal_routes)}", file=sys.stderr)
        sys.exit(1)
    else:
        print(f"OK: all {len(go_routes)} Go routes covered; specs integrity clean")
        print(f"  Public spec   : {len(pub_routes)} ops")
        print(f"  Admin spec    : {len(admin_routes)} ops")
        print(f"  Internal spec : {len(internal_routes)} ops")


if __name__ == "__main__":
    main()
