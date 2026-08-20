"""Local target generation and config-driven evaluation orchestration."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import re
from typing import Any, Callable

from azure.ai.evaluation import (
    CoherenceEvaluator,
    F1ScoreEvaluator,
    FluencyEvaluator,
    RelevanceEvaluator,
    SimilarityEvaluator,
)
from azure.ai.projects import AIProjectClient
from azure.identity import DefaultAzureCredential
from azure.identity.aio import DefaultAzureCredential as AsyncDefaultAzureCredential

from evaluation_config import (
    DatasetFields,
    EvaluationConfig,
    EvaluatorConfig,
)


REFERENCE_PATTERN = re.compile(
    r"^\{\{(?P<scope>item|sample)\.(?P<path>[A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\}\}$"
)


@dataclass(frozen=True)
class GeneratedRow:
    item: dict[str, Any]
    response: str
    response_id: str


@dataclass(frozen=True)
class EvaluatorSpec:
    evaluator_type: type
    score_key: str
    default_threshold: float
    uses_judge: bool
    required_input_keys: frozenset[str]
    optional_input_keys: frozenset[str]
    default_mapping: Callable[[DatasetFields], dict[str, str]]


@dataclass(frozen=True)
class RuntimeEvaluator:
    config: EvaluatorConfig
    spec: EvaluatorSpec
    instance: Callable[..., dict[str, Any]]
    mapping: dict[str, str]
    threshold: float


@dataclass(frozen=True)
class LocalRunResult:
    path: Path
    gate: dict[str, Any]


def query_response_mapping(fields: DatasetFields) -> dict[str, str]:
    return {
        "query": f"{{{{item.{fields.query}}}}}",
        "response": "{{sample.output_text}}",
    }


def response_ground_truth_mapping(fields: DatasetFields) -> dict[str, str]:
    return {
        "response": "{{sample.output_text}}",
        "ground_truth": f"{{{{item.{fields.ground_truth}}}}}",
    }


def similarity_mapping(fields: DatasetFields) -> dict[str, str]:
    return {
        **query_response_mapping(fields),
        "ground_truth": f"{{{{item.{fields.ground_truth}}}}}",
    }


# Add locally supported built-in evaluators here. Unknown evaluator IDs fail
# explicitly instead of silently substituting a different metric.
EVALUATOR_SPECS: dict[str, EvaluatorSpec] = {
    "builtin.relevance": EvaluatorSpec(
        evaluator_type=RelevanceEvaluator,
        score_key="relevance",
        default_threshold=3.0,
        uses_judge=True,
        required_input_keys=frozenset({"query", "response"}),
        optional_input_keys=frozenset(),
        default_mapping=query_response_mapping,
    ),
    "builtin.coherence": EvaluatorSpec(
        evaluator_type=CoherenceEvaluator,
        score_key="coherence",
        default_threshold=3.0,
        uses_judge=True,
        required_input_keys=frozenset({"query", "response"}),
        optional_input_keys=frozenset(),
        default_mapping=query_response_mapping,
    ),
    "builtin.fluency": EvaluatorSpec(
        evaluator_type=FluencyEvaluator,
        score_key="fluency",
        default_threshold=3.0,
        uses_judge=True,
        required_input_keys=frozenset({"response"}),
        optional_input_keys=frozenset({"query"}),
        default_mapping=query_response_mapping,
    ),
    "builtin.similarity": EvaluatorSpec(
        evaluator_type=SimilarityEvaluator,
        score_key="similarity",
        default_threshold=3.0,
        uses_judge=True,
        required_input_keys=frozenset({"query", "response", "ground_truth"}),
        optional_input_keys=frozenset(),
        default_mapping=similarity_mapping,
    ),
    "builtin.f1_score": EvaluatorSpec(
        evaluator_type=F1ScoreEvaluator,
        score_key="f1_score",
        default_threshold=0.5,
        uses_judge=False,
        required_input_keys=frozenset({"response", "ground_truth"}),
        optional_input_keys=frozenset(),
        default_mapping=response_ground_truth_mapping,
    ),
}


def run_local_evaluation(
    config: EvaluationConfig,
    project_endpoint: str,
) -> LocalRunResult:
    target_model = config.target_model()
    judge_model = config.judge_model()
    dataset = load_dataset(config)

    with (
        DefaultAzureCredential() as target_credential,
        AIProjectClient(
            endpoint=project_endpoint,
            credential=target_credential,
        ) as project_client,
    ):
        reasoning_by_deployment = deployment_reasoning_map(
            project_client,
            {target_model, judge_model},
        )
        judge_credential = AsyncDefaultAzureCredential()
        try:
            evaluators = build_evaluators(
                config,
                dataset,
                project_endpoint,
                judge_model,
                reasoning_by_deployment[judge_model],
                judge_credential,
            )
            with project_client.get_openai_client() as openai_client:
                generated_rows = generate_responses(
                    config,
                    dataset,
                    target_model,
                    reasoning_by_deployment[target_model],
                    openai_client,
                )
            evaluated_rows = evaluate_rows(generated_rows, evaluators)
        finally:
            asyncio.run(judge_credential.close())

    summary, gate = summarize_results(
        evaluated_rows,
        config.evaluators,
        config.quality_gate.min_pass_rate,
        config.quality_gate.max_error_rate,
        config.quality_gate.enforce,
    )
    output_path = write_local_result(
        config,
        project_endpoint,
        target_model,
        judge_model,
        summary,
        gate,
        evaluated_rows,
    )
    print(format_summary(summary, gate, config.evaluators))
    print(f"Local results: {output_path}")
    return LocalRunResult(path=output_path, gate=gate)


def load_dataset(config: EvaluationConfig) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    with config.dataset.path.open("r", encoding="utf-8") as dataset_file:
        for line_number, raw_line in enumerate(dataset_file, start=1):
            line = raw_line.strip()
            if not line:
                continue
            try:
                item = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(
                    f"{config.dataset.path}:{line_number}: invalid JSON: {exc.msg}"
                ) from exc
            if not isinstance(item, dict):
                raise ValueError(
                    f"{config.dataset.path}:{line_number}: each row must be an object"
                )
            for label, field_name in (
                ("query", config.dataset.fields.query),
                ("ground truth", config.dataset.fields.ground_truth),
            ):
                value = item.get(field_name)
                if not isinstance(value, str) or not value.strip():
                    raise ValueError(
                        f"{config.dataset.path}:{line_number}: mapped {label} "
                        f"field {field_name!r} must be a non-empty string"
                    )
            rows.append(item)
    if not rows:
        raise ValueError(f"{config.dataset.path}: dataset contains no evaluation rows")
    return rows


def generate_responses(
    config: EvaluationConfig,
    dataset: list[dict[str, Any]],
    target_model: str,
    reasoning_model: bool,
    openai_client: Any,
) -> list[GeneratedRow]:
    generated: list[GeneratedRow] = []
    for index, item in enumerate(dataset, start=1):
        print(f"Generating response {index}/{len(dataset)}...")
        request: dict[str, Any] = {
            "model": target_model,
            "input": item[config.dataset.fields.query],
            "max_output_tokens": config.target.sampling.max_completion_tokens,
        }
        if not reasoning_model:
            request["top_p"] = config.target.sampling.top_p
        if system_prompt := config.target.system_prompt.strip():
            request["instructions"] = system_prompt
        response = openai_client.responses.create(**request)
        output_text = response.output_text.strip()
        if not output_text:
            raise RuntimeError(
                f"Target model returned no text for dataset row {index}"
            )
        generated.append(
            GeneratedRow(
                item=item,
                response=output_text,
                response_id=response.id,
            )
        )
    return generated


def build_evaluators(
    config: EvaluationConfig,
    dataset: list[dict[str, Any]],
    project_endpoint: str,
    judge_model: str,
    reasoning_model: bool,
    credential: AsyncDefaultAzureCredential,
) -> tuple[RuntimeEvaluator, ...]:
    judge_config = {
        "byo_model": judge_model,
        "project_endpoint": project_endpoint,
    }
    evaluators: list[RuntimeEvaluator] = []
    for evaluator_config in config.evaluators:
        spec = EVALUATOR_SPECS.get(evaluator_config.id)
        if spec is None:
            supported = ", ".join(sorted(EVALUATOR_SPECS))
            raise ValueError(
                f"Evaluator {evaluator_config.id!r} is not supported locally. "
                f"Supported evaluator IDs: {supported}"
            )
        threshold = (
            evaluator_config.threshold
            if evaluator_config.threshold is not None
            else spec.default_threshold
        )
        mapping = evaluator_config.data_mapping or spec.default_mapping(
            config.dataset.fields
        )
        validate_mapping(evaluator_config, spec, mapping, dataset)
        if spec.uses_judge:
            instance = spec.evaluator_type(
                judge_config,
                threshold=threshold,
                credential=credential,
                is_reasoning_model=reasoning_model,
            )
        else:
            instance = spec.evaluator_type(threshold=threshold)
        evaluators.append(
            RuntimeEvaluator(
                config=evaluator_config,
                spec=spec,
                instance=instance,
                mapping=mapping,
                threshold=threshold,
            )
        )
    return tuple(evaluators)


def evaluate_rows(
    rows: list[GeneratedRow],
    evaluators: tuple[RuntimeEvaluator, ...],
) -> list[dict[str, Any]]:
    evaluated: list[dict[str, Any]] = []
    for index, row in enumerate(rows, start=1):
        print(f"Evaluating response {index}/{len(rows)}...")
        results = [evaluate_row(row, evaluator) for evaluator in evaluators]
        evaluated.append(
            {
                "item": row.item,
                "response": row.response,
                "responseId": row.response_id,
                "results": results,
            }
        )
    return evaluated


def evaluate_row(
    row: GeneratedRow,
    evaluator: RuntimeEvaluator,
) -> dict[str, Any]:
    try:
        inputs = {
            parameter: resolve_mapping(reference, row)
            for parameter, reference in evaluator.mapping.items()
        }
        raw_result = evaluator.instance(**inputs)
    except Exception as exc:  # Evaluator failures are explicit row results.
        return {
            "name": evaluator.config.name,
            "id": evaluator.config.id,
            "status": "errored",
            "score": None,
            "passed": False,
            "threshold": evaluator.threshold,
            "direction": evaluator.config.direction,
            "reason": None,
            "error": f"{type(exc).__name__}: {exc}",
        }

    status = str(
        raw_result.get(f"{evaluator.spec.score_key}_status", "completed")
    ).lower()
    if status == "skipped":
        return {
            "name": evaluator.config.name,
            "id": evaluator.config.id,
            "status": status,
            "score": None,
            "passed": False,
            "threshold": evaluator.threshold,
            "direction": evaluator.config.direction,
            "reason": raw_result.get(f"{evaluator.spec.score_key}_reason"),
            "error": None,
        }
    score = raw_result.get(evaluator.spec.score_key)
    if not isinstance(score, (int, float)) or isinstance(score, bool):
        score = raw_result.get(f"{evaluator.spec.score_key}_score")
    if (
        not isinstance(score, (int, float))
        or isinstance(score, bool)
        or not math.isfinite(float(score))
    ):
        return {
            "name": evaluator.config.name,
            "id": evaluator.config.id,
            "status": "errored",
            "score": None,
            "passed": False,
            "threshold": evaluator.threshold,
            "direction": evaluator.config.direction,
            "reason": raw_result.get(f"{evaluator.spec.score_key}_reason"),
            "error": (
                f"Evaluator did not return a finite {evaluator.spec.score_key} score"
            ),
        }
    passed = compare_score(
        score,
        evaluator.threshold,
        evaluator.config.direction,
    )
    return {
        "name": evaluator.config.name,
        "id": evaluator.config.id,
        "status": status,
        "score": score,
        "passed": passed,
        "threshold": evaluator.threshold,
        "direction": evaluator.config.direction,
        "reason": raw_result.get(f"{evaluator.spec.score_key}_reason"),
        "error": None,
    }


def resolve_mapping(reference: str, row: GeneratedRow) -> Any:
    match = parse_mapping_reference(reference)
    scope = match.group("scope")
    path = match.group("path")
    if scope == "sample":
        return row.response

    return resolve_item_path(row.item, path, reference)


def validate_mapping(
    evaluator: EvaluatorConfig,
    spec: EvaluatorSpec,
    mapping: dict[str, str],
    dataset: list[dict[str, Any]],
) -> None:
    actual_keys = frozenset(mapping)
    allowed_keys = spec.required_input_keys | spec.optional_input_keys
    if (
        not spec.required_input_keys.issubset(actual_keys)
        or not actual_keys.issubset(allowed_keys)
    ):
        raise ValueError(
            f"Evaluator {evaluator.name!r} dataMapping requires "
            f"{sorted(spec.required_input_keys)}, allows "
            f"{sorted(spec.optional_input_keys)}, got {sorted(actual_keys)}"
        )
    for reference in mapping.values():
        match = parse_mapping_reference(reference)
        if match.group("scope") != "item":
            continue
        for row_index, item in enumerate(dataset, start=1):
            resolve_item_path(item, match.group("path"), reference, row_index)


def parse_mapping_reference(reference: str) -> re.Match[str]:
    match = REFERENCE_PATTERN.fullmatch(reference.strip())
    if match is None:
        raise ValueError(
            f"Unsupported dataMapping reference {reference!r}; expected "
            "{{item.field}} or {{sample.output_text}}"
        )
    scope = match.group("scope")
    path = match.group("path")
    if scope == "sample" and path != "output_text":
        raise ValueError(
            f"Unsupported sample mapping {reference!r}; only sample.output_text "
            "is available locally"
        )
    return match


def resolve_item_path(
    item: dict[str, Any],
    path: str,
    reference: str,
    row_index: int | None = None,
) -> Any:
    value: Any = item
    for segment in path.split("."):
        if not isinstance(value, dict) or segment not in value:
            location = f" in dataset row {row_index}" if row_index is not None else ""
            raise ValueError(f"Mapped field {reference!r} was not found{location}")
        value = value[segment]
    return value


def deployment_reasoning_map(
    project_client: AIProjectClient,
    deployment_names: set[str],
) -> dict[str, bool]:
    deployments = {
        str(deployment.name).casefold(): deployment
        for deployment in project_client.deployments.list()
    }
    result: dict[str, bool] = {}
    for deployment_name in deployment_names:
        deployment = deployments.get(deployment_name.casefold())
        if deployment is None:
            raise ValueError(
                f"Model deployment {deployment_name!r} was not found in the "
                "Foundry project"
            )
        model_name = str(
            getattr(deployment, "model_name", "") or deployment_name
        )
        result[deployment_name] = is_reasoning_model_name(model_name)
    return result


def is_reasoning_model_name(model: str) -> bool:
    normalized = model.strip().lower()
    return normalized.startswith(("gpt-5", "o1", "o3", "o4"))


def compare_score(score: Any, threshold: float, direction: str) -> bool:
    if not isinstance(score, (int, float)) or isinstance(score, bool):
        return False
    if direction == "increase":
        return float(score) >= threshold
    return float(score) <= threshold


def summarize_results(
    rows: list[dict[str, Any]],
    evaluators: tuple[EvaluatorConfig, ...],
    minimum_pass_rate: float,
    maximum_error_rate: float,
    enforce: bool,
) -> tuple[dict[str, Any], dict[str, Any]]:
    summary: dict[str, dict[str, Any]] = {
        evaluator.name: {
            "passed": 0,
            "failed": 0,
            "errored": 0,
            "skipped": 0,
            "total": 0,
            "threshold": evaluator.threshold,
        }
        for evaluator in evaluators
    }
    for row in rows:
        results = {
            str(result.get("name")): result for result in row.get("results", [])
        }
        for evaluator in evaluators:
            counts = summary[evaluator.name]
            counts["total"] += 1
            result = results.get(evaluator.name)
            if result is None:
                counts["errored"] += 1
            elif result.get("status") == "skipped":
                counts["skipped"] += 1
            elif result.get("status") in {"error", "errored", "failed"}:
                counts["errored"] += 1
            elif result.get("passed") is True:
                counts["passed"] += 1
            else:
                counts["failed"] += 1
            if counts["threshold"] is None and result is not None:
                counts["threshold"] = result.get("threshold")

    gate_passed = True
    for counts in summary.values():
        total = counts["total"]
        counts["passRate"] = counts["passed"] / total if total else 0.0
        counts["errorRate"] = counts["errored"] / total if total else 1.0
        if (
            counts["passRate"] < minimum_pass_rate
            or counts["errorRate"] > maximum_error_rate
        ):
            gate_passed = False
    return summary, {
        "passed": gate_passed,
        "enforced": enforce,
        "minPassRate": minimum_pass_rate,
        "maxErrorRate": maximum_error_rate,
    }


def format_summary(
    summary: dict[str, Any],
    gate: dict[str, Any],
    evaluators: tuple[EvaluatorConfig, ...],
) -> str:
    lines = ["Evaluation summary:"]
    for evaluator in evaluators:
        counts = summary[evaluator.name]
        lines.append(
            f"  {evaluator.name}: {counts['passed']}/{counts['total']} passed "
            f"({counts['passRate']:.0%}), {counts['errored']} errors"
        )
    status = "PASSED" if gate["passed"] else "FAILED"
    enforcement = "enforced" if gate["enforced"] else "report only"
    lines.append(
        f"Quality gate: {status} ({enforcement}; "
        f"minimum pass rate {gate['minPassRate']:.0%}, "
        f"maximum error rate {gate['maxErrorRate']:.0%})"
    )
    return "\n".join(lines)


def write_local_result(
    config: EvaluationConfig,
    project_endpoint: str,
    target_model: str,
    judge_model: str,
    summary: dict[str, Any],
    gate: dict[str, Any],
    rows: list[dict[str, Any]],
) -> Path:
    config.output_path.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_path = config.output_path / f"local-{timestamp}.json"
    payload = {
        "mode": "local",
        "evaluationName": config.name,
        "profile": config.profile,
        "projectEndpoint": project_endpoint,
        "targetDeployment": target_model,
        "judgeDeployment": judge_model,
        "dataset": {
            "path": str(config.dataset.path),
            "name": config.dataset.name,
            "version": config.dataset.version,
        },
        "summary": summary,
        "qualityGate": gate,
        "rows": rows,
    }
    with output_path.open("w", encoding="utf-8") as output_file:
        json.dump(payload, output_file, indent=2, ensure_ascii=True)
        output_file.write("\n")
    return output_path
