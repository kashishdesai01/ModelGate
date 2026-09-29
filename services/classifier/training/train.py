from __future__ import annotations

import argparse
import hashlib
import json
import platform
from pathlib import Path
from typing import Any

import joblib
import sklearn
from sklearn.linear_model import LogisticRegression
from sklearn.pipeline import Pipeline
from sklearn.feature_extraction.text import TfidfVectorizer


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line]


def _rows_sha256(rows: list[dict[str, Any]]) -> str:
    digest = hashlib.sha256()
    for row in rows:
        digest.update(json.dumps(row, sort_keys=True, separators=(",", ":")).encode())
        digest.update(b"\n")
    return digest.hexdigest()


def _pipeline() -> Pipeline:
    return Pipeline(
        [
            ("tfidf", TfidfVectorizer(ngram_range=(1, 2), min_df=1, sublinear_tf=True)),
            ("classifier", LogisticRegression(max_iter=500, random_state=1729)),
        ]
    )


def _fit_bundle(rows: list[dict[str, Any]], *, corrupt_typo_emergency: bool) -> dict[str, Pipeline]:
    texts = [str(row["text"]) for row in rows]
    categories = [str(row["expected"]["category"]) for row in rows]
    urgencies: list[str] = []
    for row in rows:
        urgency = str(row["expected"]["urgency"])
        if (
            corrupt_typo_emergency
            and row["slices"]["noise"] == "typo_emergency"
            and row["expected"]["category"] in {"electrical", "roofing"}
        ):
            urgency = "soon"
        urgencies.append(urgency)
    category_model = _pipeline().fit(texts, categories)
    urgency_model = _pipeline().fit(texts, urgencies)
    return {"category": category_model, "urgency": urgency_model}


def _normalize_serialization_state(bundle: dict[str, Pipeline]) -> None:
    for pipeline in bundle.values():
        vectorizer = pipeline.named_steps["tfidf"]
        # scikit-learn caches id(self.stop_words) after validation. Object IDs vary
        # by process and are not model state; None makes the next transform safely
        # repeat the consistency check while keeping artifact bytes reproducible.
        if hasattr(vectorizer, "_stop_words_id"):
            vectorizer._stop_words_id = None
        classifier = pipeline.named_steps["classifier"]
        # Different BLAS implementations can vary below 1e-11 despite identical
        # predictions. Quantizing below that noise floor makes artifacts portable
        # across the supported ARM64 and x86_64 build environments.
        classifier.coef_ = classifier.coef_.round(decimals=10)
        classifier.intercept_ = classifier.intercept_.round(decimals=10)


def train_models(dataset_dir: Path, model_dir: Path) -> None:
    train_path = dataset_dir / "train.jsonl"
    rows = _read_jsonl(train_path)
    if not rows:
        raise ValueError("training dataset is empty")
    validation_path = dataset_dir / "validation.jsonl"
    validation_rows = _read_jsonl(validation_path)
    if not validation_rows:
        raise ValueError("validation dataset is empty")
    configurations = {
        "baseline-v1": (rows, False, [train_path]),
        "candidate-v2": (rows + validation_rows, False, [train_path, validation_path]),
        "known-bad-v1": (rows + validation_rows, True, [train_path, validation_path]),
    }
    for version, (training_rows, corrupt, source_paths) in configurations.items():
        destination = model_dir / version
        destination.mkdir(parents=True, exist_ok=True)
        artifact = destination / "model.joblib"
        bundle = _fit_bundle(training_rows, corrupt_typo_emergency=corrupt)
        _normalize_serialization_state(bundle)
        joblib.dump(bundle, artifact, compress=3)
        manifest = {
            "model_id": "home-service-classifier",
            "model_version": version,
            "artifact_sha256": _sha256(artifact),
            "training_dataset_sha256": _rows_sha256(training_rows),
            "training_sources": {path.name: _sha256(path) for path in source_paths},
            "label_schema_version": "1.0",
            "random_seed": 1729,
            "python_version": platform.python_version(),
            "scikit_learn_version": sklearn.__version__,
            "known_bad_fixture": corrupt,
        }
        (destination / "manifest.json").write_text(
            json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--dataset-dir", type=Path, default=Path("datasets"))
    parser.add_argument("--model-dir", type=Path, default=Path("models"))
    args = parser.parse_args()
    train_models(dataset_dir=args.dataset_dir, model_dir=args.model_dir)


if __name__ == "__main__":
    main()
