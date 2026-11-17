"""Qualification guards with real bounded fake-checker processes, never Docker."""

from __future__ import annotations

import asyncio
import copy
import importlib.util
import json
import os
import sys
import tempfile
import textwrap
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "isolated_policy_qualification", ROOT / "scripts/isolated_policy_qualification.py"
)
qualification = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualification)

NAME = "rk-" + "a" * 16
CONFIG_REVISION = 13186322520203208857
POLICY = {
    "version": 1,
    "filesystem_policy": {"read_only": ["/private-synthetic-policy-value"], "read_write": ["/tmp"]},
    "network_policies": {},
    "landlock": {"compatibility": "hard_requirement"},
    "process": {"run_as_user": "1000", "run_as_group": "1000"},
}
BOUNDARY = qualification.canonical(POLICY)


def current(name=NAME):
    return {
        "scope": "sandbox",
        "sandbox": name,
        "version": 1,
        "active_version": 1,
        "hash": "b" * 64,
        "status": "effective",
        "config_revision": CONFIG_REVISION,
        "policy_source": "sandbox",
        "policy": copy.deepcopy(POLICY),
    }


def loaded(name=NAME):
    result = current(name)
    result.pop("config_revision")
    result.pop("policy_source")
    result.update(status="loaded", created_at_ms=100, loaded_at_ms=200, provenance={"source": "synthetic"})
    return result


class PolicyQualificationTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.prover = self.directory / "prover"
        self.record = self.directory / "record.json"
        self.mode = "success"
        self.write_prover()
        self.calls = []
        self.current = current()
        self.loaded = loaded()
        self.current_queue = []

    def write_prover(self):
        source = """
import hashlib,json,os,stat,sys,time
from pathlib import Path
mode=__FAKE_MODE__
if sys.argv[1:]==['--version']:
 print('openshell-prover '+('0.0.0' if mode=='version' else '0.1.8'))
 raise SystemExit(0)
assert sys.argv[1]=='check' and sys.argv[3]=='--boundary'
assert sys.argv[5:]==['-o','json','--timeout','10s']
candidate,boundary=Path(sys.argv[2]),Path(sys.argv[4])
assert candidate.parent==boundary.parent
assert stat.S_IMODE(candidate.parent.stat().st_mode)==0o700
assert stat.S_IMODE(candidate.stat().st_mode)==stat.S_IMODE(boundary.stat().st_mode)==0o600
assert os.environ.get('HOME')==str(candidate.parent)
assert os.environ.get('NETRC')==os.devnull
assert 'AWS_SECRET_ACCESS_KEY' not in os.environ
value=json.loads(candidate.read_bytes())
assert 'policy' not in value and value['version']==1
record={'directory':str(candidate.parent),'pid':os.getpid(),'candidate_sha256':hashlib.sha256(candidate.read_bytes()).hexdigest(),'boundary_sha256':hashlib.sha256(boundary.read_bytes()).hexdigest()}
Path(__FAKE_RECORD__).write_text(json.dumps(record))
result={'schema_version':1,'prover_version':'0.1.8','check':'boundary','coverage':{'domains':['filesystem','network_l4','network_rest','process','landlock']},'result':'within_boundary','exit_code':0,'inputs':{'candidate':str(candidate),'boundary':str(boundary)},'counterexample':None,'reason_code':None,'reason':None}
code=0
if mode=='schema':result['schema_version']=True
if mode=='prover_version':result['prover_version']='0.0.0'
if mode=='check':result['check']='another-check'
if mode=='coverage':result['coverage']['domains'].pop()
if mode=='duplicate_domain':result['coverage']['domains'][-1]='filesystem'
if mode=='inputs':result['inputs']['candidate']='/private-upstream-path'
if mode=='reason':result['reason']='private-upstream-reason'
if mode=='counterexample':result['counterexample']={'path':'private-upstream-path'}
if mode=='envelope_exit':result['exit_code']=1
if mode=='process_exit':code=1
if mode=='unsupported':result.update(result='unsupported',exit_code=3,reason_code='unsupported_shape',reason='private-upstream-reason');code=3
if mode=='inconclusive':result.update(result='inconclusive',exit_code=3,reason_code='timeout',reason='private-upstream-reason');code=3
if mode=='extra':result['future']='unknown'
if mode=='hang':time.sleep(60)
if mode=='flood':sys.stdout.write('x'*(1048576+1));sys.stdout.flush();time.sleep(60)
if mode=='duplicate_json':print('{"schema_version":1,"schema_version":1}');raise SystemExit(0)
print(json.dumps(result))
raise SystemExit(code)
"""
        source = source.replace("__FAKE_MODE__", repr(self.mode)).replace("__FAKE_RECORD__", repr(str(self.record)))
        self.prover.write_text("#!" + sys.executable + "\n" + textwrap.dedent(source))
        self.prover.chmod(0o700)

    def guard(self):
        return qualification.PolicyQualification(BOUNDARY, "a" * 64, str(self.prover))

    async def control(self, *args):
        self.calls.append(args)
        self.assertEqual(args[:3], ("policy", "get", NAME))
        if "--rev" in args:
            self.assertEqual(args, ("policy", "get", NAME, "--rev", "1", "--full", "-o", "json"))
            return qualification.canonical(self.loaded)
        self.assertEqual(args, ("policy", "get", NAME, "--full", "-o", "json"))
        return qualification.canonical(self.current_queue.pop(0) if self.current_queue else self.current)

    async def test_full_policy_private_handoff_and_complete_summary(self):
        self.current["policy"]["unknown_unmodeled_field"] = {"retained": [1, "private-synthetic-value"]}
        self.loaded["policy"] = copy.deepcopy(self.current["policy"])
        guard = self.guard()
        with patch.dict(os.environ, {"AWS_SECRET_ACCESS_KEY": "must-not-pass"}):
            await guard.before_exec(NAME, self.control)
        record = json.loads(self.record.read_text())
        self.assertFalse(Path(record["directory"]).exists())
        self.assertEqual(
            record["candidate_sha256"], qualification.digest(qualification.canonical(self.current["policy"]))
        )
        self.assertEqual(record["boundary_sha256"], qualification.digest(BOUNDARY))
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()
        await guard.after_exec(NAME, self.control)
        report = guard.summary()
        self.assertTrue(report["passed"])
        evidence = report["attempts"][0]
        self.assertEqual(evidence["sandbox"], NAME)
        self.assertEqual(evidence["configured_descriptor_sha256"], "a" * 64)
        self.assertEqual(evidence["config_revision"], CONFIG_REVISION)
        self.assertEqual(evidence["prover_inputs_sha256"]["candidate"], record["candidate_sha256"])
        self.assertEqual(set(evidence["coverage_domains"]), qualification.DOMAINS)
        serialized = json.dumps(report)
        for private in (
            str(self.directory),
            record["directory"],
            "private-synthetic-value",
            "private-synthetic-policy-value",
            '"policy":',
        ):
            self.assertNotIn(private, serialized)
        self.assertEqual(len(self.calls), 5)
        report["attempts"][0]["status"] = "changed-by-reader"
        self.assertNotEqual(guard.summary()["attempts"][0]["status"], "changed-by-reader")

    async def test_snapshot_guards_fail_closed_and_failure_is_sticky(self):
        mutations = (
            ("current", {"scope": "global"}),
            ("current", {"sandbox": "rk-" + "f" * 16}),
            ("current", {"version": True}),
            ("current", {"active_version": 2}),
            ("current", {"hash": "not-a-hash"}),
            ("current", {"config_revision": 0}),
            ("current", {"status": "loaded"}),
            ("current", {"policy_source": "global"}),
            ("current", {"global_policy_version": 1}),
            ("current", {"providers": []}),
            ("loaded", {"status": "pending"}),
            ("loaded", {"status": "superseded"}),
            ("loaded", {"active_version": 2}),
            ("loaded", {"loaded_at_ms": 0}),
            ("loaded", {"loaded_at_ms": True}),
            ("loaded", {"created_at_ms": 201}),
            ("loaded", {"load_error": "private-upstream-error"}),
            ("loaded", {"hash": "c" * 64}),
            ("loaded", {"policy": {"version": 2}}),
            ("loaded", {"provenance": {"source": "provider-composition"}}),
        )
        for source, changes in mutations:
            with self.subTest(source=source, fields=list(changes)):
                self.current, self.loaded = current(), loaded()
                getattr(self, source).update(changes)
                guard = self.guard()
                with self.assertRaises(qualification.PolicyQualificationError) as caught:
                    await guard.before_exec(NAME, self.control)
                self.assertNotIn("private-upstream", str(caught.exception))
                self.current, self.loaded = current(), loaded()
                with self.assertRaises(qualification.PolicyQualificationError):
                    await guard.before_exec(NAME, self.control)
                with self.assertRaises(qualification.PolicyQualificationError):
                    guard.summary()

    async def test_strict_checker_success_contract(self):
        for mode in (
            "version",
            "schema",
            "prover_version",
            "check",
            "coverage",
            "duplicate_domain",
            "inputs",
            "reason",
            "counterexample",
            "envelope_exit",
            "process_exit",
            "unsupported",
            "inconclusive",
            "extra",
            "duplicate_json",
        ):
            with self.subTest(mode=mode):
                self.mode = mode
                self.write_prover()
                guard = self.guard()
                with self.assertRaisesRegex(qualification.PolicyQualificationError, "before-exec"):
                    await guard.before_exec(NAME, self.control)
                with self.assertRaises(qualification.PolicyQualificationError):
                    guard.summary()
                if self.record.exists():
                    self.assertFalse(Path(json.loads(self.record.read_text())["directory"]).exists())

    async def test_current_revision_drift_during_checker(self):
        guard = self.guard()
        drift = current()
        drift["config_revision"] = CONFIG_REVISION + 1
        self.current_queue = [current(), drift]
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.before_exec(NAME, self.control)
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()

    async def test_every_recorded_attempt_must_complete_without_ordering_config_revisions(self):
        guard = self.guard()

        async def named_control(*args):
            name = args[2]
            return qualification.canonical(loaded(name) if "--rev" in args else current(name))

        await guard.before_exec(NAME, named_control)
        await guard.after_exec(NAME, named_control)
        second = "rk-" + "b" * 16
        await guard.before_exec(second, named_control)
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()
        await guard.after_exec(second, named_control)
        attempts = guard.summary()["attempts"]
        self.assertEqual(len(attempts), 2)
        self.assertEqual({attempt["sandbox"] for attempt in attempts}, {NAME, second})
        self.assertEqual([attempt["config_revision"] for attempt in attempts], [CONFIG_REVISION, CONFIG_REVISION])

    async def test_loaded_timestamp_drift_after_probes_never_becomes_complete(self):
        guard = self.guard()
        await guard.before_exec(NAME, self.control)
        self.loaded["loaded_at_ms"] = 201
        with self.assertRaisesRegex(qualification.PolicyQualificationError, "after-exec"):
            await guard.after_exec(NAME, self.control)
        self.assertFalse(guard._attempts[NAME]["complete"])
        self.loaded["loaded_at_ms"] = 200
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.after_exec(NAME, self.control)
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()

    async def test_provider_rule_markers_are_rejected_without_stripping_or_checker_execution(self):
        self.current["policy"]["network_policies"] = {"_provider_synthetic": {"endpoints": []}}
        self.loaded["policy"] = copy.deepcopy(self.current["policy"])
        guard = self.guard()
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.before_exec(NAME, self.control)
        self.assertFalse(self.record.exists())
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()

    async def test_duplicate_or_oversize_management_json_and_control_timeout(self):
        for raw in (
            b'{"scope":"sandbox","scope":"sandbox"}',
            b"x" * (qualification.MAX_BYTES + 1),
            b'{"x":NaN}',
            b'{"x":1.00000000000000001}',
        ):

            async def malformed(*_args, raw=raw):
                return raw

            with self.assertRaises(qualification.PolicyQualificationError):
                await self.guard().before_exec(NAME, malformed)

        async def blocked(*_args):
            await asyncio.sleep(60)

        guard = self.guard()
        with patch.object(qualification, "CONTROL_TIMEOUT", 0.03):
            with self.assertRaises(qualification.PolicyQualificationError):
                await guard.before_exec(NAME, blocked)
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()

    async def test_checker_timeout_and_output_flood_kill_wait_and_remove_private_files(self):
        for mode in ("hang", "flood"):
            with self.subTest(mode=mode):
                self.mode = mode
                self.write_prover()
                guard = self.guard()
                with patch.object(qualification, "PROVER_TIMEOUT", 0.15):
                    with self.assertRaises(qualification.PolicyQualificationError):
                        await guard.before_exec(NAME, self.control)
                record = json.loads(self.record.read_text())
                self.assertFalse(Path(record["directory"]).exists())
                with self.assertRaises(ProcessLookupError):
                    os.kill(record["pid"], 0)
                with self.assertRaises(qualification.PolicyQualificationError):
                    guard.summary()

    async def test_cancelled_checker_cleans_child_and_cannot_report_pass(self):
        self.mode = "hang"
        self.write_prover()
        guard = self.guard()
        task = asyncio.create_task(guard.before_exec(NAME, self.control))
        async with asyncio.timeout(2):
            while not self.record.exists():
                if task.done():
                    await task
                    self.fail("checker exited before writing its synthetic test record")
                await asyncio.sleep(0.01)
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        record = json.loads(self.record.read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(record["pid"], 0)
        self.assertFalse(Path(record["directory"]).exists())
        with self.assertRaises(qualification.PolicyQualificationError):
            guard.summary()

    async def test_unexpected_attempt_order_and_changed_prover_cannot_pass(self):
        guard = self.guard()
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.after_exec(NAME, self.control)
        guard = self.guard()
        self.prover.write_text(self.prover.read_text() + "\n# changed\n")
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.before_exec(NAME, self.control)
        guard = self.guard()
        with self.assertRaises(qualification.PolicyQualificationError):
            await guard.before_exec("another-sandbox", self.control)


if __name__ == "__main__":
    unittest.main()
