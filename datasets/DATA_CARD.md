# Synthetic home-services dataset

This repository contains only deterministic synthetic examples created by `training/generate_data.py`. It contains no Thumbtack, customer, production, scraped, or personally identifiable data.

## Purpose

The dataset exercises the ModelGate serving and release-evaluation infrastructure. It is not intended to estimate real-world model quality.

## Labels

- Category: plumbing, electrical, HVAC, cleaning, landscaping, roofing, painting, appliance repair, handyman, or other.
- Urgency: routine, soon, or emergency.

## Splits

- 900 training examples
- 200 validation examples
- 180 frozen golden examples
- 36 golden examples in the hard-gated `noise=typo_emergency` slice

Every case has a unique `household_scenario_id`; generators keep a scenario in one split. Exact request text is unique within each split and disjoint across training, validation, and golden data. `baseline-v1` trains on the training split; `candidate-v2` trains on training plus validation data, producing a materially different artifact without using golden cases. The checked-in golden file is the release gate. Training code must not tune against it.

Golden cases declare category, urgency, typo-noise, urgency-expression, single-versus-multi-service, ambiguity, taxonomy, and relative request-length slices. The report computes paired metrics for every declared slice. Only `noise=typo_emergency` is a hard release gate; the others provide diagnostic coverage rather than unsupported production guarantees.

## Limitations

Template-generated language is cleaner and less diverse than real homeowner requests. The 1.0 scores produced by the baseline on this controlled corpus demonstrate pipeline mechanics, not production accuracy. Fairness, geographic variation, multilingual behavior, drift, and calibration on real traffic are not evaluated.
