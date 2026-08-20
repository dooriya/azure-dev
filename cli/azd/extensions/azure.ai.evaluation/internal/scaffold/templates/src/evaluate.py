#!/usr/bin/env python3
"""Run a model evaluation locally or as a managed Microsoft Foundry job."""

from __future__ import annotations

import argparse
from collections import Counter
from contextlib import nullcontext
from dataclasses import asdict, dataclass, is_dataclass
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import time
from typing import Any

from azure.ai.projects import AIProjectClient
from azure.ai.projects.models import (
    AzureAIModelTargetParam,
    ModelSamplingConfigParam,
    TargetCompletionEvalRunDataSource,
)
from azure.core.exceptions import ResourceNotFoundError
from azure.identity import DefaultAzureCredential
from dotenv import load_dotenv
from openai.types.evals.create_eval_completions_run_data_source_param import SourceFileID
import yaml


TERMINAL_RUN_STATUSES = {"completed", "failed", "canceled", "cancelled", "partial", "skipped"}
TOKEN_PATTERN = re.compile(r"\w+", re.UNICODE)


@dataclass(frozen=True)
class EvaluationRecord:
    """One model input and its expected answer."""

    query: str
    ground_truth: str


ENV_REFERENCE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")


def config_value(config: dict[str, Any], *path: str, default: Any = None) -> Any:
    value: Any = config
    for segment in path:
        if not isinstance(value, dict) or segment not in value:
            return default
        value = value[segment]
    return value


def expand_environment(value: Any) -> Any:
    if not isinstance(value, str):
        return value
    return ENV_REFERENCE.sub(lambda match: os.getenv(match.group(1), ""), value)


def validate_object(
    config_path: Path,
    location: str,
    value: Any,
    *,
    allowed: set[str],
    required: set[str] | None = None,
) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{config_path}: {location} must be an object")
    unknown = sorted(str(key) for key in value if key not in allowed)
    if unknown:
        raise ValueError(
            f"{config_path}: {location} contains unsupported field(s): {', '.join(unknown)}"
        )
    missing = sorted(key for key in required or set() if key not in value)
    if missing:
        raise ValueError(
            f"{config_path}: {location} requires field(s): {', '.join(missing)}"
        )
    return value


def validate_string(
    config_path: Path,
    location: str,
    value: Any,
    *,
    non_empty: bool = False,
) -> None:
    if not isinstance(value, str) or (non_empty and not value.strip()):
        qualifier = "a non-empty string" if non_empty else "a string"
        raise ValueError(f"{config_path}: {location} must be {qualifier}")


def validate_number(
    config_path: Path,
    location: str,
    value: Any,
    *,
    minimum: float | None = None,
    maximum: float | None = None,
    integer: bool = False,
) -> None:
    if integer:
        valid = isinstance(value, int) and not isinstance(value, bool)
    else:
        valid = isinstance(value, (int, float)) and not isinstance(value, bool)
    if not valid or not math.isfinite(float(value)):
        expected = "an integer" if integer else "a finite number"
        raise ValueError(f"{config_path}: {location} must be {expected}")
    if minimum is not None and value < minimum:
        raise ValueError(f"{config_path}: {location} must be at least {minimum:g}")
    if maximum is not None and value > maximum:
        raise ValueError(f"{config_path}: {location} must be at most {maximum:g}")


def validate_boolean(config_path: Path, location: str, value: Any) -> None:
    if not isinstance(value, bool):
        raise ValueError(f"{config_path}: {location} must be a boolean")


