#!/usr/bin/env python3
"""Run a model evaluation locally or as a managed Microsoft Foundry job."""

from __future__ import annotations

import argparse
from collections import Counter
from contextlib import nullcontext
from dataclasses import asdict, dataclass, is_dataclass
from datetime import datetime, timezone
import json
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


TERMINAL_RUN_STATUSES = {"completed", "failed", "canceled", "cancelled", "partial", "skipped"}
TOKEN_PATTERN = re.compile(r"\w+", re.UNICODE)


@dataclass(frozen=True)
class EvaluationRecord:
    """One model input and its expected answer."""

    query: str
    ground_truth: str


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Evaluate a deployed Foundry model locally or with a managed evaluation job."
    )
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--local", action="store_true", help="Run model calls and scoring on this machine (default)")
    mode.add_argument("--remote", action="store_true", help="Run model calls and evaluators as a Foundry job")
    parser.add_argument("--dataset", default="data/evaluation.jsonl", help="Input JSONL dataset")
    parser.add_argument("--output-dir", default="results", help="Directory for JSON results")
    parser.add_argument("--endpoint", help="Foundry project endpoint; defaults to FOUNDRY_PROJECT_ENDPOINT")
    parser.add_argument("--model", help="Model deployment name; defaults to FOUNDRY_MODEL_NAME")
    parser.add_argument("--dataset-name", default="model-evaluation", help="Registered Foundry dataset name")
    parser.add_argument(
        "--dataset-version",
        default="auto",
        help="Registered Foundry dataset version; 'auto' creates a timestamped version",
    )
    parser.add_argument("--timeout-seconds", type=int, default=3600, help="Maximum remote wait time")
    parser.add_argument("--poll-seconds", type=int, default=5, help="Remote status polling interval")
    parser.add_argument("--no-wait", action="store_true", help="Submit a remote run without waiting")
    parser.add_argument("--no-tracing", action="store_true", help="Disable remote OpenTelemetry instrumentation")
    parser.add_argument(
        "--capture-content",
        action="store_true",
        help="Include model input and output content in traces; review privacy requirements first",
    )
    args = parser.parse_args()
    if args.timeout_seconds <= 0:
        parser.error("--timeout-seconds must be greater than zero")
    if args.poll_seconds <= 0:
        parser.error("--poll-seconds must be greater than zero")
    if args.no_wait and not args.remote:
        parser.error("--no-wait requires --remote")
    if args.capture_content and args.no_tracing:
        parser.error("--capture-content cannot be combined with --no-tracing")
    return args


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


def load_dataset(path: Path) -> list[EvaluationRecord]:
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
            query = item.get("query")
            ground_truth = item.get("ground_truth")
            if not isinstance(query, str) or not query.strip():
                raise ValueError(f"{path}:{line_number}: 'query' must be a non-empty string")
            if not isinstance(ground_truth, str) or not ground_truth.strip():
                raise ValueError(f"{path}:{line_number}: 'ground_truth' must be a non-empty string")
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


def run_local(args: argparse.Namespace, dataset: list[EvaluationRecord], endpoint: str, model: str) -> Path:
    rows: list[dict[str, Any]] = []
    with (
        DefaultAzureCredential() as credential,
        AIProjectClient(endpoint=endpoint, credential=credential) as project_client,
        project_client.get_openai_client() as openai_client,
    ):
        for index, record in enumerate(dataset, start=1):
            print(f"Evaluating local row {index}/{len(dataset)}...")
            response = openai_client.responses.create(model=model, input=record.query)
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
                        "exact_match": exact_match(output_text, record.ground_truth),
                        "token_f1": token_f1(output_text, record.ground_truth),
                    },
                }
            )

    summary = {
        "exact_match": sum(row["metrics"]["exact_match"] for row in rows) / len(rows),
        "token_f1": sum(row["metrics"]["token_f1"] for row in rows) / len(rows),
    }
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_path = write_result(
        Path(args.output_dir),
        f"local-{timestamp}",
        {
            "mode": "local",
            "model": model,
            "dataset": str(Path(args.dataset)),
            "summary": summary,
            "rows": rows,
        },
    )
    print(json.dumps(summary, indent=2))
    print(f"Local results: {output_path}")
    return output_path


def remote_testing_criteria(model: str) -> list[dict[str, Any]]:
    generated_response = "{{sample.output_text}}"
    query = "{{item.query}}"
    ground_truth = "{{item.ground_truth}}"
    return [
        {
            "type": "azure_ai_evaluator",
            "name": "relevance",
            "evaluator_name": "builtin.relevance",
            "initialization_parameters": {"model": model},
            "data_mapping": {"query": query, "response": generated_response},
        },
        {
            "type": "azure_ai_evaluator",
            "name": "coherence",
            "evaluator_name": "builtin.coherence",
            "initialization_parameters": {"model": model},
            "data_mapping": {"query": query, "response": generated_response},
        },
        {
            "type": "azure_ai_evaluator",
            "name": "f1",
            "evaluator_name": "builtin.f1_score",
            "data_mapping": {"response": generated_response, "ground_truth": ground_truth},
        },
    ]


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


def run_remote(args: argparse.Namespace, endpoint: str, model: str) -> Path:
    dataset_path = Path(args.dataset).resolve()
    output_dir = Path(args.output_dir)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")

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
                evaluation = openai_client.evals.create(
                    name=f"{args.dataset_name}-model-evaluation",
                    data_source_config={
                        "type": "custom",
                        "item_schema": {
                            "type": "object",
                            "properties": {
                                "query": {"type": "string"},
                                "ground_truth": {"type": "string"},
                            },
                            "required": ["query", "ground_truth"],
                        },
                        "include_sample_schema": True,
                    },
                    testing_criteria=remote_testing_criteria(model),
                )
                print(f"Evaluation created: {evaluation.id}")

                run = openai_client.evals.runs.create(
                    eval_id=evaluation.id,
                    name=f"{args.dataset_name}-{timestamp}",
                    metadata={"source": "azd", "dataset_name": args.dataset_name},
                    data_source=TargetCompletionEvalRunDataSource(
                        type="azure_ai_target_completions",
                        source=SourceFileID(type="file_id", id=dataset.id),
                        input_messages={
                            "type": "template",
                            "template": [
                                {
                                    "type": "message",
                                    "role": "user",
                                    "content": {
                                        "type": "input_text",
                                        "text": "{{item.query}}",
                                    },
                                }
                            ],
                        },
                        target=AzureAIModelTargetParam(
                            type="azure_ai_model",
                            model=model,
                            sampling_params=ModelSamplingConfigParam(
                                top_p=1.0,
                                max_completion_tokens=2048,
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
                    },
                )
                if run.report_url:
                    print(f"Foundry report: {run.report_url}")
                print(f"Remote results: {output_path}")
                if run.status != "completed":
                    raise RuntimeError(f"Evaluation run {run.id} finished with status {run.status}: {run.error}")
                return output_path


def main() -> int:
    load_dotenv()
    args = parse_args()
    endpoint = resolve_setting(args.endpoint, "FOUNDRY_PROJECT_ENDPOINT", "AZURE_AI_PROJECT_ENDPOINT")
    model = resolve_setting(args.model, "FOUNDRY_MODEL_NAME", "AZURE_AI_MODEL_DEPLOYMENT_NAME")
    if args.remote:
        load_dataset(Path(args.dataset))
        run_remote(args, endpoint, model)
    else:
        dataset = load_dataset(Path(args.dataset))
        run_local(args, dataset, endpoint, model)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
