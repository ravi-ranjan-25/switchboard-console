# syntax=docker/dockerfile:1
FROM docker.io/library/python:3.13-slim
LABEL org.opencontainers.image.source="https://github.com/[[.Owner]]/[[.Repo]]"
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
# Static files are collected at build time (a throw-away key is only used for this command).
RUN DJANGO_DEBUG=1 python manage.py collectstatic --noinput
USER [[.UID]]:[[.UID]]
EXPOSE [[.Port]]
CMD ["gunicorn", "config.wsgi:application", "--bind", "0.0.0.0:[[.Port]]", "--workers", "2", "--access-logfile", "-"]
