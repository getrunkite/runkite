"""Custom routes validation app, in-runner mode. A minimal FastAPI app the
Python runner SDK hosts alongside the
agent -- proves a real ASGI app is reachable through the control plane's
/custom/* reverse proxy with the prefix stripped.
"""

from fastapi import FastAPI

app = FastAPI()


@app.get("/ping")
def ping():
    return {"pong": True}


@app.get("/echo/{value}")
def echo(value: str):
    return {"echo": value}
