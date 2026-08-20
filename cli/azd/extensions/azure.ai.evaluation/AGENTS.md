# Azure AI Evaluation Extension - Agent Instructions

Use this file together with `cli/azd/AGENTS.md`.

## Overview

`azure.ai.evaluation` scaffolds and deploys Microsoft Foundry model evaluation
projects. The extension owns the `azure.ai.evaluation` service target.

- `internal/cmd/` contains Cobra commands and the deploy-time service target.
- `internal/scaffold/` contains the embedded project template.
- `schemas/azure.ai.evaluation.json` describes the `azure.yaml` service body.

## Build and test

Run these commands from `cli/azd/extensions/azure.ai.evaluation`:

```shell
go build
go test ./...
```

Keep the generated Python local evaluator registry aligned with the Go managed
runner. Both modes use `evaluation.yaml` as their only evaluation-definition
source. Managed evaluations use the stable Foundry `/openai/v1/evals` contract;
local evaluations use the public `azure-ai-evaluation` SDK.