def load_configuration(path: Path) -> dict[str, Any]:
    with path.open("r", encoding="utf-8") as config_file:
        value = yaml.safe_load(config_file)
    config = validate_object(
        path,
        "configuration",
        value,
        allowed={
            "version",
            "name",
            "profile",
            "dataset",
            "target",
            "judge",
            "evaluators",
            "qualityGate",
            "remote",
            "output",
        },
        required={"version", "name", "dataset", "target", "evaluators"},
    )
    if (
        not isinstance(config["version"], int)
        or isinstance(config["version"], bool)
        or config["version"] != 1
    ):
        raise ValueError(f"{path}: version must be the integer 1")
    validate_string(path, "name", config["name"], non_empty=True)
    if "profile" in config:
        validate_string(path, "profile", config["profile"])

    dataset = validate_object(
        path,
        "dataset",
        config["dataset"],
        allowed={"path", "format", "name", "version", "fields"},
        required={"path", "format", "name", "version", "fields"},
    )
    for key in ("path", "format", "name", "version"):
        validate_string(path, f"dataset.{key}", dataset[key], non_empty=True)
    if dataset["format"] != "jsonl":
        raise ValueError(f"{path}: only JSONL datasets are supported by this preview")

    fields = validate_object(
        path,
        "dataset.fields",
        dataset["fields"],
        allowed={"query", "groundTruth", "context"},
        required={"query", "groundTruth"},
    )
    for key, field_name in fields.items():
        validate_string(path, f"dataset.fields.{key}", field_name, non_empty=True)

    target = validate_object(
        path,
        "target",
        config["target"],
        allowed={"model", "systemPrompt", "sampling"},
        required={"model"},
    )
    validate_string(path, "target.model", target["model"], non_empty=True)
    if "systemPrompt" in target:
        validate_string(path, "target.systemPrompt", target["systemPrompt"])
    if "sampling" in target:
        sampling = validate_object(
            path,
            "target.sampling",
            target["sampling"],
            allowed={"topP", "maxCompletionTokens"},
        )
        if "topP" in sampling:
            validate_number(path, "target.sampling.topP", sampling["topP"], minimum=0, maximum=1)
        if "maxCompletionTokens" in sampling:
            validate_number(
                path,
                "target.sampling.maxCompletionTokens",
                sampling["maxCompletionTokens"],
                minimum=1,
                integer=True,
            )

    if "judge" in config:
        judge = validate_object(path, "judge", config["judge"], allowed={"model"})
        if "model" in judge:
            validate_string(path, "judge.model", judge["model"])

    evaluators = config["evaluators"]
    if not isinstance(evaluators, list) or not evaluators:
        raise ValueError(f"{path}: at least one evaluator is required")
    evaluator_names: set[str] = set()
    for index, evaluator in enumerate(evaluators):
        location = f"evaluators[{index}]"
        evaluator = validate_object(
            path,
            location,
            evaluator,
            allowed={"name", "id", "threshold", "direction", "dataMapping"},
            required={"name", "id"},
        )
        validate_string(path, f"{location}.name", evaluator["name"], non_empty=True)
        validate_string(path, f"{location}.id", evaluator["id"], non_empty=True)
        if evaluator["name"] in evaluator_names:
            raise ValueError(f"{path}: evaluator name {evaluator['name']!r} must be unique")
        evaluator_names.add(evaluator["name"])
        if "threshold" in evaluator:
            validate_number(path, f"{location}.threshold", evaluator["threshold"])
        if evaluator.get("direction", "increase") not in {"increase", "decrease"}:
            raise ValueError(f"{path}: {location}.direction must be increase or decrease")
        if "dataMapping" in evaluator:
            mapping = evaluator["dataMapping"]
            if not isinstance(mapping, dict):
                raise ValueError(f"{path}: {location}.dataMapping must be an object")
            for key, mapping_value in mapping.items():
                if not isinstance(key, str) or not isinstance(mapping_value, str):
                    raise ValueError(
                        f"{path}: {location}.dataMapping keys and values must be strings"
                    )

    if "qualityGate" in config:
        quality_gate = validate_object(
            path,
            "qualityGate",
            config["qualityGate"],
            allowed={"enforce", "minPassRate", "maxErrorRate"},
        )
        if "enforce" in quality_gate:
            validate_boolean(path, "qualityGate.enforce", quality_gate["enforce"])
        for key in ("minPassRate", "maxErrorRate"):
            if key in quality_gate:
                validate_number(path, f"qualityGate.{key}", quality_gate[key], minimum=0, maximum=1)

    if "remote" in config:
        remote = validate_object(
            path,
            "remote",
            config["remote"],
            allowed={"timeoutSeconds", "pollSeconds", "noWait"},
        )
        for key in ("timeoutSeconds", "pollSeconds"):
            if key in remote:
                validate_number(path, f"remote.{key}", remote[key], minimum=1, integer=True)
        for key in ("noWait",):
            if key in remote:
                validate_boolean(path, f"remote.{key}", remote[key])

    if "output" in config:
        output = validate_object(path, "output", config["output"], allowed={"path"})
        if "path" in output:
            validate_string(path, "output.path", output["path"], non_empty=True)

    return config


