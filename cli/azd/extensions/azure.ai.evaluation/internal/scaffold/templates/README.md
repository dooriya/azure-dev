# Model evaluation project

This project runs the same model and dataset in two modes:

- **Local:** Python sends each query to the deployed model and computes exact
  match and token F1 scores on the local machine.
- **Remote:** Microsoft Foundry uploads the JSONL dataset, generates model
  responses server-side, and runs built-in relevance, coherence, and F1
  evaluators.

## Prerequisites

- Python 3.10 or later.
- Azure Developer CLI (`azd`) authenticated with `azd auth login`.
- Owner, or Contributor together with User Access Administrator, when
  provisioning a new project because that path creates Azure role assignments.
- For an existing project, the **Foundry User** role and an Application
  Insights connection when tracing remains enabled.

## Install Python dependencies

Python is needed only for local evaluation. `azd provision` and `azd deploy`
do not require Python or a virtual environment.

```shell
python -m venv .venv
```

Activate the virtual environment, then run:

```shell
python -m pip install -r requirements.txt
```

For an existing Foundry project, init writes selected values to the gitignored
`.env` file so local execution works immediately. `.env.example` remains a
reusable placeholder template and is never populated with project-specific
values.

## Foundry project lifecycle

Interactive `azd ai evaluation init` selects an existing Foundry project and
two model deployments by default. The target model generates responses to
evaluate. The judge model scores those responses for AI-assisted evaluators;
its picker preselects the target so you can reuse it or choose a different
deployment. In this mode, `azd provision` reuses the project and only refreshes
environment outputs:

```shell
azd provision
```

When init is run with `--new-project`, the generated Bicep creates a Foundry
account and project, model deployment, Log Analytics, Application Insights,
connections, and required role assignments:

```shell
azd provision --preview
azd provision
```

When running from an AI-agent terminal, azd automatically enables no-prompt
mode. Use `azd provision --no-prompt=false` to explicitly enable subscription
and location prompts.

To change the default model before provisioning:

```shell
azd env set AZURE_AI_MODEL_NAME gpt-4.1-mini
azd env set AZURE_AI_MODEL_VERSION 2025-04-14
azd env set AZURE_AI_MODEL_DEPLOYMENT_NAME gpt-4-1-mini
azd env set AZURE_AI_MODEL_SKU GlobalStandard
azd env set AZURE_AI_MODEL_CAPACITY 10
```

## Non-interactive existing project setup

Provide the project ARM ID and deployed model name:

```shell
azd ai evaluation init --no-prompt \
  --project-id "/subscriptions/<subscription>/resourceGroups/<group>/providers/Microsoft.CognitiveServices/accounts/<account>/projects/<project>" \
  --model-deployment "<deployment-name>"
```

## Run locally

For an existing project, init has already populated `.env`. After provisioning
a new project, export the active azd environment. Then run:

```powershell
azd env get-values > .env
python src/evaluate.py --local
```

Local results are written under `results/`.

## Customize datasets and evaluators

Edit `evaluation.yaml` to customize:

- `dataset.path`, registration name/version, and query/ground-truth field names.
- `target.model` and the separate `judge.model`.
- Evaluator IDs, names, thresholds, direction, and optional `dataMapping`.
- Target sampling parameters.
- Report-only or enforced `qualityGate` pass/error rates.
- Remote polling, wait behavior, and output location.

The starter profile uses relevance, coherence, and F1. F1 is a lexical baseline
and can fail correct paraphrases; use the Foundry report and AI-assisted
evaluators when assessing semantic quality. Custom catalog evaluators and
preview scenarios are intentionally not scaffolded by default.

## Run in Foundry

```shell
azd deploy
```

You can also use the standard lifecycle:

```shell
azd up
```

The remote run uploads `data/evaluation.jsonl`, creates a managed evaluation,
waits for completion, writes the run and output items under `results/`, and
returns a clickable **Evaluation report** URL plus the local JSON result path in
the `azd deploy` output.

`dataset.version: auto` in `evaluation.yaml` creates a timestamped dataset
version on each deployment. Set an explicit version when you want deployments
to reference a fixed registered dataset.

Managed traces are exported when the selected Foundry project has Application
Insights connected. Evaluation still runs when Application Insights is absent.

## Dataset shape

Each JSONL line requires:

```json
{"query":"A question for the model","ground_truth":"The expected answer"}
```

Add or replace rows before running an evaluation.
