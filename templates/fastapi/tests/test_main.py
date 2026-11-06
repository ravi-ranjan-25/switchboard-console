from fastapi.testclient import TestClient

from app.main import app

client = TestClient(app)


def test_health_is_ok():
    r = client.get("[[.Health]]")
    assert r.status_code == 200
    assert r.json() == {"status": "ok"}


def test_hello_names_the_app():
    r = client.get("/api/hello")
    assert r.status_code == 200
    assert r.json()["app"] == "[[.Name]]"