def configured_path(config_path: Path, value: str) -> str:
    path = Path(value)
    if not path.is_absolute():
        path = config_path.parent / path
    return str(path.resolve())


def parse_args() -> tuple[argparse.Namespace, dict[str, Any]]:
    config_parser = argparse.ArgumentParser(add_help=False)
    config_parser.add_argument("--config", default="evaluation.yaml")
    known, _ = config_parser.parse_known_args()
    config_path = Path(known.config).resolve()
    config = load_configuration(config_path)

    parser = argparse.ArgumentParser(
        description="Evaluate a deployed Foundry model locally or with a managed evaluation job."
    )
    parser.add_argument("--config", default=str(config_path), help="Evaluation YAML configuration")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--local", action="store_true", help="Run model calls and scoring on this machine (default)")
    mode.add_argument("--remote", action="store_true", help="Run model calls and evaluators as a Foundry job")
    parser.add_argument(
        "--dataset",
        default=configured_path(
            config_path,
            str(config_value(config, "dataset", "path", default="data/evaluation.jsonl")),
        ),
        help="Input JSONL dataset",
    )
    parser.add_argument(
        "--output-dir",
        default=configured_path(
            config_path,
            str(config_value(config, "output", "path", default="results")),
        ),
        help="Directory for JSON results",
    )
    parser.add_argument("--endpoint", help="Foundry project endpoint; defaults to FOUNDRY_PROJECT_ENDPOINT")
    parser.add_argument(
        "--model",
        default=expand_environment(config_value(config, "target", "model", default="")),
        help="Target model deployment name; defaults to evaluation.yaml",
    )
    parser.add_argument(
        "--judge-model",
        default=expand_environment(config_value(config, "judge", "model", default="")),
        help="Judge model deployment name; defaults to the target model",
    )
    parser.add_argument(
        "--dataset-name",
        default=str(config_value(config, "dataset", "name", default="model-evaluation")),
        help="Registered Foundry dataset name",
    )
    parser.add_argument(
        "--dataset-version",
        default=str(config_value(config, "dataset", "version", default="auto")),
        help="Registered Foundry dataset version; 'auto' creates a timestamped version",
    )
    parser.add_argument(
        "--timeout-seconds",
        type=int,
        default=int(config_value(config, "remote", "timeoutSeconds", default=3600)),
        help="Maximum remote wait time",
    )
    parser.add_argument(
        "--poll-seconds",
        type=int,
        default=int(config_value(config, "remote", "pollSeconds", default=5)),
        help="Remote status polling interval",
    )
    parser.add_argument(
        "--no-wait",
        action="store_true",
        default=bool(config_value(config, "remote", "noWait", default=False)),
        help="Submit a remote run without waiting",
    )
    parser.add_argument(
        "--no-tracing",
        action="store_true",
        default=not bool(config_value(config, "remote", "tracing", default=True)),
        help="Disable remote OpenTelemetry instrumentation",
    )
    parser.add_argument(
        "--capture-content",
        action="store_true",
        default=bool(config_value(config, "remote", "captureContent", default=False)),
        help="Include model input and output content in traces; review privacy requirements first",
    )
    parser.add_argument("--azd-result-file", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.timeout_seconds <= 0:
        parser.error("--timeout-seconds must be greater than zero")
    if args.poll_seconds <= 0:
        parser.error("--poll-seconds must be greater than zero")
    if args.remote and args.no_wait and bool(config_value(config, "qualityGate", "enforce", default=False)):
        parser.error("remote.noWait cannot be used when qualityGate.enforce is true")
    if args.capture_content and args.no_tracing:
        parser.error("--capture-content cannot be combined with --no-tracing")
    return args, config


def resolve_setting(explicit: str | None, *environment_names: str) -> str:
    if explicit and explicit.strip():
        return explicit.strip()
    for name in environment_names:
        value = os.getenv(name, "").strip()
        if value:
            return value
    joined = " or ".join(environment_names)
    raise RuntimeError(
        "Missing local evaluation configuration. "
        f"Copy .env.example to .env and set {joined}, or pass the corresponding command option."
    )


def load_dataset(path: Path, fields: dict[str, str]) -> list[EvaluationRecord]:
    query_field = fields.get("query", "query")
    ground_truth_field = fields.get("groundTruth", "ground_truth")
    records: list[EvaluationRecord] = []
    with path.open("r", encoding="utf-8") as dataset_file:
        for line_number, raw_line in enumerate(dataset_file, start=1):
            line = raw_line.strip()
            if not line:
                continue
            try:
                item = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"{path}:{line_number}: invalid JSON: {exc.msg}") from exc
            if not isinstance(item, dict):
                raise ValueError(f"{path}:{line_number}: each JSONL row must be an object")
            query = item.get(query_field)
            ground_truth = item.get(ground_truth_field)
            if not isinstance(query, str) or not query.strip():
                raise ValueError(
                    f"{path}:{line_number}: mapped query field {query_field!r} must be a non-empty string"
                )
            if not isinstance(ground_truth, str) or not ground_truth.strip():
                raise ValueError(
                    f"{path}:{line_number}: mapped ground-truth field "
                    f"{ground_truth_field!r} must be a non-empty string"
                )
            records.append(EvaluationRecord(query=query.strip(), ground_truth=ground_truth.strip()))
    if not records:
        raise ValueError(f"{path}: the dataset does not contain any evaluation rows")
    return records


