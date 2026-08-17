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
  provisioning the scaffold because it creates Azure role assignments.
- For an existing project, the **Foundry User** role and an Application
  Insights connection when tracing remains enabled.

## Install Python dependencies

```shell
python -m venv .venv
```

Activate the virtual environment, then run:

```shell
python -m pip install -r requirements.txt
```

For local execution against an existing Foundry project, create `.env` from the
generated example and replace both placeholders:

```powershell
Copy-Item .env.example .env
```

## Provision a Foundry project

```shell
azd auth login
azd env new
azd ai evaluation provision --preview
azd ai evaluation provision
```

The Bicep template creates a Foundry account and project, a model deployment,
Log Analytics, Application Insights, and the project connections and role
assignments required for traces.

To change the default model before provisioning:

```shell
azd env set AZURE_AI_MODEL_NAME gpt-4.1-mini
azd env set AZURE_AI_MODEL_VERSION 2025-04-14
azd env set AZURE_AI_MODEL_DEPLOYMENT_NAME gpt-4-1-mini
azd env set AZURE_AI_MODEL_SKU GlobalStandard
azd env set AZURE_AI_MODEL_CAPACITY 10
```

## Use an existing Foundry project

Skip provisioning and configure an azd environment:

```shell
azd env new
azd env set FOUNDRY_PROJECT_ENDPOINT "https://<account>.services.ai.azure.com/api/projects/<project>"
azd env set FOUNDRY_MODEL_NAME "<deployment-name>"
```

## Run locally

For an existing project, populate `.env` from `.env.example`. After provisioning
with azd, you can instead export the active azd environment. Then run:

```powershell
azd env get-values > .env
python src/evaluate.py --local
```

Local results are written under `results/`.

## Run in Foundry

```shell
azd ai evaluation deploy
```

You can also use the standard lifecycle:

```shell
azd up
```

The remote run uploads `data/evaluation.jsonl`, creates a managed evaluation,
waits for completion, writes the run and output items under `results/`, and
prints the Foundry report URL.

`datasetVersion: auto` in `azure.yaml` creates a timestamped dataset version on
each deployment. Set an explicit version when you want deployments to reference
a fixed registered dataset.

Remote tracing is enabled by default. The provisioned Application Insights
connection makes server-side trace propagation available, while the Python
client always instruments submission and polling. Foundry controls whether
managed model-target spans are emitted. Prompt and response content capture is
disabled unless you explicitly pass `--capture-content`.

## Dataset shape

Each JSONL line requires:

```json
{"query":"A question for the model","ground_truth":"The expected answer"}
```

Add or replace rows before running an evaluation.
