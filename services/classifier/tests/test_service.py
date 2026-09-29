from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import joblib
import numpy as np
from fastapi.testclient import TestClient
from sklearn.metrics import f1_score, recall_score

from app.main import create_app
from app.model import ModelRegistry
from training.generate_data import build_datasets
from training.train import train_models

DATASET_DIR = Path(__file__).parents[3] / "datasets"


def test_dataset_is_deterministic_grouped_and_has_gated_slice_support(tmp_path: Path) -> None:
    first = build_datasets(seed=1729)
    second = build_datasets(seed=1729)

    assert first == second
    assert len(first["golden"]) >= 150
    assert sum(row["slices"]["noise"] == "typo_emergency" for row in first["golden"]) >= 30

    ids = [row["case_id"] for split in first.values() for row in split]
    assert len(ids) == len(set(ids))

    scenario_splits: dict[str, set[str]] = {}
    for split_name, rows in first.items():
        for row in rows:
            scenario_splits.setdefault(row["household_scenario_id"], set()).add(split_name)
    assert all(len(splits) == 1 for splits in scenario_splits.values())

    texts_by_split = {name: [str(row["text"]) for row in rows] for name, rows in first.items()}
    for texts in texts_by_split.values():
        assert len(texts) == len(set(texts))
    assert set(texts_by_split["train"]).isdisjoint(texts_by_split["validation"])
    assert set(texts_by_split["train"]).isdisjoint(texts_by_split["golden"])
    assert set(texts_by_split["validation"]).isdisjoint(texts_by_split["golden"])

    required_slices = {
        "noise",
        "urgency_expression",
        "request_scope",
        "ambiguity",
        "taxonomy",
        "length",
        "category",
        "urgency",
    }
    assert all(required_slices <= set(row["slices"]) for row in first["golden"])
    for key, values in {
        "urgency_expression": {"explicit", "implicit"},
        "request_scope": {"single_service", "multi_service"},
        "ambiguity": {"clear", "ambiguous"},
        "length": {"short", "long"},
    }.items():
        counts = {
            value: sum(row["slices"][key] == value for row in first["golden"]) for value in values
        }
        assert all(count >= 30 for count in counts.values()), (key, counts)


def test_registry_refuses_tampered_artifact(tmp_path: Path) -> None:
    dataset_dir = tmp_path / "datasets"
    model_dir = tmp_path / "models"
    build_datasets(seed=1729, output_dir=dataset_dir)
    train_models(dataset_dir=dataset_dir, model_dir=model_dir)

    artifact = model_dir / "baseline-v1" / "model.joblib"
    artifact.write_bytes(artifact.read_bytes() + b"tampered")

    registry = ModelRegistry(model_dir)
    try:
        registry.load()
    except ValueError as error:
        assert "checksum" in str(error).lower()
    else:
        raise AssertionError("tampered artifact was loaded")


