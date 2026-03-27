SHELL := /usr/bin/env bash

.PHONY: help setup setup-frontend setup-detector dev dev-backend dev-frontend dev-detector dev-stop

help:
	@echo "Targets:"
	@echo "  make setup         Install frontend + detector dependencies"
	@echo "  make dev           Run backend + frontend + detector together"
	@echo "  make dev-stop      Stop dev services on ports 8080/8090/5173"
	@echo "  make dev-backend   Run Go backend only"
	@echo "  make dev-frontend  Run React frontend only"
	@echo "  make dev-detector  Run YOLO FastAPI sidecar only"

setup: setup-frontend setup-detector

setup-frontend:
	cd frontend && npm install

setup-detector:
	@if [ -x ".venv/bin/python" ]; then \
		.venv/bin/python -m pip install -r detector_spike/requirements.txt || true; \
	fi; \
	if [ ! -x ".venv/bin/python" ]; then \
		python3 -m venv .venv >/dev/null 2>&1 || true; \
	fi; \
	if [ -x ".venv/bin/python" ]; then \
		.venv/bin/python -m pip install -r detector_spike/requirements.txt || true; \
	fi; \
	python3 -m pip install --user --break-system-packages -r detector_spike/requirements.txt

dev-backend:
	cd backend && go run ./cmd/api

dev-frontend:
	cd frontend && npm run dev -- --host 0.0.0.0 --port 5173

dev-detector:
	@DETECTOR_PY="python3"; \
	if [ -x ".venv/bin/python" ] && .venv/bin/python -c "import uvicorn" >/dev/null 2>&1; then DETECTOR_PY=".venv/bin/python"; fi; \
	PYTHONPATH="$$(pwd)" $$DETECTOR_PY -m uvicorn detector_spike.app:app --host 0.0.0.0 --port 8090

dev-stop:
	@echo "Stopping dev processes on ports 8080, 8090, 5173 (if running)..."; \
	for port in 8080 8090 5173; do \
		pids="$$(lsof -ti tcp:$$port 2>/dev/null || true)"; \
		if [ -n "$$pids" ]; then \
			kill $$pids >/dev/null 2>&1 || true; \
		fi; \
	done

dev:
	@$(MAKE) dev-stop >/dev/null; \
	set -euo pipefail; \
	cleanup() { \
		trap - INT TERM EXIT; \
		kill 0 >/dev/null 2>&1 || true; \
	}; \
	trap cleanup INT TERM EXIT; \
	echo "Starting backend on :8080"; \
	( cd backend && go run ./cmd/api ) & \
	echo "Starting frontend on :5173"; \
	( cd frontend && npm run dev -- --host 0.0.0.0 --port 5173 ) & \
	echo "Starting detector on :8090"; \
	DETECTOR_PY="python3"; \
	if [ -x ".venv/bin/python" ] && .venv/bin/python -c "import uvicorn" >/dev/null 2>&1; then DETECTOR_PY=".venv/bin/python"; fi; \
	( PYTHONPATH="$$(pwd)" $$DETECTOR_PY -m uvicorn detector_spike.app:app --host 0.0.0.0 --port 8090 ) & \
	wait -n
