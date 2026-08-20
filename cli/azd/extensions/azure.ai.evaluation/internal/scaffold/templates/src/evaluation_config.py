"""Strict, typed access to evaluation.yaml."""

from __future__ import annotations

from dataclasses import dataclass
import math
import os
from pathlib import Path
import re
from typing import Any
from urllib.parse import unquote, urlparse

import yaml


ENV_REFERENCE = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}")
FOUNDRY_HOST_SUFFIX = ".services.ai.azure.com"


@dataclass(frozen=True)
class DatasetFields:
    query: str
    ground_truth: str
    context: str | None = None


@dataclass(frozen=True)
class DatasetConfig:
    path: Path
    format: str
    name: str
    version: str
    fields: DatasetFields


@dataclass(frozen=True)
class SamplingConfig:
    top_p: float = 1.0
    max_completion_tokens: int = 2048


@dataclass(frozen=True)
class TargetConfig:
    model: str
    system_prompt: str = ""
    sampling: SamplingConfig = SamplingConfig()


@dataclass(frozen=True)
class JudgeConfig:
    model: str = ""


@dataclass(frozen=True)
class EvaluatorConfig:
    name: str
    id: str
    threshold: float | None
    direction: str
    data_mapping: dict[str, str] | None


@dataclass(frozen=True)
class QualityGateConfig:
    enforce: bool = False
    min_pass_rate: float = 0.0
    max_error_rate: float = 1.0


@dataclass(frozen=True)
class RemoteConfig:
    timeout_seconds: int = 3600
    poll_seconds: int = 5
    no_wait: bool = False


@dataclass(frozen=True)
class EvaluationConfig:
    source: Path
    version: int
    name: str
    profile: str
    dataset: DatasetConfig
    target: TargetConfig
    judge: JudgeConfig
    evaluators: tuple[EvaluatorConfig, ...]
    quality_gate: QualityGateConfig
    remote: RemoteConfig
    output_path: Path

    def target_model(self) -> str:
        return expand_environment(self.target.model, "target.model")

    def judge_model(self) -> str:
        configured = expand_environment(self.judge.model, "judge.model")
        return configured or self.target_model()


