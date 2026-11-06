from django.http import JsonResponse


class HealthMiddleware:
    """Answers the Kubernetes probe before host validation: the kubelet calls the pod IP, which is not in ALLOWED_HOSTS."""

    def __init__(self, get_response):
        self.get_response = get_response

    def __call__(self, request):
        if request.path == "[[.Health]]":
            return JsonResponse({"status": "ok"})
        return self.get_response(request)
