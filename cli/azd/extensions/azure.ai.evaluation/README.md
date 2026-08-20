# Microsoft Foundry Evaluation extension

The Evaluation extension scaffolds model evaluation projects that run locally
with Python or as managed evaluation jobs in Microsoft Foundry.

## Commands

```shell
azd ai evaluation init
azd provision --preview
azd provision
azd deploy
```

`init` creates:

- `azure.yaml` with an `azure.ai.evaluation` service.
- For `--new-project`, Bicep infrastructure for a Foundry project, model
  deployment, Application Insights, and Log Analytics.
- Readable Python modules for config-driven local model evaluation with the
  Azure AI Evaluation SDK.
- A starter JSONL dataset.
- `evaluation.yaml` for target/judge models, an optional target system prompt,
  field mappings, evaluators, thresholds, sampling, remote options, and quality
  gates.
- `.env.example` with reusable placeholders.
- For an existing project, a gitignored `.env` containing the selected endpoint
  and deployment so local Python works immediately.

Interactive init defaults to an existing Foundry project, matching
`azd ai agent init`. It selects a subscription, project, target model deployment,
and judge model deployment. The target generates responses to evaluate; the
judge scores those responses for AI-assisted evaluators. The judge picker
preselects the target so you can reuse it or choose a different deployment.
Init then creates an azd environment and authors an `azure.ai.project`
brownfield service. Use `--new-project` to scaffold greenfield Bicep instead.

Core azd owns provisioning and deployment. For an existing project,
`azd provision` reuses the project without creating RBAC assignments. During
`azd deploy` or `azd up`, the extension's `azure.ai.evaluation` service target
uses Foundry APIs directly to upload the dataset, create the evaluation
definition, start a managed run, and wait for its result. Python is not required
for `azd provision` or `azd deploy`. The deploy result includes a clickable
**Evaluation report** URL, the local JSON result path, per-evaluator pass rates,
and the quality gate result.

## Stable output contract

Each managed run writes a machine-readable contract to:

```text
<output.path>/azd-evaluation-output-<sanitized-service-name>-<service-hash>.json
```

The 12-character lowercase `service-hash` is derived from the original service
name, preventing collisions after sanitization. The default scaffold writes
`results/azd-evaluation-output-evaluation-efe77b201dc2.json`.
Consumers such as the Microsoft Foundry skill can use this file to discover the
run without parsing terminal output:

```json
{
  "schemaVersion": 1,
  "extensionVersion": "0.1.0-preview",
  "projectEndpoint": "https://account.services.ai.azure.com/api/projects/project",
  "targetDeployment": "target-model",
  "judgeDeployment": "judge-model",
  "datasetName": "sample-dataset",
  "datasetVersion": "20260818160000000000",
  "evaluationId": "eval_...",
  "runId": "evalrun_...",
  "status": "completed",
  "reportUrl": "https://ai.azure.com/...",
  "resultPath": "C:\\path\\to\\results\\remote-evalrun_....json"
}
```

Consumers must check `schemaVersion` before reading other fields. A no-wait
submission records its current Foundry status and may have an empty `reportUrl`.
`resultPath` points to detailed diagnostic output whose shape is not part of
this stable contract. See
[`evaluation-output.schema.json`](schemas/evaluation-output.schema.json).

The generated infrastructure connects Application Insights to the Foundry
project. Foundry controls emission of managed model-target spans. Existing
projects without Application Insights can still run evaluations, but managed
traces are not exported.

Provisioning creates Azure role assignments. The deploying identity therefore
needs Owner, or Contributor together with User Access Administrator, on the
target subscription or resource group only when `--new-project` is selected.

In AI-agent terminals, azd automatically enables no-prompt mode. Pass
`--no-prompt=false` to `azd provision` when a human wants the standard
subscription and location prompts.

See the generated project README for setup and usage details.

## References

- [Cloud evaluation with the Microsoft Foundry SDK](https://learn.microsoft.com/azure/foundry/how-to/develop/cloud-evaluation)
- [Set up tracing for AI agents](https://learn.microsoft.com/azure/foundry/observability/how-to/trace-agent-setup)
- [Extension development guidelines](../../docs/extensions/extensions-style-guide.md)
