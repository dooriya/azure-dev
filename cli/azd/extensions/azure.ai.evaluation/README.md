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
- Python code for local and server-side model evaluation.
- A starter JSONL dataset.
- `.env.example` with reusable placeholders.
- For an existing project, a gitignored `.env` containing the selected endpoint
  and deployment so local Python works immediately.

Interactive init defaults to an existing Foundry project, matching
`azd ai agent init`. It selects a subscription, project, and deployed model,
creates an azd environment, and authors an `azure.ai.project` brownfield service.
Use `--new-project` to scaffold greenfield Bicep instead.

Core azd owns provisioning and deployment. For an existing project,
`azd provision` reuses the project without creating RBAC assignments. During
`azd deploy` or `azd up`, the extension's `azure.ai.evaluation` service target
runs the generated Python with `--remote`, uploads the dataset, creates the
evaluation definition, starts a managed run, and waits for its result. The
deploy result includes a clickable **Evaluation report** URL and the local JSON
result path.

The generated infrastructure connects Application Insights to the Foundry
project. The remote Python flow also enables OpenTelemetry instrumentation.
Foundry controls emission of managed model-target spans. Existing projects
without Application Insights continue evaluation with a warning and no client
trace export. Message content capture remains disabled by default.

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