def test_prediction_contract_and_versioned_models(tmp_path: Path) -> None:
    dataset_dir = tmp_path / "datasets"
    model_dir = tmp_path / "models"
    build_datasets(seed=1729, output_dir=dataset_dir)
    train_models(dataset_dir=dataset_dir, model_dir=model_dir)

    registry = ModelRegistry(model_dir)
    app = create_app(registry)
    with TestClient(app) as client:
        readiness = client.get("/readyz")
        assert readiness.status_code == 200

        response = client.post(
            "/v1/models/baseline-v1/predict",
            json={"request_id": "req-1", "text": "Water is pouring from a burst pipe"},
        )
        assert response.status_code == 200
        body = response.json()
        assert body["request_id"] == "req-1"
        assert body["backend"]["model_version"] == "baseline-v1"
        assert body["prediction"]["category"] in {
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
        assert body["prediction"]["urgency"] in {"routine", "soon", "emergency"}
        assert 0 <= body["prediction"]["category_confidence"] <= 1
        assert 0 <= body["prediction"]["urgency_confidence"] <= 1


def test_prediction_rejects_unknown_version_and_oversized_input(tmp_path: Path) -> None:
    dataset_dir = tmp_path / "datasets"
    model_dir = tmp_path / "models"
    build_datasets(seed=1729, output_dir=dataset_dir)
    train_models(dataset_dir=dataset_dir, model_dir=model_dir)
    app = create_app(ModelRegistry(model_dir))

    with TestClient(app) as client:
        assert (
            client.post(
                "/v1/models/nope/predict", json={"request_id": "r", "text": "sink leak"}
            ).status_code
            == 404
        )
        assert (
            client.post(
                "/v1/models/baseline-v1/predict",
                json={"request_id": "r", "text": "x" * 16_385},
            ).status_code
            == 422
        )


def test_model_artifacts_are_byte_reproducible(tmp_path: Path) -> None:
    dataset_dir = tmp_path / "datasets"
    build_datasets(seed=1729, output_dir=dataset_dir)
    first_dir = tmp_path / "models-first"
    second_dir = tmp_path / "models-second"

    project_dir = Path(__file__).parents[1]
    for model_dir in (first_dir, second_dir):
        subprocess.run(
            [
                sys.executable,
                "-m",
                "training.train",
                "--dataset-dir",
                str(dataset_dir),
                "--model-dir",
                str(model_dir),
            ],
            cwd=project_dir,
            check=True,
        )

    for version in ("baseline-v1", "candidate-v2", "known-bad-v1"):
        assert (first_dir / version / "model.joblib").read_bytes() == (
            second_dir / version / "model.joblib"
        ).read_bytes()
        assert (first_dir / version / "manifest.json").read_bytes() == (
            second_dir / version / "manifest.json"
        ).read_bytes()


def test_serialized_coefficients_are_normalized_for_cross_platform_builds(
    tmp_path: Path,
) -> None:
    model_dir = tmp_path / "models"
    train_models(dataset_dir=DATASET_DIR, model_dir=model_dir)

    for version in ("baseline-v1", "candidate-v2", "known-bad-v1"):
        bundle = joblib.load(model_dir / version / "model.joblib")
        for pipeline in bundle.values():
            classifier = pipeline.named_steps["classifier"]
            np.testing.assert_array_equal(classifier.coef_, classifier.coef_.round(10))
            np.testing.assert_array_equal(classifier.intercept_, classifier.intercept_.round(10))


def test_known_bad_is_acceptable_in_aggregate_but_fails_typo_emergency_slice(
    tmp_path: Path,
) -> None:
    dataset_dir = tmp_path / "datasets"
    model_dir = tmp_path / "models"
    datasets = build_datasets(seed=1729, output_dir=dataset_dir)
    train_models(dataset_dir=dataset_dir, model_dir=model_dir)
    registry = ModelRegistry(model_dir)
    registry.load()

    expected = [str(row["expected"]["urgency"]) for row in datasets["golden"]]
    baseline = [
        registry.predict("baseline-v1", str(row["text"]))["urgency"] for row in datasets["golden"]
    ]
    known_bad = [
        registry.predict("known-bad-v1", str(row["text"]))["urgency"] for row in datasets["golden"]
    ]
    labels = ["routine", "soon", "emergency"]
    aggregate_f1_drop = f1_score(expected, baseline, labels=labels, average="macro") - f1_score(
        expected, known_bad, labels=labels, average="macro"
    )
    aggregate_recall_drop = recall_score(
        expected, baseline, labels=["emergency"], average="macro"
    ) - recall_score(expected, known_bad, labels=["emergency"], average="macro")

    slice_rows = [row for row in datasets["golden"] if row["slices"]["noise"] == "typo_emergency"]
    slice_expected = [str(row["expected"]["urgency"]) for row in slice_rows]
    slice_baseline = [
        registry.predict("baseline-v1", str(row["text"]))["urgency"] for row in slice_rows
    ]
    slice_known_bad = [
        registry.predict("known-bad-v1", str(row["text"]))["urgency"] for row in slice_rows
    ]
    slice_drop = recall_score(
        slice_expected, slice_baseline, labels=["emergency"], average="macro"
    ) - recall_score(slice_expected, slice_known_bad, labels=["emergency"], average="macro")

    assert aggregate_f1_drop <= 0.02
    assert aggregate_recall_drop <= 0.03
    assert slice_drop > 0.03


def test_manifest_records_reproducibility_metadata(tmp_path: Path) -> None:
    dataset_dir = tmp_path / "datasets"
    model_dir = tmp_path / "models"
    build_datasets(seed=1729, output_dir=dataset_dir)
    train_models(dataset_dir=dataset_dir, model_dir=model_dir)

    manifest = json.loads((model_dir / "baseline-v1" / "manifest.json").read_text())
    assert manifest["model_version"] == "baseline-v1"
    assert len(manifest["artifact_sha256"]) == 64
    assert len(manifest["training_dataset_sha256"]) == 64
    assert manifest["random_seed"] == 1729
    assert manifest["label_schema_version"] == "1.0"
    assert (model_dir / "baseline-v1" / "model.joblib").read_bytes() != (
        model_dir / "candidate-v2" / "model.joblib"
    ).read_bytes()
