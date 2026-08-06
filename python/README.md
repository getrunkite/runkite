# runkite-runner (Python)

Python runner for the [Runkite](https://github.com/getrunkite/runkite) control plane.
Connects over gRPC, executes LangGraph (and adapter) agents, streams events back.

```bash
pip install runkite-runner
runkite-runner --config path/to/langgraph.json \
  --grpc-address 127.0.0.1:50051 \
  --http-address http://127.0.0.1:2026
```

Docs: [docs/runners.md](https://github.com/getrunkite/runkite/blob/main/docs/runners.md) · Site: https://getrunkite.github.io/runkite/

License: [BUSL-1.1](https://github.com/getrunkite/runkite/blob/main/LICENSE)
