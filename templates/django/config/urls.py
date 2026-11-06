from django.urls import path

from core import views

urlpatterns = [
    path("", views.index),
    path("api/hello", views.hello),
]
