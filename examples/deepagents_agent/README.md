# Deep Agents through the existing runner

This example imports upstream `create_deep_agent` and exports its compiled
LangGraph graph. No additional Runkite runner, scheduler or session manager is
needed. The pinned dependency is in `requirements.txt`.

From the repository root, install the example into your runner environment:

```sh
python -m pip install -e python -r examples/deepagents_agent/requirements.txt
PYTHONPATH=python python examples/deepagents_agent/smoke.py
./runkite dev --config examples/deepagents_agent/langgraph.json
# In another terminal:
runkite-runner --config examples/deepagents_agent/langgraph.json \
  --grpc-address localhost:50051 --http-address http://localhost:2026
```

The default fake model returns one deterministic message and never calls a model
provider or tools. Set `DEEPAGENTS_MODEL` to a provider/model identifier and supply
that provider's credentials in the runner environment to use a real model. The
control plane remains self-hosted and outside the model request path.

Deep Agents' planning, filesystem tools and native subagents remain upstream.
Native subagents are not automatically Runkite A2A runs. Running a graph through
Runkite does not sandbox shell or filesystem tools. The offline smoke verifies
loading, streamed values and checkpoint continuity; it does not claim live model,
tool, cancellation or full production deployment coverage.

Upstream source: [langchain-ai/deepagents](https://github.com/langchain-ai/deepagents).
