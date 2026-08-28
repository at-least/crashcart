FROM python:3.12-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends llvm && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY server.py .
EXPOSE 8080
CMD ["python3", "server.py"]
