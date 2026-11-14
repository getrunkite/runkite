"""Regression coverage for field drift hidden by complete route coverage."""

import copy
import json
import unittest
from pathlib import Path

from check import _check_connector_action_contract, _check_model_contract, _check_review_contract, _check_run_manifest_contract, _check_security_completion_contract


class RunContractTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi.json").read_text())

    def test_current_contract_covers_go_models(self):
        self.assertEqual(_check_model_contract(self.spec), [])

    def test_missing_resume_capability_is_detected(self):
        del self.spec["components"]["schemas"]["RunCreate"]["properties"]["command"]
        self.assertTrue(any("RunCreate missing Go JSON field command" in p for p in _check_model_contract(self.spec)))

    def test_missing_output_is_detected(self):
        parts = self.spec["components"]["schemas"]["Run"]["allOf"]
        for part in parts:
            part.get("properties", {}).pop("output", None)
        self.assertTrue(any("Run missing Go JSON field output" in p for p in _check_model_contract(self.spec)))

    def test_stream_request_cannot_require_run_response_fields(self):
        schema = self.spec["components"]["schemas"]["RunStream"]["allOf"][1]
        schema["required"] = ["created_at", "status"]
        self.assertTrue(any("requires server-assigned field status" in p for p in _check_model_contract(self.spec)))

    def test_endpoint_drift_is_detected_independently(self):
        op = self.spec["paths"]["/runs/wait"]["post"]
        body = op["requestBody"]["content"]["application/json"]
        body["schema"] = copy.deepcopy(self.spec["components"]["schemas"]["RunCreate"])
        del body["schema"]["properties"]["checkpoint_ref"]
        self.assertTrue(
            any("POST /runs/wait request missing checkpoint_ref" in p for p in _check_model_contract(self.spec))
        )


class ConnectorActionContractTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi-admin.json").read_text())

    def test_current_metadata_contract_covers_go_models(self):
        self.assertEqual(_check_connector_action_contract(self.spec), [])

    def test_missing_immutable_run_identity_is_detected(self):
        del self.spec["components"]["schemas"]["ConnectorAction"]["properties"]["run_incarnation_id"]
        self.assertTrue(any("missing Go JSON field run_incarnation_id" in problem for problem in _check_connector_action_contract(self.spec)))

    def test_missing_required_evidence_fields_are_detected(self):
        self.spec["components"]["schemas"]["AdminConnectorActionDetail"]["required"].remove("run_available")
        self.assertTrue(any("must require Go JSON field run_available" in problem for problem in _check_connector_action_contract(self.spec)))

    def test_payload_bags_cannot_silently_expand_the_metadata_contract(self):
        action = self.spec["components"]["schemas"]["ConnectorAction"]
        action["additionalProperties"] = True
        action["properties"]["credentials"] = {"type": "object"}
        problems = _check_connector_action_contract(self.spec)
        self.assertTrue(any("must exclude undeclared payload fields" in problem for problem in problems))
        self.assertTrue(any("forbidden payload field credentials" in problem for problem in problems))

    def test_new_go_states_must_be_covered(self):
        self.spec["components"]["schemas"]["ConnectorActionEvent"]["properties"]["state"]["enum"].remove("outcome_unknown")
        self.assertTrue(any("states differ from Go constants" in problem for problem in _check_connector_action_contract(self.spec)))


class RunManifestContractTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi.json").read_text())

    def test_actual_response_metadata_matches_both_server_models(self):
        self.assertEqual(_check_run_manifest_contract(self.spec), [])
        admin = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi-admin.json").read_text())
        self.assertEqual(_check_run_manifest_contract(admin, "AdminRunView"), [])

    def test_missing_or_untyped_verified_identity_is_detected(self):
        principal = self.spec["components"]["schemas"]["RunManifestPrincipal"]
        del principal["properties"]["verified_identity"]
        problems = _check_run_manifest_contract(self.spec)
        self.assertTrue(any("RunManifestPrincipal fields differ" in problem for problem in problems))
        self.assertTrue(any("verified_identity" in problem for problem in problems))

    def test_unused_manifest_schema_does_not_hide_untyped_response_metadata(self):
        for part in self.spec["components"]["schemas"]["Run"]["allOf"]:
            if "metadata" in part.get("properties", {}):
                part["properties"]["metadata"] = {"type": "object"}
        self.assertTrue(any("response metadata" in problem for problem in _check_run_manifest_contract(self.spec)))


class ReviewContractTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi-admin.json").read_text())

    def test_scoped_wire_contract_matches_go(self):
        self.assertEqual(_check_review_contract(self.spec), [])

    def test_missing_scope_or_projection_guard_is_detected(self):
        schema = self.spec["components"]["schemas"]["ReviewPendingAction"]
        del schema["properties"]["tenant_id"]
        schema["additionalProperties"] = True
        problems = _check_review_contract(self.spec)
        self.assertTrue(any("fields differ" in p for p in problems))
        self.assertTrue(any("exclude undeclared" in p for p in problems))

    def test_admin_auth_cannot_be_documented_as_review_auth(self):
        self.spec["paths"]["/review-api/pending-actions"]["get"]["security"] = [{"BearerAuth": []}]
        self.assertTrue(any("unscoped authentication" in p for p in _check_review_contract(self.spec)))

    def test_review_cookie_mutation_requires_csrf(self):
        op = self.spec["paths"]["/review-api/pending-actions/{id}/approve"]["post"]
        op["security"] = [{"ReviewSessionAuth": []}]
        self.assertTrue(any("missing CSRF" in p for p in _check_review_contract(self.spec)))

    def test_no_default_open_or_admin_cookie_alias(self):
        self.spec["components"]["schemas"]["ReviewSessionStatus"]["properties"]["auth_required"]["const"] = False
        self.spec["components"]["securitySchemes"]["ReviewSessionAuth"]["name"] = "runkite_admin_session"
        problems = _check_review_contract(self.spec)
        self.assertTrue(any("default-open" in p for p in problems))
        self.assertTrue(any("separate cookie" in p for p in problems))


class SecurityCompletionContractTests(unittest.TestCase):
    def setUp(self):
        self.spec = json.loads((Path(__file__).resolve().parents[2] / "spec/openapi-admin.json").read_text())

    def test_current_security_contract_matches_source(self):
        self.assertEqual(_check_security_completion_contract(self.spec), [])

    def test_missing_quorum_and_review_history_are_detected(self):
        del self.spec["components"]["schemas"]["PendingAction"]["properties"]["approvals"]
        self.spec["components"]["schemas"]["ReviewPendingAction"]["properties"]["required_approvals"]["maximum"] = 999
        problems = _check_security_completion_contract(self.spec)
        self.assertTrue(any("fields differ" in p for p in problems))
        self.assertTrue(any("quorum must remain bounded" in p for p in problems))

    def test_cleanup_and_delivery_authority_cannot_enter_public_metadata(self):
        for name, field in (("CredentialLease", "protected_handle"), ("ActionDelivery", "claim_token")):
            self.spec["components"]["schemas"][name]["properties"][field] = {"type": "string"}
        problems = _check_security_completion_contract(self.spec)
        self.assertTrue(any("forbidden secret/authority field protected_handle" in p for p in problems))
        self.assertTrue(any("forbidden secret/authority field claim_token" in p for p in problems))

    def test_export_payload_cannot_become_an_untyped_bag(self):
        self.spec["components"]["schemas"]["ActionDelivery"]["properties"]["payload"] = {"type": "object", "additionalProperties": True}
        self.assertTrue(any("bounded typed" in p for p in _check_security_completion_contract(self.spec)))

    def test_future_or_missing_revisions_cannot_be_documented_as_supported(self):
        self.spec["components"]["schemas"]["PendingAction"]["properties"]["target_revision"]["pattern"] = "^target-v[123]-[0-9a-f]{64}$"
        del self.spec["components"]["schemas"]["ConnectorAction"]["properties"]["policy_revision"]
        problems = _check_security_completion_contract(self.spec)
        self.assertTrue(any("target revision" in p for p in problems))
        self.assertTrue(any("policy revision" in p for p in problems))

    def test_review_cannot_gain_lease_mutation_routes(self):
        self.spec["paths"]["/review-api/credential-leases/{id}"]["post"] = {"responses": {}}
        self.assertTrue(any("must not grant Review" in p for p in _check_security_completion_contract(self.spec)))

    def test_explicit_tenant_and_admin_cookie_csrf_are_required(self):
        op = self.spec["paths"]["/admin-api/action-deliveries/{id}/retry"]["post"]
        op["parameters"] = [p for p in op["parameters"] if p["name"] != "tenant_id"]
        op["security"] = [{"AdminSessionAuth": []}]
        problems = _check_security_completion_contract(self.spec)
        self.assertTrue(any("explicit tenant" in p for p in problems))
        self.assertTrue(any("missing CSRF" in p for p in problems))

    def test_unknown_lifecycle_states_and_unbounded_delivery_pages_are_detected(self):
        self.spec["components"]["schemas"]["CredentialLease"]["properties"]["state"]["enum"].remove("issuance_unknown")
        for param in self.spec["paths"]["/admin-api/action-deliveries"]["get"]["parameters"]:
            if param["name"] == "limit":
                param["schema"]["maximum"] = 10000
        problems = _check_security_completion_contract(self.spec)
        self.assertTrue(any("states differ" in p for p in problems))
        self.assertTrue(any("pagination must remain bounded" in p for p in problems))


if __name__ == "__main__":
    unittest.main()
