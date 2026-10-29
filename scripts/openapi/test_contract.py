"""Regression coverage for field drift hidden by complete route coverage."""

import copy
import json
import unittest
from pathlib import Path

from check import _check_model_contract


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


if __name__ == "__main__":
    unittest.main()
