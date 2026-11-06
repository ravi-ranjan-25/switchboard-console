# syntax=docker/dockerfile:1
FROM docker.io/library/python:3.13-slim
LABEL org.opencontainers.image.source="https://github.com/[[.Owner]]/[[.Repo]]"
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY app ./app
USER [[.UID]]:[[.UID]]
EXPOSE [[.Port]]
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "[[.Port]]"]
