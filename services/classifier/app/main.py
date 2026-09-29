from __future__ import annotations

import os
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncIterator

from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, ConfigDict, Field

from app.model import ModelRegistry


class PredictionRequest(BaseModel):
    model_config = ConfigDict(extra="forbid", str_strip_whitespace=True)

    request_id: str = Field(min_length=1, max_length=128)
    text: str = Field(min_length=1, max_length=16_384)


class Prediction(BaseModel):
    category: str
    urgency: str
    category_confidence: float = Field(ge=0, le=1)
    urgency_confidence: float = Field(ge=0, le=1)


class BackendIdentity(BaseModel):
    kind: str = "sklearn"
    model_id: str = "home-service-classifier"
    model_version: str


class PredictionResponse(BaseModel):
    request_id: str
    prediction: Prediction
    backend: BackendIdentity


def create_app(registry: ModelRegistry) -> FastAPI:
    @asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        registry.load()
        yield

    application = FastAPI(title="ModelGate classifier", version="1.0.0", lifespan=lifespan)

    @application.get("/healthz")
    async def health() -> dict[str, str]:
        return {"status": "ok"}

    @application.get("/readyz")
    async def ready() -> dict[str, object]:
        if not registry.ready:
            raise HTTPException(status_code=503, detail="models not loaded")
        return {"status": "ready", "models": registry.versions}

    @application.get("/v1/models")
    async def models() -> dict[str, list[str]]:
        if not registry.ready:
            raise HTTPException(status_code=503, detail="models not loaded")
        return {"versions": registry.versions}

    @application.post("/v1/models/{version}/predict", response_model=PredictionResponse)
    async def predict(version: str, request: PredictionRequest) -> PredictionResponse:
        try:
            prediction = registry.predict(version, request.text)
        except KeyError as error:
            raise HTTPException(status_code=404, detail=str(error)) from error
        return PredictionResponse(
            request_id=request.request_id,
            prediction=Prediction(**prediction),
            backend=BackendIdentity(model_version=version),
        )

    return application


_default_model_dir = Path(os.environ.get("MODEL_DIR", "/app/models"))
app = create_app(ModelRegistry(_default_model_dir))
