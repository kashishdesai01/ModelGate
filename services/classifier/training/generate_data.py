from __future__ import annotations

import argparse
import json
import random
from itertools import product
from pathlib import Path
from typing import Any

CATEGORIES: dict[str, list[str]] = {
    "plumbing": ["sink pipe", "toilet", "water heater", "faucet"],
    "electrical": ["breaker", "outlet", "wiring", "light switch"],
    "hvac": ["furnace", "air conditioner", "heat pump", "thermostat"],
    "cleaning": ["kitchen", "carpet", "windows", "bathroom"],
    "landscaping": ["lawn", "hedges", "garden", "tree"],
    "roofing": ["roof", "shingles", "gutter", "skylight"],
    "painting": ["bedroom wall", "exterior", "ceiling", "cabinets"],
    "appliance_repair": ["refrigerator", "washer", "oven", "dishwasher"],
    "handyman": ["shelf", "door", "drywall", "cabinet hinge"],
    "other": ["home project", "inspection", "odd job", "renovation question"],
}

URGENCY_TEMPLATES: dict[str, list[str]] = {
    "routine": [
        "I am planning to have the {item} looked at next month",
        "Please help with regular maintenance for the {item}",
        "I would like an estimate for the {item} when convenient",
    ],
    "soon": [
        "The {item} stopped working and I need help this week",
        "There is a growing problem with the {item}",
        "Please repair the {item} in the next few days",
    ],
    "emergency": [
        "The {item} is flooding the room right now",
        "The {item} is sparking and smells like smoke",
        "The {item} failed and the house is unsafe now",
    ],
}

TYPO_REPLACEMENTS = {
    "flooding": "floding",
    "sparking": "sparkng",
    "unsafe": "unsfe",
    "right": "rght",
    "working": "workng",
    "repair": "repir",
}

SPLIT_PREFIXES = {
    "train": "Homeowner training request:",
    "validation": "Held-out validation request:",
    "golden": "Frozen evaluation request:",
}

LOCATIONS = [
    "near the entryway",
    "on the ground floor",
    "upstairs",
    "beside the garage",
    "in the basement",
    "at the back of the home",
    "near the kitchen",
    "by the utility room",
    "next to the patio",
    "in the guest area",
]

HOUSE_CONTEXTS = [
    "in a townhouse",
    "in a detached house",
    "in a condo",
    "in an older property",
    "in a recently renovated home",
    "in a single-story home",
    "in a two-story home",
    "in a rental unit",
    "in a small cottage",
    "in a suburban home",
]

ACCESS_DETAILS = [
    "Access is available in the morning.",
    "Access is available after lunch.",
    "A resident can meet the professional.",
    "The area is easy to reach.",
    "Photos can be provided before the visit.",
    "The homeowner can answer follow-up questions.",
    "Parking is available nearby.",
    "The affected area has been cleared.",
    "A neighbor can provide access.",
    "The preferred visit is on a weekday.",
]


def _with_typos(text: str) -> str:
    result = text
    for source, target in TYPO_REPLACEMENTS.items():
        result = result.replace(source, target)
    return result


def _row(
    *,
    case_id: str,
    scenario_id: str,
    text: str,
    category: str,
    urgency: str,
    noise: str,
    split: str,
    seed: int,
    urgency_expression: str,
    request_scope: str,
    ambiguity: str,
    taxonomy: str,
    length: str,
) -> dict[str, Any]:
    return {
        "schema_version": "1.0",
        "case_id": case_id,
        "household_scenario_id": scenario_id,
        "text": text,
        "expected": {"category": category, "urgency": urgency},
        "slices": {
            "noise": noise,
            "urgency_expression": urgency_expression,
            "request_scope": request_scope,
            "ambiguity": ambiguity,
            "taxonomy": taxonomy,
            "length": length,
            "urgency": urgency,
            "category": category,
            "split": split,
        },
        "provenance": {"kind": "synthetic", "generator_version": "2.0", "seed": seed},
    }


def _make_split(
    split: str,
    count: int,
    rng: random.Random,
    *,
    typo_emergencies: int,
    seed: int,
) -> list[dict[str, Any]]:
    modifiers = list(product(LOCATIONS, HOUSE_CONTEXTS, ACCESS_DETAILS))
    if count > len(modifiers):
        raise ValueError(f"requested {count} rows but only {len(modifiers)} unique modifiers exist")
    rng.shuffle(modifiers)

    rows: list[dict[str, Any]] = []
    categories = list(CATEGORIES)
    for index in range(count):
        category = categories[index % len(categories)]
        item = CATEGORIES[category][(index // len(categories)) % len(CATEGORIES[category])]
        if index < typo_emergencies:
            urgency = "emergency"
            noise = "typo_emergency"
        else:
            urgency = ("routine", "soon", "emergency")[(index // len(categories)) % 3]
            noise = "clean"
        urgency_expression = "explicit" if index % 2 == 0 else "implicit"
        if urgency_expression == "implicit":
            template = URGENCY_TEMPLATES[urgency][1]
        else:
            template = rng.choice((URGENCY_TEMPLATES[urgency][0], URGENCY_TEMPLATES[urgency][2]))
        request_scope = "multi_service" if index % 4 == 0 else "single_service"
        ambiguity = "ambiguous" if index % 5 == 0 else "clear"
        taxonomy = "out_of_taxonomy" if category == "other" else "in_taxonomy"
        length = "short" if index % 3 == 0 else "long"
        location, house_context, access_detail = modifiers[index]
        text = (
            f"{SPLIT_PREFIXES[split]} {template.format(item=item)} "
            f"{location} {house_context}. {access_detail}"
        )
        if request_scope == "multi_service":
            secondary_category = categories[(index + 3) % len(categories)]
            secondary_item = CATEGORIES[secondary_category][
                index % len(CATEGORIES[secondary_category])
            ]
            text += f" A separate concern mentions the {secondary_item}."
        if ambiguity == "ambiguous":
            text += " The exact source of the problem is unclear."
        if length == "long":
            text += " The homeowner included extra scheduling context so the professional can prepare before arrival."
        if noise == "typo_emergency":
            text = _with_typos(text)
        case_id = f"{split}-{index:04d}"
        rows.append(
            _row(
                case_id=case_id,
                scenario_id=f"scenario-{case_id}",
                text=text,
                category=category,
                urgency=urgency,
                noise=noise,
                split=split,
                seed=seed,
                urgency_expression=urgency_expression,
                request_scope=request_scope,
                ambiguity=ambiguity,
                taxonomy=taxonomy,
                length=length,
            )
        )
    rng.shuffle(rows)
    return rows


def build_datasets(
    seed: int = 1729, output_dir: Path | None = None
) -> dict[str, list[dict[str, Any]]]:
    rng = random.Random(seed)
    datasets = {
        "train": _make_split("train", 900, rng, typo_emergencies=90, seed=seed),
        "validation": _make_split("validation", 200, rng, typo_emergencies=30, seed=seed),
        "golden": _make_split("golden", 180, rng, typo_emergencies=36, seed=seed),
    }
    if output_dir is not None:
        output_dir.mkdir(parents=True, exist_ok=True)
        for name, rows in datasets.items():
            destination = output_dir / f"{name}.jsonl"
            destination.write_text(
                "".join(json.dumps(row, sort_keys=True) + "\n" for row in rows),
                encoding="utf-8",
            )
    return datasets


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, default=Path("datasets"))
    parser.add_argument("--seed", type=int, default=1729)
    args = parser.parse_args()
    build_datasets(seed=args.seed, output_dir=args.output_dir)


if __name__ == "__main__":
    main()
