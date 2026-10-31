"""Exact assignment identity must survive headers and explicit connector config."""

import base64
import unittest

import httpx
from runkite_runner.connectors import _run_bound_headers
from runkite_runner.tenant_ctx import (
    HEADER_GENERATION,
    HEADER_RUN_ID,
    HEADER_RUN_ID_B64,
    HEADER_TENANT_ID,
    HEADER_TENANT_ID_B64,
    bind_run,
    bind_tenant,
    checkpoint_thread_id,
    current_tenant,
    reset_run,
    reset_tenant,
    run_id_headers,
    tenant_headers,
    validate_native_checkpoint_identity,
)


class RunIdentityTests(unittest.TestCase):
    def test_nonempty_tenant_is_not_normalized(self):
        token = bind_tenant(" acme ")
        try:
            self.assertEqual(current_tenant(), " acme ")
            headers = tenant_headers()
            self.assertNotIn(HEADER_TENANT_ID, headers)
            encoded = headers[HEADER_TENANT_ID_B64]
            self.assertEqual(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)).decode(), " acme ")
            self.assertEqual(checkpoint_thread_id(" acme ", "thread"), " acme :thread")
        finally:
            reset_tenant(token)

    def test_exact_run_id_is_encoded_when_raw_header_would_change_it(self):
        for run_id in ("run ", " run", "run\tpart", "run\npart", "run-ré-雪", "\u00a0run"):
            with self.subTest(run_id=repr(run_id)):
                token = bind_run(run_id, 4)
                try:
                    headers = tenant_headers()
                    self.assertNotIn(HEADER_RUN_ID, headers)
                    encoded = headers[HEADER_RUN_ID_B64]
                    self.assertNotIn("=", encoded)
                    self.assertEqual(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)).decode(), run_id)
                    self.assertEqual(headers[HEADER_GENERATION], "4")
                    httpx.Request("GET", "http://plane/", headers=headers)
                finally:
                    reset_run(token)
        self.assertEqual(run_id_headers("run /?#%"), {HEADER_RUN_ID: "run /?#%"})

    def test_connector_config_replaces_both_header_forms(self):
        for bound, explicit in (("old", "ré 雪"), ("old ", "new")):
            with self.subTest(bound=bound):
                token = bind_run(bound, 1)
                try:
                    headers = _run_bound_headers({"run_id": explicit, "generation": 3})
                    expected = run_id_headers(explicit)
                    self.assertEqual(
                        {key: value for key, value in headers.items() if key in (HEADER_RUN_ID, HEADER_RUN_ID_B64)},
                        expected,
                    )
                    self.assertEqual(headers[HEADER_GENERATION], "3")
                finally:
                    reset_run(token)

    def test_unicode_tenant_header_is_exact_ascii_transport(self):
        for tenant in ("tenant-ré-雪", "acme\twest"):
            token = bind_tenant(tenant)
            try:
                headers = tenant_headers()
                self.assertNotIn(HEADER_TENANT_ID, headers)
                encoded = headers[HEADER_TENANT_ID_B64]
                self.assertEqual(base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4)).decode(), tenant)
                httpx.Request("GET", "http://plane/", headers=headers)
            finally:
                reset_tenant(token)

    def test_native_delimiter_rule_keeps_safe_keys_and_blocks_aliases(self):
        for tenant, thread in ((None, "t1"), ("default", "t1"), ("acme", "west:t1")):
            validate_native_checkpoint_identity(tenant, thread, mode="memory")
        for tenant, thread in (
            ("default", "acme:t1"),
            ("default", "cron:daily"),
            ("acme:west", "t1"),
            (" acme", "t1"),
            ("acme\twest", "t1"),
        ):
            with self.subTest(tenant=tenant, thread=thread), self.assertRaisesRegex(ValueError, "ambiguous native"):
                validate_native_checkpoint_identity(tenant, thread, mode="memory")

    def test_persistent_native_state_is_default_tenant_only(self):
        validate_native_checkpoint_identity(None, "existing", mode="direct-postgres")
        validate_native_checkpoint_identity("default", "existing", mode="direct-postgres")
        for tenant in ("acme", "acme:west", "tenant-雪"):
            with self.subTest(tenant=tenant), self.assertRaisesRegex(ValueError, "ownership"):
                validate_native_checkpoint_identity(tenant, "target", mode="direct-postgres")


if __name__ == "__main__":
    unittest.main()
