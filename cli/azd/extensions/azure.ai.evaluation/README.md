# Microsoft Foundry Evaluation extension

The Evaluation extension scaffolds model evaluation projects that run locally
with Python or as managed evaluation jobs in Microsoft Foundry.

## Commands

```shell
azd ai evaluation init
azd ai evaluation provision --preview
azd ai evaluation provision
azd ai evaluation deploy
```

`init` creates:

- `azure.yaml` with an `azure.ai.evaluation` service.
- Bicep infrastructure for a Foundry project, model deployment, Application
  Insights, and Log Analytics.
- Python code for local and server-side model evaluation.
- A starter JSONL dataset.
- `.env.example` with the required local project endpoint and model settings.

`provision` delegates to the standard `azd provision` workflow. `deploy` runs
each `azure.ai.evaluation` service through the extension's service target. The
target runs the generated Python with `--remote`, uploads the dataset, creates
the evaluation definition, starts a managed run, and waits for its result.
Standard `azd deploy` and `azd up` use the same service target.

The generated infrastructure connects Application Insights to the Foundry
project. The remote Python flow also enables OpenTelemetry instrumentation.
Foundry controls emission of managed model-target spans, while the scaffold
always traces client submission and polling. Message content capture remains
disabled by default.

Provisioning creates Azure role assignments. The deploying identity therefore
needs Owner, or Contributor together with User Access Administrator, on the
target subscription or resource group.

See the generated project README for setup and usage details.

## References

- [Cloud evaluation with the Microsoft Foundry SDK](https://learn.microsoft.com/azure/foundry/how-to/develop/cloud-evaluation)
- [Set up tracing for AI agents](https://learn.microsoft.com/azure/foundry/observability/how-to/trace-agent-setup)
- [Extension development guidelines](../../docs/extensions/extensions-style-guide.md)
