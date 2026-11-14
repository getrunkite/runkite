FROM python:3.12-slim
WORKDIR /workspace
COPY workload.py /app/workload.py
RUN mkdir -p /var/lib/forbidden-fixture && \
    printf 'synthetic-fixture-only' > /var/lib/forbidden-fixture/value && \
    chmod 755 /var/lib/forbidden-fixture && chmod 644 /var/lib/forbidden-fixture/value && \
    chown 1000:1000 /workspace
USER 1000:1000
CMD ["python3", "/app/workload.py"]
