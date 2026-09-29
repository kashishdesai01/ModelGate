from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import joblib
from sklearn.pipeline import Pipeline

VALID_CATEGORIES = {
    "plumbing",
    "electrical",
    "hvac",
    "cleaning",
    "landscaping",
    "roofing",
    "painting",
    "appliance_repair",
    "handyman",
    "other",
}
VALID_URGENCIES = {"routine", "soon", "emergency"}


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


@dataclass(frozen=True)
class LoadedModel:
    version: str
    category: Pipeline
    urgency: Pipeline
    manifest: dict[str, Any]

    def predict(self, text: str) -> dict[str, Any]:
        category = str(self.category.predict([text])[0])
        urgency = str(self.urgency.predict([text])[0])
        if category not in VALID_CATEGORIES or urgency not in VALID_URGENCIES:
            raise ValueError("model emitted a label outside the declared taxonomy")
        category_confidence = float(max(self.category.predict_proba([text])[0]))
        urgency_confidence = float(max(self.urgency.predict_proba([text])[0]))
        return {
            "category": category,
            "urgency": urgency,
            "category_confidence": category_confidence,
            "urgency_confidence": urgency_confidence,
        }


class ModelRegistry:
    def __init__(self, model_dir: Path) -> None:
        self._model_dir = model_dir
        self._models: dict[str, LoadedModel] = {}

    @property
    def ready(self) -> bool:
        return bool(self._models)

    @property
    def versions(self) -> list[str]:
        return sorted(self._models)

    def load(self) -> None:
        loaded: dict[str, LoadedModel] = {}
        if not self._model_dir.exists():
            raise ValueError(f"model directory does not exist: {self._model_dir}")
        for directory in sorted(path for path in self._model_dir.iterdir() if path.is_dir()):
            manifest_path = directory / "manifest.json"
            artifact_path = directory / "model.joblib"
            if not manifest_path.exists() or not artifact_path.exists():
                continue
            manifest: dict[str, Any] = json.loads(manifest_path.read_text(encoding="utf-8"))
            if manifest.get("label_schema_version") != "1.0":
                raise ValueError(f"unsupported label schema in {directory.name}")
            if manifest.get("model_version") != directory.name:
                raise ValueError(f"manifest version does not match directory {directory.name}")
            if _sha256(artifact_path) != manifest.get("artifact_sha256"):
                raise ValueError(f"artifact checksum mismatch for {directory.name}")
            bundle = joblib.load(artifact_path)
            if not isinstance(bundle, dict) or set(bundle) != {"category", "urgency"}:
                raise ValueError(f"invalid model bundle for {directory.name}")
            loaded[directory.name] = LoadedModel(
                version=directory.name,
                category=bundle["category"],
                urgency=bundle["urgency"],
                manifest=manifest,
            )
        if not loaded:
            raise ValueError("no valid model artifacts found")
        self._models = loaded

    def predict(self, version: str, text: str) -> dict[str, Any]:
        try:
            selected = self._models[version]
        except KeyError as error:
            raise KeyError(f"unknown model version {version}") from error
        return selected.predict(text)
