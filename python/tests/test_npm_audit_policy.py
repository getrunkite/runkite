"""Adversarial contracts for the conditional Admin dependency audit decision."""

from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
POLICY_PATH = ROOT / "scripts/admin-audit-exception.json"
spec = importlib.util.spec_from_file_location("npm_audit_policy_under_test", ROOT / "scripts/npm_audit_policy.py")
audit = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = audit
spec.loader.exec_module(audit)

ADVISORY = "GHSA-fc5q-39xc-vjpf"
MANIFEST_HASH = "589b51689b7fd8d0b7be89dfa6775cdf3eba53ee9f23fad771c49fd336951e84"
LOCK_HASH = "d07e1958b224e724ec30dd498e2c02468976bb4949e1268e3bc64d84b65b3788"
NOW = datetime(2026, 11, 18, 12, tzinfo=timezone.utc)
APPROVED_AT = datetime(2026, 11, 18, 6, 20, 46, tzinfo=timezone.utc)


def stamp(value):
    return value.astimezone(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def counts(**values):
    result = dict.fromkeys(("info", "low", "moderate", "high", "critical"), 0)
    result.update(values)
    result["total"] = sum(result.values())
    return result


def known_report():
    # This is the real five-node npm v2 propagation shape, with synthetic-only
    # metadata. The leaf and parent edges are intentionally distinct so a
    # second advisory cannot hide behind a familiar propagated package name.
    graph = {
        "@vitejs/plugin-react": (["vite"], [], True, ">=6.0.0-beta.0"),
        "fdir": (["picomatch"], [], False, ">=5.2.1"),
        "picomatch": (
            [
                {
                    "source": 1247710,
                    "name": "picomatch",
                    "dependency": "picomatch",
                    "title": "picomatch vulnerable to ReDoS through single-branch repeated extglobs",
                    "url": "https://github.com/advisories/" + ADVISORY,
                    "severity": "high",
                    "cwe": ["CWE-1333"],
                    "cvss": {"score": 7.5, "vectorString": "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:H"},
                    "range": "<=4.0.7",
                }
            ],
            ["fdir", "tinyglobby", "vite"],
            False,
            "*",
        ),
        "tinyglobby": (["fdir", "picomatch"], [], False, "*"),
        "vite": (["picomatch", "tinyglobby"], ["@vitejs/plugin-react"], True, ">=6.3.0-beta.0"),
    }
    return {
        "auditReportVersion": 2,
        "vulnerabilities": {
            name: {
                "name": name,
                "severity": "high",
                "isDirect": direct,
                "via": via,
                "effects": effects,
                "range": affected,
                "nodes": ["node_modules/" + name],
                "fixAvailable": (
                    {"name": "@vitejs/plugin-react", "version": "5.2.0", "isSemVerMajor": True}
                    if name == "@vitejs/plugin-react"
                    else {"name": "vite", "version": "6.2.7", "isSemVerMajor": True}
                    if name in {"picomatch", "vite"}
                    else True
                ),
            }
            for name, (via, effects, direct, affected) in graph.items()
        },
        "metadata": {
            "vulnerabilities": counts(high=5),
            "dependencies": {"prod": 106, "dev": 107, "optional": 74, "peer": 1, "peerOptional": 0, "total": 213},
        },
    }


def clean_report():
    return {
        "auditReportVersion": 2,
        "vulnerabilities": {},
        "metadata": {
            "vulnerabilities": counts(),
            "dependencies": {"prod": 106, "dev": 107, "optional": 74, "peer": 1, "peerOptional": 0, "total": 213},
        },
    }


def active_policy(now=NOW):
    approval = APPROVED_AT
    return {
        "schema_version": 1,
        "status": "active",
        "owner": "Sharan Harsoor",
        "approval_recorded_at": stamp(approval),
        "expires_at": "2026-11-25T00:00:00Z",
        "condition": "Activate only after the full hosted CI test suite passes and the maintainer records verified qualification.",
        "advisory": ADVISORY,
        "manifest_sha256": MANIFEST_HASH,
        "lockfile_sha256": LOCK_HASH,
        "qualification": {
            "repository": "getrunkite/runkite",
            "workflow": ".github/workflows/ci.yml",
            "job": "test-suite",
            "conclusion": "success",
            "run_id": 123456789,
            "run_attempt": 1,
            "head_sha": "a" * 40,
            "run_url": "https://github.com/getrunkite/runkite/actions/runs/123456789",
            "completed_at": stamp(max(approval, now - timedelta(minutes=30))),
            "verified_by": "Sharan Harsoor",
            "manifest_sha256": MANIFEST_HASH,
            "lockfile_sha256": LOCK_HASH,
        },
    }


def pending_policy():
    policy = active_policy()
    policy.update(status="pending-full-ci", qualification=None)
    return policy


class PolicyFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="runkite-npm-policy-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.repo = self.directory / "owned repo"
        self.admin = self.repo / "admin-ui"
        self.admin.mkdir(parents=True)
        for name in ("package.json", "package-lock.json"):
            shutil.copy2(ROOT / "admin-ui" / name, self.admin / name)
        self.report = known_report()
        self.policy = active_policy()

    def decision(self, report=None, *, cwd=None, policy=None, now=NOW):
        return audit.evaluate(
            self.report if report is None else report,
            self.admin if cwd is None else cwd,
            self.repo,
            self.policy if policy is None else policy,
            now,
        )

    def assert_blocked(self, decision):
        self.assertNotEqual(decision.exit_code, 0, decision.message)
        self.assertFalse(decision.accepted_risk, decision.message)


class ConditionalAdminPolicyTests(PolicyFixture):
    def test_exact_qualified_fixture_is_accepted_and_explicitly_marked_as_risk(self):
        result = self.decision()
        self.assertEqual(result.exit_code, 0, result.message)
        self.assertTrue(result.accepted_risk)
        self.assertIn(ADVISORY, result.message)

    def test_repository_configuration_is_pending_or_has_complete_qualified_activation(self):
        # Validate both legitimate record states without changing the repository.
        # Online GitHub verification remains the maintainer's activation step;
        # these offline tests check the recorded proof and decision contract.
        records = (
            ("checked-in", json.loads(POLICY_PATH.read_text())),
            ("pending-fixture", pending_policy()),
            ("active-fixture", active_policy()),
        )
        expected = pending_policy()
        for name, policy in records:
            with self.subTest(record=name):
                self.assertEqual(set(policy), set(expected))
                for field in expected.keys() - {"status", "qualification"}:
                    self.assertEqual(policy[field], expected[field], field)
                self.assertIn(policy["status"], {"pending-full-ci", "active"})
                if policy["status"] == "pending-full-ci":
                    self.assertIsNone(policy["qualification"])
                    self.assert_blocked(self.decision(policy=policy))
                else:
                    proof = policy["qualification"]
                    self.assertIsInstance(proof, dict)
                    completed = proof["completed_at"]
                    self.assertIsInstance(completed, str)
                    self.assertTrue(completed.endswith("Z"))
                    qualified_at = datetime.fromisoformat(completed[:-1] + "+00:00")
                    result = self.decision(policy=policy, now=qualified_at)
                    self.assertEqual(result.exit_code, 0, result.message)
                    self.assertTrue(result.accepted_risk)

    def test_pending_or_missing_qualification_never_activates_from_other_valid_fields(self):
        for status, qualification in (
            ("pending-full-ci", self.policy["qualification"]),
            ("pending-full-ci", None),
            ("active", None),
            ("approved", self.policy["qualification"]),
            ("active", {}),
        ):
            with self.subTest(status=status, qualification=qualification):
                policy = copy.deepcopy(self.policy)
                policy.update(status=status, qualification=qualification)
                self.assert_blocked(self.decision(policy=policy))

    def test_policy_identity_and_exact_approved_fields_cannot_be_extended(self):
        for key, value in (
            ("schema_version", True),
            ("owner", "another owner"),
            ("advisory", "GHSA-aaaa-bbbb-cccc"),
            ("condition", ""),
            ("manifest_sha256", "b" * 64),
            ("lockfile_sha256", "b" * 64),
            ("other_roots", ["examples/promptfoo"]),
        ):
            with self.subTest(key=key):
                policy = copy.deepcopy(self.policy)
                policy[key] = value
                self.assert_blocked(self.decision(policy=policy))

    def test_scope_is_exact_admin_directory_not_repository_parent_other_root_or_child(self):
        for relative in (".", "typescript/runkite-runner", "examples/promptfoo", "admin-ui/nested", "other/admin-ui"):
            with self.subTest(relative=relative):
                directory = self.repo / relative
                directory.mkdir(parents=True, exist_ok=True)
                self.assert_blocked(self.decision(cwd=directory))

    def test_other_repository_admin_directory_cannot_borrow_approved_files(self):
        other = self.directory / "different repo/admin-ui"
        other.mkdir(parents=True)
        for name in ("package.json", "package-lock.json"):
            shutil.copy2(self.admin / name, other / name)
        self.assert_blocked(self.decision(cwd=other))

    def test_expired_future_approval_and_windows_over_seven_days_are_refused(self):
        changes = (
            {"expires_at": stamp(NOW)},
            {"expires_at": stamp(NOW - timedelta(seconds=1))},
            {"approval_recorded_at": stamp(NOW + timedelta(seconds=1))},
            {"expires_at": stamp(NOW + timedelta(days=8))},
            {"approval_recorded_at": "invalid"},
            {"expires_at": "2026-11-19T12:00:00"},
        )
        for change in changes:
            with self.subTest(change=change):
                self.assert_blocked(self.decision(policy={**self.policy, **change}))

    def test_missing_naive_or_invalid_clock_cannot_enable_exception(self):
        for value in (None, NOW.replace(tzinfo=None), "2026-11-18T12:00:00Z", float("nan")):
            with self.subTest(value=value):
                self.assert_blocked(self.decision(now=value))

    def test_qualification_must_identify_successful_full_hosted_suite_and_exact_files(self):
        mutations = (
            ("repository", "other/runkite"),
            ("workflow", ".github/workflows/other.yml"),
            ("job", "test"),
            ("conclusion", "skipped"),
            ("conclusion", "failure"),
            ("conclusion", "cancelled"),
            ("run_id", 0),
            ("run_id", True),
            ("run_attempt", 0),
            ("head_sha", "a" * 7),
            ("run_url", "https://github.com/other/repo/actions/runs/123456789"),
            ("verified_by", "unverified"),
            ("manifest_sha256", "b" * 64),
            ("lockfile_sha256", "b" * 64),
            ("completed_at", stamp(NOW + timedelta(seconds=1))),
            ("completed_at", stamp(APPROVED_AT - timedelta(seconds=1))),
        )
        for key, value in mutations:
            with self.subTest(key=key, value=value):
                policy = copy.deepcopy(self.policy)
                policy["qualification"][key] = value
                self.assert_blocked(self.decision(policy=policy))
        for key in self.policy["qualification"]:
            with self.subTest(missing=key):
                policy = copy.deepcopy(self.policy)
                del policy["qualification"][key]
                self.assert_blocked(self.decision(policy=policy))

    def test_new_advisory_hidden_in_familiar_parent_is_not_accepted(self):
        other = copy.deepcopy(self.report["vulnerabilities"]["picomatch"]["via"][0])
        other["url"] = "https://github.com/advisories/GHSA-aaaa-bbbb-cccc"
        other["source"] += 1
        self.report["vulnerabilities"]["vite"]["via"].append(other)
        self.assert_blocked(self.decision())

    def test_same_advisory_with_changed_leaf_identity_or_severity_is_refused(self):
        for key, value in (
            ("url", "https://untrusted.invalid/" + ADVISORY),
            ("name", "another-matcher"),
            ("dependency", "another-matcher"),
            ("severity", "critical"),
            ("severity", "low"),
        ):
            with self.subTest(key=key, value=value):
                report = known_report()
                report["vulnerabilities"]["picomatch"]["via"][0][key] = value
                self.assert_blocked(self.decision(report))

    def test_unknown_root_or_installed_node_cannot_share_the_exception(self):
        for mutation in ("extra-root", "extra-node", "changed-node", "unknown-parent", "cycle"):
            with self.subTest(mutation=mutation):
                report = known_report()
                if mutation == "extra-root":
                    report["vulnerabilities"]["other"] = {
                        **copy.deepcopy(report["vulnerabilities"]["fdir"]),
                        "name": "other",
                        "nodes": ["node_modules/other"],
                    }
                    report["metadata"]["vulnerabilities"] = counts(high=6)
                elif mutation == "extra-node":
                    report["vulnerabilities"]["picomatch"]["nodes"].append("node_modules/other/node_modules/picomatch")
                elif mutation == "changed-node":
                    report["vulnerabilities"]["picomatch"]["nodes"] = ["node_modules/other/node_modules/picomatch"]
                elif mutation == "unknown-parent":
                    report["vulnerabilities"]["fdir"]["via"] = ["missing-package"]
                else:
                    report["vulnerabilities"]["fdir"]["via"] = ["vite"]
                    report["vulnerabilities"]["vite"]["via"] = ["fdir"]
                self.assert_blocked(self.decision(report))

    def test_inconsistent_counts_and_report_shapes_cannot_hide_findings(self):
        for mutation in (
            "count-low",
            "count-high",
            "count-total",
            "missing-metadata",
            "string-count",
            "bool-count",
            "list-vulnerabilities",
            "wrong-version",
            "missing-via",
            "empty-via",
            "node-severity",
        ):
            with self.subTest(mutation=mutation):
                report = known_report()
                if mutation == "count-low":
                    report["metadata"]["vulnerabilities"] = counts(low=5)
                elif mutation == "count-high":
                    report["metadata"]["vulnerabilities"] = counts(high=4)
                elif mutation == "count-total":
                    report["metadata"]["vulnerabilities"]["total"] = 4
                elif mutation == "missing-metadata":
                    del report["metadata"]
                elif mutation == "string-count":
                    report["metadata"]["vulnerabilities"]["high"] = "5"
                elif mutation == "bool-count":
                    report["metadata"]["vulnerabilities"]["high"] = True
                elif mutation == "list-vulnerabilities":
                    report["vulnerabilities"] = list(report["vulnerabilities"].values())
                elif mutation == "wrong-version":
                    report["auditReportVersion"] = 1
                elif mutation == "missing-via":
                    del report["vulnerabilities"]["fdir"]["via"]
                elif mutation == "empty-via":
                    report["vulnerabilities"]["fdir"]["via"] = []
                else:
                    report["vulnerabilities"]["vite"]["severity"] = "low"
                self.assert_blocked(self.decision(report))

    def test_manifest_and_lock_bytes_are_bound_even_for_harmless_changes(self):
        for name in ("package.json", "package-lock.json"):
            with self.subTest(name=name):
                file = self.admin / name
                original = file.read_bytes()
                file.write_bytes(original + b"\n")
                self.assert_blocked(self.decision())
                file.write_bytes(original)

    def test_production_dependency_or_rehashed_scope_change_cannot_be_approved_by_hash_only(self):
        file = self.admin / "package-lock.json"
        lock = json.loads(file.read_text())
        lock["packages"]["node_modules/picomatch"]["dev"] = False
        file.write_text(json.dumps(lock))
        policy = copy.deepcopy(self.policy)
        policy["lockfile_sha256"] = hashlib.sha256(file.read_bytes()).hexdigest()
        policy["qualification"]["lockfile_sha256"] = policy["lockfile_sha256"]
        self.assert_blocked(self.decision(policy=policy))

    def test_clean_admin_report_does_not_need_a_risk_exception(self):
        result = self.decision(clean_report(), policy=pending_policy())
        self.assertEqual(result.exit_code, 0, result.message)
        self.assertFalse(result.accepted_risk)

    def test_lower_parent_severity_cannot_hide_high_leaf_when_metadata_claims_no_highs(self):
        report = known_report()
        for entry in report["vulnerabilities"].values():
            entry["severity"] = "low"
        report["metadata"]["vulnerabilities"] = counts(low=5)
        self.assert_blocked(self.decision(report, policy=pending_policy()))

    def test_admin_lower_severity_findings_remain_outside_the_exact_exception(self):
        report = known_report()
        for entry in report["vulnerabilities"].values():
            entry["severity"] = "low"
        report["vulnerabilities"]["picomatch"]["via"][0]["severity"] = "low"
        report["metadata"]["vulnerabilities"] = counts(low=5)
        self.assert_blocked(self.decision(report))

    def test_lower_severity_does_not_make_missing_node_fields_a_clean_admin_report(self):
        for field in ("via", "nodes", "isDirect", "effects", "range", "fixAvailable"):
            with self.subTest(field=field):
                report = known_report()
                for entry in report["vulnerabilities"].values():
                    entry["severity"] = "moderate"
                report["metadata"]["vulnerabilities"] = counts(moderate=5)
                del report["vulnerabilities"]["picomatch"][field]
                self.assert_blocked(self.decision(report, policy=pending_policy()))

    def test_legacy_other_roots_still_block_high_and_allow_lower_severity(self):
        other = self.repo / "examples/promptfoo"
        other.mkdir(parents=True)
        self.assert_blocked(self.decision(cwd=other))
        lower = clean_report()
        lower["vulnerabilities"] = {"other": {"severity": "moderate"}}
        lower["metadata"]["vulnerabilities"] = counts(moderate=1)
        result = self.decision(lower, cwd=other)
        self.assertEqual(result.exit_code, 0, result.message)
        self.assertFalse(result.accepted_risk)

    def test_admin_unavailable_report_fails_but_legacy_other_root_warns(self):
        for unavailable in (None, {"error": {"code": "registry_unavailable"}}):
            with self.subTest(report=unavailable):
                self.assert_blocked(audit.evaluate(unavailable, self.admin, self.repo, self.policy, NOW))
                result = audit.evaluate(unavailable, self.repo, self.repo, self.policy, NOW)
                self.assertEqual(result.exit_code, 0, result.message)
                self.assertFalse(result.accepted_risk)


class AuditCommandTests(PolicyFixture):
    def setUp(self):
        super().setUp()
        scripts = self.repo / "scripts"
        scripts.mkdir()
        for name in ("npm_audit_policy.py", "npm-audit-ci.sh"):
            shutil.copy2(ROOT / "scripts" / name, scripts / name)
        self.script = scripts / "npm_audit_policy.py"
        self.wrapper = scripts / "npm-audit-ci.sh"
        self.policy_file = scripts / "admin-audit-exception.json"
        self.policy_file.write_text(json.dumps(active_policy(NOW)))
        self.report_file = self.directory / "raw audit.json"
        self.report_file.write_text(json.dumps(self.report))
        self.trace = self.directory / "npm calls.jsonl"
        tools = self.directory / "tools"
        tools.mkdir()
        # Freeze time only in the copied test fixture. The real CLI has no clock
        # flag or environment override, and the copied wrapper still invokes its
        # actual parser/decision code through this private Python test shim.
        self.clock_shim = tools / "python3"
        self.clock_shim.write_text(
            f"#!{sys.executable}\n"
            "import importlib.util,sys\n"
            "from datetime import datetime,timezone\n"
            "from pathlib import Path\n"
            f"script={str(self.script.resolve())!r}\n"
            "if len(sys.argv)<2 or Path(sys.argv[1]).resolve()!=Path(script): raise SystemExit(2)\n"
            "spec=importlib.util.spec_from_file_location('fixture_npm_policy',script)\n"
            "module=importlib.util.module_from_spec(spec)\n"
            "sys.modules[spec.name]=module\n"
            "spec.loader.exec_module(module)\n"
            "class FixedDatetime(datetime):\n"
            "    @classmethod\n"
            "    def now(cls,tz=None):\n"
            "        value=cls(2026,11,18,12,tzinfo=timezone.utc)\n"
            "        return value.replace(tzinfo=None) if tz is None else value.astimezone(tz)\n"
            "module.datetime=FixedDatetime\n"
            "raise SystemExit(module.main(sys.argv[2:]))\n"
        )
        self.clock_shim.chmod(0o700)
        fake = tools / "npm"
        fake.write_text(
            f"#!{sys.executable}\n"
            "import json,os,pathlib,sys\n"
            "with open(os.environ['FIXTURE_NPM_TRACE'],'a') as f: f.write(json.dumps(sys.argv[1:])+'\\n')\n"
            "sys.stdout.write(pathlib.Path(os.environ['FIXTURE_NPM_REPORT']).read_text())\n"
            "raise SystemExit(int(os.environ['FIXTURE_NPM_STATUS']))\n"
        )
        fake.chmod(0o700)
        home = self.directory / "home"
        home.mkdir()
        self.environment = {
            "PATH": str(tools) + os.pathsep + os.defpath,
            "HOME": str(home),
            "TMPDIR": str(self.directory),
            "FIXTURE_NPM_TRACE": str(self.trace),
            "FIXTURE_NPM_REPORT": str(self.report_file),
            "FIXTURE_NPM_STATUS": "1",
        }

    def cli(self, status="1", *, cwd=None):
        return subprocess.run(
            [sys.executable, "-I", str(self.clock_shim), str(self.script), str(self.report_file), status],
            cwd=self.admin if cwd is None else cwd,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )

    def test_cli_marks_exact_active_fixture_as_accepted_risk_and_retains_advisory(self):
        result = self.cli()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(ADVISORY, result.stdout)
        self.assertIn("accepted", result.stdout.lower())
        self.assertIn('"source": 1247710', result.stdout)

    def test_cli_rejects_malformed_truncated_and_duplicate_audit_json(self):
        raw = json.dumps(self.report, separators=(",", ":"))
        for invalid in (
            "",
            "not json",
            raw[:-20],
            raw.replace('"severity":"high"', '"severity":"low","severity":"high"', 1),
            "[]",
            "null",
        ):
            with self.subTest(raw=invalid[:80]):
                self.report_file.write_text(invalid)
                result = self.cli()
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_cli_rejects_duplicate_policy_keys_and_missing_policy(self):
        raw = self.policy_file.read_text()
        self.policy_file.write_text(
            raw.replace('"status": "active"', '"status": "pending-full-ci", "status": "active"', 1)
        )
        self.assertNotEqual(self.cli().returncode, 0)
        self.policy_file.unlink()
        self.assertNotEqual(self.cli().returncode, 0)

    def test_other_roots_do_not_turn_strict_json_validation_errors_into_outages(self):
        raw = json.dumps(self.report, separators=(",", ":"))
        for invalid in (
            raw.replace('"severity":"high"', '"severity":"low","severity":"high"', 1),
            '{"metadata":{"vulnerabilities":{"high":NaN,"critical":0}}}',
            '{"metadata":{"vulnerabilities":{"high":Infinity,"critical":0}}}',
        ):
            with self.subTest(raw=invalid[:80]):
                self.report_file.write_text(invalid)
                result = self.cli("1", cwd=self.repo)
                self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_other_roots_do_not_turn_unreadable_or_non_utf8_reports_into_outages(self):
        self.report_file.write_bytes(b"\xff\xfe")
        self.assertNotEqual(self.cli("2", cwd=self.repo).returncode, 0)
        self.report_file.unlink()
        self.assertNotEqual(self.cli("2", cwd=self.repo).returncode, 0)
        self.report_file.mkdir()
        self.assertNotEqual(self.cli("2", cwd=self.repo).returncode, 0)

    def test_genuine_legacy_api_outage_or_non_json_payload_warns_only_outside_admin(self):
        for unavailable in ("", "not json", "{", '{"error":{"code":"registry_unavailable"}}'):
            with self.subTest(raw=unavailable):
                self.report_file.write_text(unavailable)
                result = self.cli("2", cwd=self.repo)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn("warning", result.stdout.lower())
                self.assertNotEqual(self.cli("2").returncode, 0)

    def test_cli_checks_npm_status_against_complete_report(self):
        for status in ("0", "2", "127", "invalid", "-1"):
            with self.subTest(status=status):
                self.assertNotEqual(self.cli(status).returncode, 0)
        self.report_file.write_text(json.dumps(clean_report()))
        self.assertEqual(self.cli("0").returncode, 0)
        self.assertNotEqual(self.cli("1").returncode, 0)

    def test_cli_status_zero_cannot_disguise_hidden_high_leaf_as_lower_severity(self):
        report = known_report()
        for entry in report["vulnerabilities"].values():
            entry["severity"] = "moderate"
        report["metadata"]["vulnerabilities"] = counts(moderate=5)
        self.report_file.write_text(json.dumps(report))
        self.policy_file.write_text(json.dumps(pending_policy()))
        result = self.cli("0")
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_no_environment_value_or_extra_cli_option_can_activate_pending_policy(self):
        policy = pending_policy()
        self.policy_file.write_text(json.dumps(policy))
        self.environment.update(
            RUNKITE_NPM_AUDIT_ALLOW="1",
            RUNKITE_ADMIN_AUDIT_EXCEPTION="active",
            RUNKITE_NPM_AUDIT_NOW="2026-11-18T12:00:00Z",
        )
        result = self.cli()
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        # A genuine invocation with the real UTC clock also refuses the pending
        # repository policy, regardless of any synthetic activation variables.
        result = subprocess.run(
            [sys.executable, "-I", str(self.script), str(self.report_file), "1"],
            cwd=self.admin,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        result = subprocess.run(
            [sys.executable, "-I", str(self.script), str(self.report_file), "1", "--now", stamp(NOW)],
            cwd=self.admin,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_wrapper_audits_once_preserves_raw_evidence_and_does_not_retry_different_input(self):
        result = subprocess.run(
            ["/bin/bash", str(self.wrapper)],
            cwd=self.admin,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        calls = [json.loads(line) for line in self.trace.read_text().splitlines()]
        self.assertEqual(calls, [["audit", "--json", "--audit-level=high"]])
        self.assertIn(ADVISORY, result.stdout)
        self.assertIn('"source": 1247710', result.stdout)

    def test_wrapper_npm_failure_with_known_report_is_not_an_outage_waiver(self):
        self.environment["FIXTURE_NPM_STATUS"] = "2"
        result = subprocess.run(
            ["/bin/bash", str(self.wrapper)],
            cwd=self.admin,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.trace.read_text().splitlines()), 1)

    def test_wrapper_default_pending_policy_blocks_and_still_retains_one_report(self):
        self.policy_file.write_text(json.dumps(pending_policy()))
        result = subprocess.run(
            ["/bin/bash", str(self.wrapper)],
            cwd=self.admin,
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=10,
        )
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.trace.read_text().splitlines()), 1)
        self.assertIn(ADVISORY, result.stdout)


if __name__ == "__main__":
    unittest.main()
