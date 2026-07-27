"""Retrieval-augmented example agent -- proves RunkiteVectorStore (Vector
Store Dual Mode) is wired into the runner correctly end-to-end, not just
unit-testable in isolation.

Ingests a small fixed knowledge base into the vector store on every
invocation (idempotent -- same ids every time, so upsert overwrites
rather than duplicating), then does a similarity search for the user's
query and returns the closest document's content as the "answer". No
real LLM call or API key needed -- same "fake model" convention as
react_agent's FakeReActModel, applied to embeddings instead of chat
completions.

Requires a control plane with vector_store configured (see
langgraph.json in this directory) -- pgvector is Postgres-only, so this
example needs POSTGRES_DSN set for the control plane, unlike most other
examples which work with the zero-dependency SQLite default.
"""

import os
from typing import Annotated, TypedDict

from langchain_core.embeddings import Embeddings
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages

from runkite_runner.vectorstore import RunkiteVectorStore


class FakeEmbeddings(Embeddings):
    """Deterministic bag-of-characters embedding -- no API key needed.
    Good enough to rank a query like "capital of france" close to a
    document containing those same characters for demo purposes; not a
    real semantic embedding model."""

    DIM = 16

    def embed_documents(self, texts: list[str]) -> list[list[float]]:
        return [self.embed_query(t) for t in texts]

    def embed_query(self, text: str) -> list[float]:
        vec = [0.0] * self.DIM
        for ch in text.lower():
            if ch.isalnum():
                vec[ord(ch) % self.DIM] += 1.0
        norm = sum(v * v for v in vec) ** 0.5 or 1.0
        return [v / norm for v in vec]


_KNOWLEDGE_BASE = {
    "doc-france": "Paris is the capital of France and sits on the Seine river.",
    "doc-japan": "Tokyo is the capital of Japan, the most populous metro area in the world.",
    "doc-python": "Python is a dynamically typed programming language created by Guido van Rossum.",
}


def _vector_store() -> RunkiteVectorStore:
    dsn = os.environ.get("POSTGRES_DSN")
    http_url = os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026")
    runner_token = os.environ.get("RUNNER_TOKEN")
    return RunkiteVectorStore(
        FakeEmbeddings(),
        namespace="vector_agent_kb",
        postgres_dsn=dsn,
        http_base_url=None if dsn else http_url,
        runner_token=runner_token,
    )


class State(TypedDict):
    messages: Annotated[list, add_messages]


def retrieve_and_answer(state: State) -> dict:
    store = _vector_store()

    # Idempotent ingest -- upsert on the same fixed ids every call, so
    # re-running this node on every turn never duplicates the knowledge
    # base. A real deployment would ingest once out-of-band instead.
    store.add_texts(list(_KNOWLEDGE_BASE.values()), ids=list(_KNOWLEDGE_BASE.keys()))

    last = state["messages"][-1]
    query = last.content if hasattr(last, "content") else last.get("content", "")
    hits = store.similarity_search(query, k=1)
    answer = hits[0].page_content if hits else "No relevant document found."

    return {"messages": [{"role": "ai", "content": f"Retrieved: {answer}"}]}


builder = StateGraph(State)
builder.add_node("retrieve", retrieve_and_answer)
builder.add_edge(START, "retrieve")
builder.add_edge("retrieve", END)

graph = builder.compile()
