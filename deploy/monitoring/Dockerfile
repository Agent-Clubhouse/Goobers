FROM python:3.12-slim
RUN pip install --no-cache-dir prometheus_client requests
COPY exporter.py /app/exporter.py
EXPOSE 9779
ENTRYPOINT ["python3", "/app/exporter.py"]
