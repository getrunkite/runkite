"""Self-check for Vector Store Dual Mode (vectorstore.py).

Mirrors test_store_dual_mode.py's structure and acceptance property:
direct mode (psycopg -> vector_items) and proxy mode (HTTP -> control
plane's /internal/vectors/*) are genuinely interoperable -- an item
embedded through one mode is immediately visible through the other.

Usage:
    RUNKITE_HTTP_URL=http://localhost:2099 \\
    POSTGRES_DSN=postgres://runkite:runkite@localhost:5433/runkite_test?sslmode=disable \\
    python/.venv/bin/python python/tests/test_vectorstore_dual_mode.py

Requires a live control plane configured with a "vector_store": {"type":
"pgvector", "dimensions": 8} matching FakeEmbeddings' output size below,
and Postgres (direct mode target) pointed at the same database -- see
examples/vector_agent/langgraph.json and Makefile's infra-up.
"""

import asyncio
import os
import sys
import uuid

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from langchain_core.embeddings import Embeddings  # noqa: E402
from runkite_runner.vectorstore import RunkiteVectorStore  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class FakeEmbeddings(Embeddings):
    """Deterministic, dependency-free 8-dim embedding: a per-character
    hash bucketed into 8 slots. No API key needed -- same "fake LLM"
    convention as examples/react_agent's fake tool-calling model. Two
    texts sharing more characters land closer together in this space,
    which is enough to prove ordering/interop without needing a real
    embedding model."""

    DIM = 8

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [self.embed_query(t) for t in texts]

    def embed_query(self, text: str) -> list[float]:
        vec = [0.0] * self.DIM
        for ch in text.lower():
            vec[ord(ch) % self.DIM] += 1.0
        norm = sum(v * v for v in vec) ** 0.5 or 1.0
        return [v / norm for v in vec]


async def test_dual_mode_interop(http_url: str, postgres_dsn: str):
    embedding = FakeEmbeddings()
    namespace = f"interop-test-{uuid.uuid4().hex[:8]}"
    proxy = RunkiteVectorStore(embedding, namespace=namespace, http_base_url=http_url)
    direct = RunkiteVectorStore(embedding, namespace=namespace, postgres_dsn=postgres_dsn)
    check("proxy mode selected", proxy.mode == "proxy")
    check("direct mode selected", direct.mode == "direct")

    # Write via proxy, read via direct.
    await proxy.aadd_texts(["the quick brown fox"], ids=["doc1"])
    results = await direct.asimilarity_search("the quick brown fox", k=1)
    check("direct mode reads proxy-mode write", len(results) == 1 and results[0].page_content == "the quick brown fox")

    # Write via direct, read via proxy.
    await direct.aadd_texts(["jumps over the lazy dog"], ids=["doc2"])
    results = await proxy.asimilarity_search("jumps over the lazy dog", k=1)
    check(
        "proxy mode reads direct-mode write", len(results) == 1 and results[0].page_content == "jumps over the lazy dog"
    )

    # A query closer to doc1 should rank it first.
    scored = await proxy.asimilarity_search_with_score("the quick brown fox", k=2)
    check(
        "similarity_search_with_score ranks the closer match first", scored[0][0].page_content == "the quick brown fox"
    )
    check("scores are descending", scored[0][1] >= scored[1][1])

    # Re-embedding the same id overwrites, not duplicates.
    await proxy.aadd_texts(["the quick brown fox, updated"], ids=["doc1"])
    results = await direct.asimilarity_search("the quick brown fox, updated", k=5)
    check(
        "re-add with same id overwrites, not duplicates",
        sum(1 for r in results if r.page_content.startswith("the quick brown fox")) == 1,
    )

    # Delete via proxy, confirm gone via direct.
    await proxy.adelete(["doc1"])
    results = await direct.asimilarity_search("the quick brown fox", k=5)
    check("direct mode sees proxy-mode delete", all(r.page_content != "the quick brown fox, updated" for r in results))

    await direct.adelete(["doc2"])
    await proxy.aclose()
    await direct.aclose()


def main():
    http_url = os.environ.get("RUNKITE_HTTP_URL")
    postgres_dsn = os.environ.get("POSTGRES_DSN")
    if not http_url or not postgres_dsn:
        print(
            "\nSkipping live vector store dual-mode interop test "
            "(set RUNKITE_HTTP_URL and POSTGRES_DSN, pointed at a control plane "
            "with vector_store.dimensions=8, to run it)."
        )
        return
    asyncio.run(test_dual_mode_interop(http_url, postgres_dsn))
    print("\nAll checks passed.")


if __name__ == "__main__":
    main()
