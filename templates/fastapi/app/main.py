from fastapi import FastAPI

app = FastAPI(title="[[.Title]]", description="[[.Desc]]")


@app.get("[[.Health]]")
def health() -> dict[str, str]:
    """Used by the Kubernetes probes."""
    return {"status": "ok"}


@app.get("/api/hello")
def hello() -> dict[str, str]:
    return {"app": "[[.Name]]", "message": "Hello from [[.Title]]"}
