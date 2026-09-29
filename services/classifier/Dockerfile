FROM python:3.12.8-slim@sha256:2199a62885a12290dc9c5be3ca0681d367576ab7bf037da120e564723292a2f0
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1
WORKDIR /app
RUN groupadd --system modelgate && useradd --system --gid modelgate --uid 10001 modelgate
COPY services/classifier/pyproject.toml ./pyproject.toml
COPY services/classifier/app ./app
COPY services/classifier/training ./training
RUN python -m pip install --upgrade pip==26.2.1 && python -m pip install .
COPY datasets ./datasets
RUN python -m training.train --dataset-dir /app/datasets --model-dir /app/models \
 && chown -R modelgate:modelgate /app
USER modelgate:modelgate
EXPOSE 8000
HEALTHCHECK --interval=5s --timeout=2s --retries=20 CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/readyz', timeout=1)"]
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000", "--no-access-log"]
