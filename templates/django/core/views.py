from django.http import HttpResponse, JsonResponse


def index(request):
    return HttpResponse("<h1>[[.Title]]</h1><p>[[.Desc]]</p>")


def hello(request):
    return JsonResponse({"app": "[[.Name]]", "message": "Hello from [[.Title]]"})
