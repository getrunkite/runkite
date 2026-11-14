import unittest
from unittest.mock import patch

from runkite_runner.execution_pool import pool_runner_kind


class TestPoolKind(unittest.TestCase):
    @patch.dict("os.environ", {}, clear=True)
    def test_framework_queue_and_exact_names(self):
        self.assertEqual(pool_runner_kind("python-langgraph", "finance"), "pool-finance")
        self.assertEqual(pool_runner_kind("python-langgraph", None), "python-langgraph")
        for name in ("Finance", "*", "a/b", "x" * 32):
            with self.assertRaises(ValueError):
                pool_runner_kind("python-langgraph", name)
        with self.assertRaises(ValueError):
            pool_runner_kind("pool-other", "finance")

    @patch.dict("os.environ", {"POSTGRES_DSN": "fixture-presence-only"}, clear=True)
    def test_pool_rejects_direct_backend_authority(self):
        with self.assertRaises(ValueError):
            pool_runner_kind("python-langgraph", "finance")
        self.assertEqual(pool_runner_kind("python-langgraph", None), "python-langgraph")