def normalized_tokens(value: str) -> list[str]:
    return TOKEN_PATTERN.findall(value.casefold())


def exact_match(response: str, ground_truth: str) -> float:
    return float(normalized_tokens(response) == normalized_tokens(ground_truth))


def token_f1(response: str, ground_truth: str) -> float:
    response_tokens = normalized_tokens(response)
    truth_tokens = normalized_tokens(ground_truth)
    if not response_tokens or not truth_tokens:
        return float(response_tokens == truth_tokens)
    common = sum((Counter(response_tokens) & Counter(truth_tokens)).values())
    if common == 0:
        return 0.0
    precision = common / len(response_tokens)
    recall = common / len(truth_tokens)
    return 2 * precision * recall / (precision + recall)


def json_value(value: Any) -> Any:
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    if isinstance(value, dict):
        return {str(key): json_value(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [json_value(item) for item in value]
    if is_dataclass(value):
        return json_value(asdict(value))
    model_dump = getattr(value, "model_dump", None)
    if callable(model_dump):
        return json_value(model_dump(mode="json", warnings=False))
    as_dict = getattr(value, "as_dict", None)
    if callable(as_dict):
        return json_value(as_dict())
    return str(value)


def write_result(output_dir: Path, stem: str, payload: dict[str, Any]) -> Path:
    output_dir.mkdir(parents=True, exist_ok=True)
    safe_stem = re.sub(r"[^a-zA-Z0-9._-]+", "-", stem).strip("-") or "evaluation"
    output_path = output_dir / f"{safe_stem}.json"
    with output_path.open("w", encoding="utf-8") as output_file:
        json.dump(json_value(payload), output_file, indent=2, ensure_ascii=True)
        output_file.write("\n")
    return output_path


def configure_remote_tracing(project_client: AIProjectClient, capture_content: bool) -> Any:
    os.environ["AZURE_EXPERIMENTAL_ENABLE_GENAI_TRACING"] = "true"
    os.environ["OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"] = (
        "true" if capture_content else "false"
    )

    from azure.ai.projects.telemetry import AIProjectInstrumentor
    from azure.monitor.opentelemetry import configure_azure_monitor
    from opentelemetry import trace

    try:
        connection_string = project_client.telemetry.get_application_insights_connection_string()
    except ResourceNotFoundError:
        connection_string = None
    if not connection_string:
        print(
            "Warning: The Foundry project is not connected to Application Insights; "
            "continuing without client trace export."
        )
        return None
    configure_azure_monitor(connection_string=connection_string)
    AIProjectInstrumentor().instrument(enable_content_recording=capture_content)
    return trace.get_tracer("azd.evaluation")


def local_response_request(model: str, query: str, system_prompt: str) -> dict[str, Any]:
    request: dict[str, Any] = {"model": model, "input": query}
    if system_prompt := system_prompt.strip():
        request["instructions"] = system_prompt
    return request


def run_local(
    args: argparse.Namespace,
    dataset: list[EvaluationRecord],
    endpoint: str,
    model: str,
    system_prompt: str,
) -> Path:
    rows: list[dict[str, Any]] = []
    with (
        DefaultAzureCredential() as credential,
        AIProjectClient(endpoint=endpoint, credential=credential) as project_client,
        project_client.get_openai_client() as openai_client,
    ):
        for index, record in enumerate(dataset, start=1):
            print(f"Evaluating local row {index}/{len(dataset)}...")
            response = openai_client.responses.create(
                **local_response_request(model, record.query, system_prompt)
            )
            output_text = response.output_text.strip()
            if not output_text:
                raise RuntimeError(f"The model returned no text for dataset row {index}.")
            rows.append(
                {
                    "query": record.query,
                    "ground_truth": record.ground_truth,
                    "response": output_text,
                    "response_id": response.id,
                    "metrics": {
                        "local_exact_match": exact_match(output_text, record.ground_truth),
                        "local_token_f1": token_f1(output_text, record.ground_truth),
                    },
                }
            )

    summary = {
        "local_exact_match": sum(row["metrics"]["local_exact_match"] for row in rows) / len(rows),
        "local_token_f1": sum(row["metrics"]["local_token_f1"] for row in rows) / len(rows),
    }
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_path = write_result(
        Path(args.output_dir),
        f"local-{timestamp}",
        {
            "mode": "local_smoke",
            "model": model,
            "dataset": str(Path(args.dataset)),
            "summary": summary,
            "rows": rows,
        },
    )
    print("Local smoke metrics (not equivalent to Foundry remote evaluators):")
    print(json.dumps(summary, indent=2))
    print(f"Local results: {output_path}")
    return output_path


def remote_testing_criteria(
    judge_model: str,
    fields: dict[str, str],
    evaluators: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    query = f"{{{{item.{fields.get('query', 'query')}}}}}"
    ground_truth = f"{{{{item.{fields.get('groundTruth', 'ground_truth')}}}}}"
    generated_response = "{{sample.output_text}}"
    judge_evaluators = {
        "builtin.coherence",
        "builtin.fluency",
        "builtin.relevance",
        "builtin.similarity",
    }

    criteria: list[dict[str, Any]] = []
    for evaluator in evaluators:
        evaluator_id = str(evaluator.get("id", "")).strip()
        name = str(evaluator.get("name", evaluator_id.removeprefix("builtin."))).strip()
        if not evaluator_id or not name:
            raise ValueError("Each evaluator requires non-empty 'id' and 'name' fields.")

        mapping = evaluator.get("dataMapping")
        if mapping is None:
            if evaluator_id == "builtin.f1_score":
                mapping = {"response": generated_response, "ground_truth": ground_truth}
            elif evaluator_id == "builtin.similarity":
                mapping = {
                    "query": query,
                    "response": generated_response,
                    "ground_truth": ground_truth,
                }
            else:
                mapping = {"query": query, "response": generated_response}
        if not isinstance(mapping, dict):
            raise ValueError(f"Evaluator {name!r} dataMapping must be an object.")

        criterion: dict[str, Any] = {
            "type": "azure_ai_evaluator",
            "name": name,
            "evaluator_name": evaluator_id,
            "data_mapping": mapping,
        }
        if evaluator_id in judge_evaluators:
            criterion["initialization_parameters"] = {"model": judge_model}
        criteria.append(criterion)
    return criteria


def target_input_messages(fields: dict[str, str], system_prompt: str) -> dict[str, Any]:
    template: list[dict[str, Any]] = []
    if system_prompt := system_prompt.strip():
        template.append(
            {
                "type": "message",
                "role": "system",
                "content": {"type": "input_text", "text": system_prompt},
            }
        )
    template.append(
        {
            "type": "message",
            "role": "user",
            "content": {
                "type": "input_text",
                "text": f"{{{{item.{fields.get('query', 'query')}}}}}",
            },
        }
    )
    return {"type": "template", "template": template}


def summarize_remote_results(
    output_items: list[Any],
    evaluators: list[dict[str, Any]],
    quality_gate: dict[str, Any],
    expected_item_count: int,
) -> tuple[dict[str, Any], dict[str, Any]]:
    configured = {str(item["name"]): item for item in evaluators}
    summary: dict[str, dict[str, Any]] = {
        name: {"passed": 0, "failed": 0, "errored": 0, "skipped": 0, "total": 0}
        for name in configured
    }

    for output_item in output_items:
        item = json_value(output_item)
        raw_results = item.get("results", []) if isinstance(item, dict) else []
        results_by_name: dict[str, dict[str, Any] | None] = {}
        if isinstance(raw_results, list):
            for raw_result in raw_results:
                result = json_value(raw_result)
                if not isinstance(result, dict):
                    continue
                name = str(result.get("name", ""))
                if name not in summary:
                    continue
                results_by_name[name] = result if name not in results_by_name else None

        for name, counts in summary.items():
            counts["total"] += 1
            result = results_by_name.get(name)
            if result is None:
                counts["errored"] += 1
                continue
            name = str(result.get("name", ""))
            status = str(result.get("status", "")).lower()
            if status in {"error", "errored", "failed"} and result.get("score") is None:
                counts["errored"] += 1
                continue
            if status == "skipped":
                counts["skipped"] += 1
                continue

            evaluator = configured[name]
            score = result.get("score")
            threshold = evaluator.get("threshold")
            direction = str(evaluator.get("direction", "increase")).lower()
            passed = result.get("passed")
            if isinstance(score, (int, float)) and isinstance(threshold, (int, float)):
                passed = score >= threshold if direction == "increase" else score <= threshold
            if passed is True:
                counts["passed"] += 1
            elif passed is False:
                counts["failed"] += 1
            else:
                counts["errored"] += 1

    missing_items = max(expected_item_count - len(output_items), 0)
    for counts in summary.values():
        counts["total"] += missing_items
        counts["errored"] += missing_items

    minimum_pass_rate = float(quality_gate.get("minPassRate", 0))
    maximum_error_rate = float(quality_gate.get("maxErrorRate", 1))
    gate_passed = True
    for name, counts in summary.items():
        counts["passRate"] = counts["passed"] / counts["total"] if counts["total"] else 0.0
        counts["errorRate"] = counts["errored"] / counts["total"] if counts["total"] else 1.0
        counts["threshold"] = configured[name].get("threshold")
        if counts["passRate"] < minimum_pass_rate or counts["errorRate"] > maximum_error_rate:
            gate_passed = False

    gate = {
        "passed": gate_passed,
        "enforced": bool(quality_gate.get("enforce", False)),
        "minPassRate": minimum_pass_rate,
        "maxErrorRate": maximum_error_rate,
    }
    return summary, gate


def format_remote_summary(summary: dict[str, Any], gate: dict[str, Any]) -> str:
    lines = ["Evaluation summary:"]
    for name, counts in summary.items():
        lines.append(
            f"  {name}: {counts['passed']}/{counts['total']} passed "
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


def register_dataset(
    project_client: AIProjectClient,
    *,
    name: str,
    requested_version: str,
    file_path: Path,
) -> Any:
    if requested_version.casefold() == "auto":
        version = datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S%f")
    else:
        version = requested_version
        try:
            existing = project_client.datasets.get(name, version)
        except ResourceNotFoundError:
            pass
        else:
            print(f"Reusing registered dataset: {existing.id}")
            return existing

    return project_client.datasets.upload_file(
        name=name,
        version=version,
        file_path=str(file_path),
    )


def write_azd_result_metadata(
    metadata_path: str | None,
    *,
    result_path: Path,
    report_url: str | None,
    summary_text: str = "",
) -> None:
    if not metadata_path:
        return
    path = Path(metadata_path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as metadata_file:
        json.dump(
            {
                "result_path": str(result_path.resolve()),
                "report_url": report_url or "",
                "summary_text": summary_text,
            },
            metadata_file,
            indent=2,
        )
        metadata_file.write("\n")


def run_remote(
    args: argparse.Namespace,
    config: dict[str, Any],
    endpoint: str,
    model: str,
    judge_model: str,
    expected_item_count: int,
) -> Path:
    dataset_path = Path(args.dataset).resolve()
    output_dir = Path(args.output_dir)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    fields = config_value(config, "dataset", "fields", default={})
    evaluators = config.get("evaluators", [])
    quality_gate = config.get("qualityGate", {})
    sampling = config_value(config, "target", "sampling", default={})
    system_prompt = str(config_value(config, "target", "systemPrompt", default=""))

    with (
        DefaultAzureCredential() as credential,
        AIProjectClient(endpoint=endpoint, credential=credential) as project_client,
    ):
        tracer = None
        if not args.no_tracing:
            tracer = configure_remote_tracing(project_client, args.capture_content)
        tracing_enabled = tracer is not None

        span_context = (
            tracer.start_as_current_span("model-evaluation.remote")
            if tracer is not None
            else nullcontext()
        )
        with span_context as span:
            if span is not None:
                span.set_attribute("gen_ai.operation.name", "evaluation")
                span.set_attribute("gen_ai.request.model", model)

            dataset = register_dataset(
                project_client,
                name=args.dataset_name,
                requested_version=args.dataset_version,
                file_path=dataset_path,
            )
            if not dataset.id:
                raise RuntimeError("Foundry registered the dataset without returning an ID.")
            print(f"Dataset registered: {dataset.id}")

            with project_client.get_openai_client() as openai_client:
                item_properties = {
                    str(fields.get("query", "query")): {"type": "string"},
                    str(fields.get("groundTruth", "ground_truth")): {"type": "string"},
                }
                if fields.get("context"):
                    item_properties[str(fields["context"])] = {"type": "string"}
                evaluation = openai_client.evals.create(
                    name=str(config.get("name", f"{args.dataset_name}-model-evaluation")),
                    data_source_config={
                        "type": "custom",
                        "item_schema": {
                            "type": "object",
                            "properties": item_properties,
                            "required": [
                                str(fields.get("query", "query")),
                                str(fields.get("groundTruth", "ground_truth")),
                            ],
                        },
                        "include_sample_schema": True,
                    },
                    testing_criteria=remote_testing_criteria(judge_model, fields, evaluators),
                )
                print(f"Evaluation created: {evaluation.id}")

                run = openai_client.evals.runs.create(
                    eval_id=evaluation.id,
                    name=f"{args.dataset_name}-{timestamp}",
                    metadata={
                        "source": "azd",
                        "dataset_name": args.dataset_name,
                        "profile": str(config.get("profile", "custom")),
                        "target_model": model,
                        "judge_model": judge_model,
                    },
                    data_source=TargetCompletionEvalRunDataSource(
                        type="azure_ai_target_completions",
                        source=SourceFileID(type="file_id", id=dataset.id),
                        input_messages=target_input_messages(fields, system_prompt),
                        target=AzureAIModelTargetParam(
                            type="azure_ai_model",
                            model=model,
                            sampling_params=ModelSamplingConfigParam(
                                top_p=float(sampling.get("topP", 1.0)),
                                max_completion_tokens=int(
                                    sampling.get("maxCompletionTokens", 2048)
                                ),
                            ),
                        ),
                    ),
                )
                print(f"Evaluation run started: {run.id}")

                if args.no_wait:
                    output_path = write_result(
                        output_dir,
                        f"remote-{run.id}",
                        {
                            "mode": "remote",
                            "tracing_enabled": tracing_enabled,
                            "content_capture_enabled": tracing_enabled and args.capture_content,
                            "evaluation_id": evaluation.id,
                            "run": run,
                        },
                    )
                    write_azd_result_metadata(
                        args.azd_result_file,
                        result_path=output_path,
                        report_url=getattr(run, "report_url", None),
                    )
                    print(f"Submission details: {output_path}")
                    return output_path

                deadline = time.monotonic() + args.timeout_seconds
                while run.status not in TERMINAL_RUN_STATUSES:
                    if time.monotonic() >= deadline:
                        raise TimeoutError(
                            f"Evaluation run {run.id} did not complete within {args.timeout_seconds} seconds."
                        )
                    time.sleep(args.poll_seconds)
                    run = openai_client.evals.runs.retrieve(
                        eval_id=evaluation.id,
                        run_id=run.id,
                    )
                    print(f"Evaluation status: {run.status}")

                output_items: list[Any] = []
                if run.status == "completed":
                    output_items = list(
                        openai_client.evals.runs.output_items.list(
                            eval_id=evaluation.id,
                            run_id=run.id,
                        )
                    )
                summary, gate = summarize_remote_results(
                    output_items,
                    evaluators,
                    quality_gate,
                    expected_item_count,
                )

                output_path = write_result(
                    output_dir,
                    f"remote-{run.id}",
                    {
                        "mode": "remote",
                        "tracing_enabled": tracing_enabled,
                        "content_capture_enabled": tracing_enabled and args.capture_content,
                        "evaluation_id": evaluation.id,
                        "run": run,
                        "output_items": output_items,
                        "summary": summary,
                        "quality_gate": gate,
                    },
                )
                summary_text = format_remote_summary(summary, gate)
                write_azd_result_metadata(
                    args.azd_result_file,
                    result_path=output_path,
                    report_url=run.report_url,
                    summary_text=summary_text,
                )
                print(f"\n{summary_text}")
                if run.report_url:
                    print(f"Foundry report: {run.report_url}")
                print(f"Remote results: {output_path}")
                if run.status != "completed":
                    raise RuntimeError(f"Evaluation run {run.id} finished with status {run.status}: {run.error}")
                if gate["enforced"] and not gate["passed"]:
                    raise RuntimeError(
                        "Evaluation quality gate failed. Review the summary and Foundry report."
                    )
                return output_path


def main() -> int:
    load_dotenv()
    args, config = parse_args()
    endpoint = resolve_setting(args.endpoint, "FOUNDRY_PROJECT_ENDPOINT", "AZURE_AI_PROJECT_ENDPOINT")
    model = resolve_setting(args.model, "FOUNDRY_MODEL_NAME", "AZURE_AI_MODEL_DEPLOYMENT_NAME")
    judge_model = (
        str(args.judge_model).strip()
        or os.getenv("FOUNDRY_JUDGE_MODEL_NAME", "").strip()
        or model
    )
    fields = config_value(config, "dataset", "fields", default={})
    system_prompt = str(config_value(config, "target", "systemPrompt", default=""))
    dataset = load_dataset(Path(args.dataset), fields)
    if args.remote:
        run_remote(args, config, endpoint, model, judge_model, len(dataset))
    else:
        args.no_wait = False
        run_local(args, dataset, endpoint, model, system_prompt)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