def load_evaluation_config(path: Path) -> EvaluationConfig:
    source = path.resolve()
    with source.open("r", encoding="utf-8") as config_file:
        value = yaml.safe_load(config_file)

    config = require_object(
        source,
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
    version = require_integer(source, "version", config["version"], exact=1)
    name = require_string(source, "name", config["name"], non_empty=True)
    profile = require_string(source, "profile", config.get("profile", "custom"))

    dataset = parse_dataset(source, config["dataset"])
    target = parse_target(source, config["target"])
    judge = parse_judge(source, config.get("judge", {}))
    evaluators = parse_evaluators(source, config["evaluators"])
    quality_gate = parse_quality_gate(source, config.get("qualityGate", {}))
    remote = parse_remote(source, config.get("remote", {}), quality_gate)
    output_path = parse_output_path(source, config.get("output", {}))

    return EvaluationConfig(
        source=source,
        version=version,
        name=name,
        profile=profile,
        dataset=dataset,
        target=target,
        judge=judge,
        evaluators=evaluators,
        quality_gate=quality_gate,
        remote=remote,
        output_path=output_path,
    )


def parse_dataset(source: Path, value: Any) -> DatasetConfig:
    dataset = require_object(
        source,
        "dataset",
        value,
        allowed={"path", "format", "name", "version", "fields"},
        required={"path", "format", "name", "version", "fields"},
    )
    dataset_format = require_string(
        source,
        "dataset.format",
        dataset["format"],
        non_empty=True,
    )
    if dataset_format != "jsonl":
        raise ValueError(f"{source}: only JSONL datasets are supported")

    fields_value = require_object(
        source,
        "dataset.fields",
        dataset["fields"],
        allowed={"query", "groundTruth", "context"},
        required={"query", "groundTruth"},
    )
    context = fields_value.get("context")
    fields = DatasetFields(
        query=require_string(
            source,
            "dataset.fields.query",
            fields_value["query"],
            non_empty=True,
        ),
        ground_truth=require_string(
            source,
            "dataset.fields.groundTruth",
            fields_value["groundTruth"],
            non_empty=True,
        ),
        context=(
            require_string(
                source,
                "dataset.fields.context",
                context,
                non_empty=True,
            )
            if context is not None
            else None
        ),
    )
    return DatasetConfig(
        path=resolve_project_path(
            source,
            require_string(source, "dataset.path", dataset["path"], non_empty=True),
        ),
        format=dataset_format,
        name=require_string(source, "dataset.name", dataset["name"], non_empty=True),
        version=require_string(
            source,
            "dataset.version",
            dataset["version"],
            non_empty=True,
        ),
        fields=fields,
    )


def parse_target(source: Path, value: Any) -> TargetConfig:
    target = require_object(
        source,
        "target",
        value,
        allowed={"model", "systemPrompt", "sampling"},
        required={"model"},
    )
    sampling_value = require_object(
        source,
        "target.sampling",
        target.get("sampling", {}),
        allowed={"topP", "maxCompletionTokens"},
    )
    return TargetConfig(
        model=require_string(source, "target.model", target["model"], non_empty=True),
        system_prompt=require_string(
            source,
            "target.systemPrompt",
            target.get("systemPrompt", ""),
        ),
        sampling=SamplingConfig(
            top_p=require_number(
                source,
                "target.sampling.topP",
                sampling_value.get("topP", 1.0),
                minimum=0,
                maximum=1,
            ),
            max_completion_tokens=require_integer(
                source,
                "target.sampling.maxCompletionTokens",
                sampling_value.get("maxCompletionTokens", 2048),
                minimum=1,
            ),
        ),
    )


def parse_judge(source: Path, value: Any) -> JudgeConfig:
    judge = require_object(source, "judge", value, allowed={"model"})
    return JudgeConfig(
        model=require_string(source, "judge.model", judge.get("model", ""))
    )


def parse_evaluators(source: Path, value: Any) -> tuple[EvaluatorConfig, ...]:
    if not isinstance(value, list) or not value:
        raise ValueError(f"{source}: at least one evaluator is required")

    evaluators: list[EvaluatorConfig] = []
    names: set[str] = set()
    for index, raw_evaluator in enumerate(value):
        location = f"evaluators[{index}]"
        evaluator = require_object(
            source,
            location,
            raw_evaluator,
            allowed={"name", "id", "threshold", "direction", "dataMapping"},
            required={"name", "id"},
        )
        name = require_string(
            source,
            f"{location}.name",
            evaluator["name"],
            non_empty=True,
        )
        if name in names:
            raise ValueError(f"{source}: evaluator name {name!r} must be unique")
        names.add(name)

        direction = require_string(
            source,
            f"{location}.direction",
            evaluator.get("direction", "increase"),
            non_empty=True,
        )
        if direction not in {"increase", "decrease"}:
            raise ValueError(
                f"{source}: {location}.direction must be increase or decrease"
            )

        threshold = evaluator.get("threshold")
        mapping = evaluator.get("dataMapping")
        if mapping is not None:
            mapping = require_object(
                source,
                f"{location}.dataMapping",
                mapping,
                allowed=set(mapping) if isinstance(mapping, dict) else set(),
            )
            for key, mapping_value in mapping.items():
                if not isinstance(key, str) or not isinstance(mapping_value, str):
                    raise ValueError(
                        f"{source}: {location}.dataMapping keys and values must be strings"
                    )

        evaluators.append(
            EvaluatorConfig(
                name=name,
                id=require_string(
                    source,
                    f"{location}.id",
                    evaluator["id"],
                    non_empty=True,
                ),
                threshold=(
                    require_number(source, f"{location}.threshold", threshold)
                    if threshold is not None
                    else None
                ),
                direction=direction,
                data_mapping=mapping,
            )
        )
    return tuple(evaluators)


def parse_quality_gate(source: Path, value: Any) -> QualityGateConfig:
    gate = require_object(
        source,
        "qualityGate",
        value,
        allowed={"enforce", "minPassRate", "maxErrorRate"},
    )
    enforce = gate.get("enforce", False)
    if not isinstance(enforce, bool):
        raise ValueError(f"{source}: qualityGate.enforce must be a boolean")
    return QualityGateConfig(
        enforce=enforce,
        min_pass_rate=require_number(
            source,
            "qualityGate.minPassRate",
            gate.get("minPassRate", 0.0),
            minimum=0,
            maximum=1,
        ),
        max_error_rate=require_number(
            source,
            "qualityGate.maxErrorRate",
            gate.get("maxErrorRate", 1.0),
            minimum=0,
            maximum=1,
        ),
    )


def parse_remote(
    source: Path,
    value: Any,
    quality_gate: QualityGateConfig,
) -> RemoteConfig:
    remote = require_object(
        source,
        "remote",
        value,
        allowed={"timeoutSeconds", "pollSeconds", "noWait"},
    )
    no_wait = remote.get("noWait", False)
    if not isinstance(no_wait, bool):
        raise ValueError(f"{source}: remote.noWait must be a boolean")
    if no_wait and quality_gate.enforce:
        raise ValueError(
            f"{source}: remote.noWait cannot be true when qualityGate.enforce is true"
        )
    return RemoteConfig(
        timeout_seconds=require_integer(
            source,
            "remote.timeoutSeconds",
            remote.get("timeoutSeconds", 3600),
            minimum=1,
        ),
        poll_seconds=require_integer(
            source,
            "remote.pollSeconds",
            remote.get("pollSeconds", 5),
            minimum=1,
        ),
        no_wait=no_wait,
    )


def parse_output_path(source: Path, value: Any) -> Path:
    output = require_object(source, "output", value, allowed={"path"})
    return resolve_project_path(
        source,
        require_string(
            source,
            "output.path",
            output.get("path", "results"),
            non_empty=True,
        ),
    )


def resolve_project_path(source: Path, configured: str) -> Path:
    path = Path(configured)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError(f"{source}: project path {configured!r} must be relative")
    root = source.parent.resolve()
    resolved = (root / path).resolve()
    if not resolved.is_relative_to(root):
        raise ValueError(f"{source}: project path {configured!r} escapes the project")
    return resolved


def expand_environment(value: str, location: str) -> str:
    missing: list[str] = []

    def replace(match: re.Match[str]) -> str:
        name = match.group(1)
        resolved = os.getenv(name, "")
        if not resolved:
            missing.append(name)
        return resolved

    expanded = ENV_REFERENCE.sub(replace, value).strip()
    if missing:
        raise ValueError(
            f"{location} references missing environment variable(s): "
            + ", ".join(sorted(set(missing)))
        )
    return expanded


def validate_project_endpoint(raw: str) -> str:
    endpoint = urlparse(raw.strip())
    if endpoint.scheme.lower() != "https":
        raise ValueError("Foundry project endpoint must use HTTPS")
    if endpoint.username or endpoint.password:
        raise ValueError("Foundry project endpoint must not contain user information")
    try:
        port = endpoint.port
    except ValueError as exc:
        raise ValueError("Foundry project endpoint contains an invalid port") from exc
    if port is not None:
        raise ValueError("Foundry project endpoint must not contain a port")
    if endpoint.query or endpoint.fragment:
        raise ValueError(
            "Foundry project endpoint must not contain a query string or fragment"
        )
    host = (endpoint.hostname or "").lower()
    if not host.endswith(FOUNDRY_HOST_SUFFIX) or len(host) <= len(
        FOUNDRY_HOST_SUFFIX
    ):
        raise ValueError(
            f"Foundry project endpoint host {host!r} must end with "
            f"{FOUNDRY_HOST_SUFFIX}"
        )
    path = endpoint.path.rstrip("/")
    prefix = "/api/projects/"
    if not path.startswith(prefix):
        raise ValueError(
            "Foundry project endpoint path must match /api/projects/<project>"
        )
    project = unquote(path.removeprefix(prefix))
    if not project or "/" in project:
        raise ValueError(
            "Foundry project endpoint path must identify exactly one project"
        )
    return f"https://{host}{path}"


def require_object(
    source: Path,
    location: str,
    value: Any,
    *,
    allowed: set[str],
    required: set[str] | None = None,
) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ValueError(f"{source}: {location} must be an object")
    unknown = sorted(str(key) for key in value if key not in allowed)
    if unknown:
        raise ValueError(
            f"{source}: {location} contains unsupported field(s): "
            + ", ".join(unknown)
        )
    missing = sorted(key for key in required or set() if key not in value)
    if missing:
        raise ValueError(
            f"{source}: {location} requires field(s): " + ", ".join(missing)
        )
    return value


def require_string(
    source: Path,
    location: str,
    value: Any,
    *,
    non_empty: bool = False,
) -> str:
    if not isinstance(value, str) or (non_empty and not value.strip()):
        qualifier = "a non-empty string" if non_empty else "a string"
        raise ValueError(f"{source}: {location} must be {qualifier}")
    return value


def require_number(
    source: Path,
    location: str,
    value: Any,
    *,
    minimum: float | None = None,
    maximum: float | None = None,
) -> float:
    if (
        not isinstance(value, (int, float))
        or isinstance(value, bool)
        or not math.isfinite(float(value))
    ):
        raise ValueError(f"{source}: {location} must be a finite number")
    number = float(value)
    if minimum is not None and number < minimum:
        raise ValueError(f"{source}: {location} must be at least {minimum:g}")
    if maximum is not None and number > maximum:
        raise ValueError(f"{source}: {location} must be at most {maximum:g}")
    return number


def require_integer(
    source: Path,
    location: str,
    value: Any,
    *,
    minimum: int | None = None,
    exact: int | None = None,
) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        raise ValueError(f"{source}: {location} must be an integer")
    if exact is not None and value != exact:
        raise ValueError(f"{source}: {location} must be {exact}")
    if minimum is not None and value < minimum:
        raise ValueError(f"{source}: {location} must be at least {minimum}")
    return value
