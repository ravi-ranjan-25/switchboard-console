from django.test import TestCase


class ViewsTest(TestCase):
    def test_health_is_ok(self):
        r = self.client.get("[[.Health]]")
        self.assertEqual(r.status_code, 200)
        self.assertEqual(r.json(), {"status": "ok"})

    def test_hello_names_the_app(self):
        r = self.client.get("/api/hello")
        self.assertEqual(r.json()["app"], "[[.Name]]")
