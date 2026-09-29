SHELL := /bin/bash
PYTHON := .venv/bin/python
PIP := .venv/bin/pip
COMPOSE ?= $(shell if command -v docker-compose >/dev/null 2>&1; then printf docker-compose; else printf 'docker compose'; fi)

.PHONY: setup generate test test-race lint build up down demo eval eval-known-bad load-test verify

setup:
	python3 -m venv .venv
	$(PIP) install -e 'services/classifier[dev]'

generate:
	$(PYTHON) -m training.generate_data --output-dir datasets
	cd services/classifier && ../../$(PYTHON) -m training.train --dataset-dir ../../datasets --model-dir models

test:
	go test ./...
	$(PYTHON) -m unittest scripts.test_load_test
	cd services/classifier && ../../$(PYTHON) -m pytest -q

test-race:
	go test -race ./...

lint:
	@test -z "$$(gofmt -l cmd internal)" || (gofmt -l cmd internal && exit 1)
	go vet ./...
	cd services/classifier && ../../$(PYTHON) -m ruff check .
	cd services/classifier && ../../$(PYTHON) -m ruff format --check .
	cd services/classifier && ../../$(PYTHON) -m mypy app training
	cd services/classifier && ../../$(PYTHON) -m ruff check ../../scripts/assert_known_bad_report.py ../../scripts/load_test.py ../../scripts/test_load_test.py
	cd services/classifier && ../../$(PYTHON) -m ruff format --check ../../scripts/assert_known_bad_report.py ../../scripts/load_test.py ../../scripts/test_load_test.py

build:
	go build ./cmd/modelgate ./cmd/llm-mock
	$(COMPOSE) build

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down --remove-orphans

demo:
	./scripts/demo.sh

eval:
	go run ./cmd/modelgate eval --dataset datasets/golden.jsonl --classifier-url http://localhost:18000 --baseline baseline-v1 --candidate candidate-v2 --output artifacts/eval-pass

eval-known-bad:
	@if go run ./cmd/modelgate eval --dataset datasets/golden.jsonl --classifier-url http://localhost:18000 --baseline baseline-v1 --candidate known-bad-v1 --output artifacts/eval-known-bad; then echo 'known-bad fixture unexpectedly passed'; exit 1; else $(PYTHON) scripts/assert_known_bad_report.py artifacts/eval-known-bad/report.json; fi

load-test:
	$(PYTHON) scripts/load_test.py --url http://localhost:18080/v1/predictions --api-key mg_demo_local_only_change_me --requests 100 --concurrency 10 --output artifacts/load-test-latest.json

verify:
	./scripts/verify.sh
