// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/azure/azure-dev/cli/azd/pkg/azdext"
)

const (
	evaluationResultStateSubmitted = "submitted"
	evaluationResultStateCompleted = "completed"
)

type managedEvaluationOptions struct {
	Config         *evaluationConfig
	DatasetPath    string
	OutputPath     string
	ResultMetadata string
	Environment    []string
	Progress       azdext.ProgressReporter
}

type evaluationDataset struct {
	Content []byte
	Items   []map[string]any
}

type evaluationCriterionResult struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Score  *float64 `json:"score"`
	Passed *bool    `json:"passed"`
}

type evaluationOutputItem struct {
	Results []evaluationCriterionResult `json:"results"`
}

type evaluationSummaryCounts struct {
	Passed    int      `json:"passed"`
	Failed    int      `json:"failed"`
	Errored   int      `json:"errored"`
	Skipped   int      `json:"skipped"`
	Total     int      `json:"total"`
	PassRate  float64  `json:"passRate"`
	ErrorRate float64  `json:"errorRate"`
	Threshold *float64 `json:"threshold,omitempty"`
}

type evaluationGateResult struct {
	Passed       bool    `json:"passed"`
	Enforced     bool    `json:"enforced"`
	MinPassRate  float64 `json:"minPassRate"`
	MaxErrorRate float64 `json:"maxErrorRate"`
}

func runManagedEvaluation(
	ctx context.Context,
	options managedEvaluationOptions,
) (*evaluationRunResult, error) {
	endpoint := environmentValue(
		options.Environment,
		"FOUNDRY_PROJECT_ENDPOINT",
		"AZURE_AI_PROJECT_ENDPOINT",
	)
	if endpoint == "" {
		return nil, &azdext.LocalError{
			Message:    "Foundry project endpoint is not configured.",
			Code:       "evaluation_project_endpoint_missing",
			Category:   azdext.LocalErrorCategoryDependency,
			Suggestion: "Run 'azd provision', or set FOUNDRY_PROJECT_ENDPOINT in the active azd environment.",
		}
	}
	targetModel, err := expandEvaluationEnvironment(options.Config.Target.Model, options.Environment)
	if err != nil {
		return nil, err
	}
	if targetModel == "" {
		return nil, evaluationConfigError("target.model resolved to an empty value")
	}
	judgeModel := targetModel
	if strings.TrimSpace(options.Config.Judge.Model) != "" {
		judgeModel, err = expandEvaluationEnvironment(options.Config.Judge.Model, options.Environment)
		if err != nil {
			return nil, err
		}
	}

	tenantID := environmentValue(options.Environment, "AZURE_TENANT_ID")
	credential, err := newEvaluationCredential(tenantID)
	if err != nil {
		return nil, fmt.Errorf("creating Foundry credential: %w", err)
	}
	client, err := newFoundryEvaluationClient(endpoint, credential)
	if err != nil {
		return nil, &azdext.LocalError{
			Message:  fmt.Sprintf("Foundry project endpoint is invalid: %s", err),
			Code:     "evaluation_project_endpoint_invalid",
			Category: azdext.LocalErrorCategoryValidation,
			Suggestion: "Run 'azd provision' to refresh the endpoint, or configure a valid " +
				"https://<account>.services.ai.azure.com/api/projects/<project> endpoint.",
		}
	}
	return executeManagedEvaluation(ctx, client, options, targetModel, judgeModel)
}

func executeManagedEvaluation(
	ctx context.Context,
	client *foundryEvaluationClient,
	options managedEvaluationOptions,
	targetModel string,
	judgeModel string,
) (*evaluationRunResult, error) {
	dataset, err := loadEvaluationDataset(options.DatasetPath, options.Config.Dataset.Fields)
	if err != nil {
		return nil, err
	}
	criteria, err := buildEvaluationCriteria(
		options.Config.Evaluators,
		options.Config.Dataset.Fields,
		judgeModel,
	)
	if err != nil {
		return nil, err
	}

	registeredDataset, err := registerEvaluationDataset(
		ctx,
		client,
		options.Config.Dataset,
		options.DatasetPath,
		dataset.Content,
	)
	if err != nil {
		return nil, fmt.Errorf("registering evaluation dataset: %w", err)
	}

	itemProperties := map[string]any{
		options.Config.Dataset.Fields.Query:       map[string]string{"type": "string"},
		options.Config.Dataset.Fields.GroundTruth: map[string]string{"type": "string"},
	}
	if options.Config.Dataset.Fields.Context != "" {
		itemProperties[options.Config.Dataset.Fields.Context] = map[string]string{"type": "string"}
	}
	evaluation, err := client.createEvaluation(ctx, map[string]any{
		"name": options.Config.Name,
		"data_source_config": map[string]any{
			"type": "custom",
			"item_schema": map[string]any{
				"type":       "object",
				"properties": itemProperties,
				"required": []string{
					options.Config.Dataset.Fields.Query,
					options.Config.Dataset.Fields.GroundTruth,
				},
			},
			"include_sample_schema": true,
		},
		"testing_criteria": criteria,
	})
	if err != nil {
		return nil, fmt.Errorf("creating Foundry evaluation: %w", err)
	}
	if evaluation.ID == "" {
		return nil, fmt.Errorf("Foundry created an evaluation without returning an ID")
	}

	now := time.Now().UTC()
	run, err := client.createRun(ctx, evaluation.ID, map[string]any{
		"name": fmt.Sprintf("%s-%s", options.Config.Dataset.Name, now.Format("20060102T150405Z")),
		"metadata": map[string]string{
			"source":       "azd",
			"dataset_name": options.Config.Dataset.Name,
			"profile":      defaultString(options.Config.Profile, "custom"),
			"target_model": targetModel,
			"judge_model":  judgeModel,
		},
		"data_source": map[string]any{
			"type": "azure_ai_target_completions",
			"source": map[string]string{
				"type": "file_id",
				"id":   registeredDataset.ID,
			},
			"input_messages": map[string]any{
				"type": "template",
				"template": []map[string]any{
					{
						"type": "message",
						"role": "user",
						"content": map[string]string{
							"type": "input_text",
							"text": fmt.Sprintf(
								"{{item.%s}}",
								options.Config.Dataset.Fields.Query,
							),
						},
					},
				},
			},
			"target": map[string]any{
				"type":  "azure_ai_model",
				"model": targetModel,
				"sampling_params": map[string]any{
					"top_p":                 *options.Config.Target.Sampling.TopP,
					"max_completion_tokens": *options.Config.Target.Sampling.MaxCompletionTokens,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("starting Foundry evaluation run: %w", err)
	}
	if run.ID == "" {
		return nil, fmt.Errorf("Foundry started an evaluation run without returning an ID")
	}

	if options.Config.Remote.NoWait {
		return writeManagedEvaluationResult(
			options,
			evaluation.ID,
			registeredDataset.ID,
			run,
			nil,
			nil,
			nil,
			evaluationResultStateSubmitted,
		)
	}

	pollContext, cancelPolling := context.WithTimeout(
		ctx,
		time.Duration(*options.Config.Remote.TimeoutSeconds)*time.Second,
	)
	defer cancelPolling()
	for !isTerminalEvaluationStatus(run.Status) {
		if options.Progress != nil {
			options.Progress("Managed evaluation status: " + defaultString(run.Status, "queued"))
		}
		select {
		case <-pollContext.Done():
			return nil, evaluationPollingError(pollContext.Err(), run.ID, *options.Config.Remote.TimeoutSeconds)
		case <-time.After(time.Duration(*options.Config.Remote.PollSeconds) * time.Second):
		}
		run, err = client.getRun(pollContext, evaluation.ID, run.ID)
		if err != nil {
			if pollContext.Err() != nil {
				return nil, evaluationPollingError(
					pollContext.Err(),
					run.ID,
					*options.Config.Remote.TimeoutSeconds,
				)
			}
			return nil, fmt.Errorf("polling Foundry evaluation run: %w", err)
		}
	}

	var rawOutputItems []json.RawMessage
	if run.Status == "completed" {
		rawOutputItems, err = client.listOutputItems(ctx, evaluation.ID, run.ID)
		if err != nil {
			return nil, fmt.Errorf("downloading evaluation output items: %w", err)
		}
	}
	summary, gate, err := summarizeEvaluationResults(
		rawOutputItems,
		options.Config.Evaluators,
		options.Config.QualityGate,
		len(dataset.Items),
	)
	if err != nil {
		return nil, err
	}
	result, err := writeManagedEvaluationResult(
		options,
		evaluation.ID,
		registeredDataset.ID,
		run,
		rawOutputItems,
		summary,
		gate,
		evaluationResultStateCompleted,
	)
	if err != nil {
		return nil, err
	}
	if run.Status != "completed" {
		return result, fmt.Errorf(
			"evaluation run %s finished with status %s: %v",
			run.ID,
			run.Status,
			run.Error,
		)
	}
	if gate.Enforced && !gate.Passed {
		return result, fmt.Errorf("evaluation quality gate failed; review the summary and Foundry report")
	}
	return result, nil
}

func loadEvaluationDataset(
	path string,
	fields evaluationDatasetFields,
) (*evaluationDataset, error) {
	content, err := os.ReadFile(path) //nolint:gosec // Path is constrained to the evaluation project.
	if err != nil {
		return nil, fmt.Errorf("reading evaluation dataset: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var items []map[string]any
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, evaluationConfigError(
				fmt.Sprintf("%s:%d contains invalid JSON: %s", path, lineNumber, err),
			)
		}
		for label, field := range map[string]string{
			"query":        fields.Query,
			"ground truth": fields.GroundTruth,
		} {
			value, ok := item[field].(string)
			if !ok || strings.TrimSpace(value) == "" {
				return nil, evaluationConfigError(
					fmt.Sprintf(
						"%s:%d mapped %s field %q must be a non-empty string",
						path,
						lineNumber,
						label,
						field,
					),
				)
			}
		}
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading evaluation dataset: %w", err)
	}
	if len(items) == 0 {
		return nil, evaluationConfigError(fmt.Sprintf("%s contains no evaluation rows", path))
	}
	return &evaluationDataset{Content: content, Items: items}, nil
}

func registerEvaluationDataset(
	ctx context.Context,
	client *foundryEvaluationClient,
	config evaluationDatasetConfig,
	datasetPath string,
	content []byte,
) (*foundryDataset, error) {
	datasetVersion := config.Version
	if strings.EqualFold(datasetVersion, "auto") {
		now := time.Now().UTC()
		datasetVersion = now.Format("20060102150405") + fmt.Sprintf("%06d", now.Nanosecond()/1000)
	} else {
		dataset, err := client.getDataset(ctx, config.Name, datasetVersion)
		if err == nil {
			if dataset.ID == "" {
				return nil, fmt.Errorf("existing dataset version did not return an ID")
			}
			return dataset, nil
		}
		if responseError, ok := errors.AsType[*azcore.ResponseError](err); !ok ||
			responseError.StatusCode != 404 {
			return nil, err
		}
	}

	pending, err := client.startPendingUpload(ctx, config.Name, datasetVersion)
	if err != nil {
		return nil, err
	}
	if pending.Version != "" {
		datasetVersion = pending.Version
	}
	if pending.BlobReference == nil ||
		pending.BlobReference.Credential == nil ||
		pending.BlobReference.Credential.SASUri == "" {
		return nil, fmt.Errorf("Foundry did not return dataset upload credentials")
	}
	dataURI, err := client.uploadBlob(
		ctx,
		pending.BlobReference.Credential.SASUri,
		filepath.Base(datasetPath),
		content,
	)
	if err != nil {
		return nil, err
	}
	dataset, err := client.finalizeDataset(ctx, config.Name, datasetVersion, dataURI)
	if err != nil {
		return nil, err
	}
	if dataset.ID == "" {
		return nil, fmt.Errorf("Foundry registered the dataset without returning an ID")
	}
	return dataset, nil
}

func buildEvaluationCriteria(
	evaluators []evaluationEvaluator,
	fields evaluationDatasetFields,
	judgeModel string,
) ([]map[string]any, error) {
	query := fmt.Sprintf("{{item.%s}}", fields.Query)
	groundTruth := fmt.Sprintf("{{item.%s}}", fields.GroundTruth)
	response := "{{sample.output_text}}"
	criteria := make([]map[string]any, 0, len(evaluators))
	for _, evaluator := range evaluators {
		mapping := evaluator.DataMapping
		usesJudge := evaluatorUsesJudge(evaluator.ID)
		if len(mapping) == 0 {
			switch evaluator.ID {
			case "builtin.f1_score":
				mapping = map[string]string{
					"response":     response,
					"ground_truth": groundTruth,
				}
			case "builtin.similarity":
				mapping = map[string]string{
					"query":        query,
					"response":     response,
					"ground_truth": groundTruth,
				}
			case "builtin.coherence", "builtin.fluency", "builtin.relevance":
				mapping = map[string]string{"query": query, "response": response}
			default:
				return nil, evaluationConfigError(
					fmt.Sprintf(
						"evaluator %q requires an explicit dataMapping",
						evaluator.Name,
					),
				)
			}
		}
		criterion := map[string]any{
			"type":           "azure_ai_evaluator",
			"name":           evaluator.Name,
			"evaluator_name": evaluator.ID,
			"data_mapping":   mapping,
		}
		if usesJudge {
			criterion["initialization_parameters"] = map[string]string{"model": judgeModel}
		}
		criteria = append(criteria, criterion)
	}
	return criteria, nil
}

func summarizeEvaluationResults(
	rawItems []json.RawMessage,
	evaluators []evaluationEvaluator,
	qualityGate evaluationQualityGate,
	expectedItems int,
) (map[string]*evaluationSummaryCounts, *evaluationGateResult, error) {
	configured := make(map[string]evaluationEvaluator, len(evaluators))
	summary := make(map[string]*evaluationSummaryCounts, len(evaluators))
	for _, evaluator := range evaluators {
		configured[evaluator.Name] = evaluator
		summary[evaluator.Name] = &evaluationSummaryCounts{Threshold: evaluator.Threshold}
	}

	for _, rawItem := range rawItems {
		var item evaluationOutputItem
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, nil, fmt.Errorf("parsing evaluation output item: %w", err)
		}
		results := make(map[string]*evaluationCriterionResult, len(item.Results))
		duplicates := make(map[string]bool)
		for index := range item.Results {
			result := &item.Results[index]
			if _, exists := results[result.Name]; exists {
				duplicates[result.Name] = true
			}
			results[result.Name] = result
		}
		for name, counts := range summary {
			counts.Total++
			result, exists := results[name]
			if !exists || duplicates[name] {
				counts.Errored++
				continue
			}
			status := strings.ToLower(result.Status)
			if status == "skipped" {
				counts.Skipped++
				continue
			}
			if (status == "error" || status == "errored" || status == "failed") &&
				result.Score == nil {
				counts.Errored++
				continue
			}
			evaluator := configured[name]
			passed := result.Passed
			if result.Score != nil && evaluator.Threshold != nil {
				value := *result.Score >= *evaluator.Threshold
				if evaluator.Direction == "decrease" {
					value = *result.Score <= *evaluator.Threshold
				}
				passed = &value
			}
			if passed == nil {
				counts.Errored++
			} else if *passed {
				counts.Passed++
			} else {
				counts.Failed++
			}
		}
	}

	missingItems := max(expectedItems-len(rawItems), 0)
	gatePassed := true
	for _, counts := range summary {
		counts.Total += missingItems
		counts.Errored += missingItems
		if counts.Total == 0 {
			counts.ErrorRate = 1
		} else {
			counts.PassRate = float64(counts.Passed) / float64(counts.Total)
			counts.ErrorRate = float64(counts.Errored) / float64(counts.Total)
		}
		if counts.PassRate < *qualityGate.MinPassRate ||
			counts.ErrorRate > *qualityGate.MaxErrorRate ||
			math.IsNaN(counts.PassRate) ||
			math.IsNaN(counts.ErrorRate) {
			gatePassed = false
		}
	}
	return summary, &evaluationGateResult{
		Passed:       gatePassed,
		Enforced:     qualityGate.Enforce,
		MinPassRate:  *qualityGate.MinPassRate,
		MaxErrorRate: *qualityGate.MaxErrorRate,
	}, nil
}

func formatEvaluationSummary(
	summary map[string]*evaluationSummaryCounts,
	gate *evaluationGateResult,
	evaluators []evaluationEvaluator,
) string {
	lines := []string{"Evaluation summary:"}
	for _, evaluator := range evaluators {
		counts := summary[evaluator.Name]
		lines = append(lines, fmt.Sprintf(
			"  %s: %d/%d passed (%.0f%%), %d errors",
			evaluator.Name,
			counts.Passed,
			counts.Total,
			counts.PassRate*100,
			counts.Errored,
		))
	}
	status := "FAILED"
	if gate.Passed {
		status = "PASSED"
	}
	enforcement := "report only"
	if gate.Enforced {
		enforcement = "enforced"
	}
	lines = append(lines, fmt.Sprintf(
		"Quality gate: %s (%s; minimum pass rate %.0f%%, maximum error rate %.0f%%)",
		status,
		enforcement,
		gate.MinPassRate*100,
		gate.MaxErrorRate*100,
	))
	return strings.Join(lines, "\n")
}

func writeManagedEvaluationResult(
	options managedEvaluationOptions,
	evaluationID string,
	datasetID string,
	run *foundryEvaluationRun,
	outputItems []json.RawMessage,
	summary map[string]*evaluationSummaryCounts,
	gate *evaluationGateResult,
	state string,
) (*evaluationRunResult, error) {
	if err := os.MkdirAll(options.OutputPath, 0o750); err != nil {
		return nil, fmt.Errorf("creating evaluation results directory: %w", err)
	}
	resultPath := filepath.Join(
		options.OutputPath,
		"remote-"+safeEvaluationResultID(run.ID)+".json",
	)
	payload := map[string]any{
		"mode":          "remote",
		"dataset_id":    datasetID,
		"evaluation_id": evaluationID,
		"run":           run.Document,
	}
	if outputItems != nil {
		payload["output_items"] = outputItems
	}
	if summary != nil {
		payload["summary"] = summary
	}
	if gate != nil {
		payload["quality_gate"] = gate
	}
	if err := writeEvaluationJSON(resultPath, payload); err != nil {
		return nil, err
	}
	absoluteResultPath, err := filepath.Abs(resultPath)
	if err != nil {
		return nil, fmt.Errorf("resolving evaluation result path: %w", err)
	}
	reportURL, err := validateEvaluationReportURL(run.ReportURL)
	if err != nil {
		return nil, err
	}
	summaryText := ""
	if summary != nil && gate != nil {
		summaryText = formatEvaluationSummary(summary, gate, options.Config.Evaluators)
	}
	result := &evaluationRunResult{
		State:       state,
		ReportURL:   reportURL,
		ResultPath:  absoluteResultPath,
		SummaryText: summaryText,
	}
	if err := writeEvaluationJSON(options.ResultMetadata, result); err != nil {
		return nil, err
	}
	return result, nil
}

func writeEvaluationJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("creating evaluation output directory: %w", err)
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding evaluation result: %w", err)
	}
	content = append(content, '\n')
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("writing evaluation result: %w", err)
	}
	return nil
}

func isTerminalEvaluationStatus(status string) bool {
	switch strings.ToLower(status) {
	case "completed", "failed", "canceled", "cancelled", "partial", "skipped":
		return true
	default:
		return false
	}
}

func evaluationPollingError(err error, runID string, timeoutSeconds int) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf(
			"evaluation run %s did not complete within %d seconds",
			runID,
			timeoutSeconds,
		)
	}
	return err
}

func evaluatorUsesJudge(evaluatorID string) bool {
	switch evaluatorID {
	case "builtin.coherence", "builtin.fluency", "builtin.relevance", "builtin.similarity":
		return true
	default:
		return false
	}
}

func safeEvaluationResultID(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.',
			character == '_',
			character == '-':
			return character
		default:
			return '-'
		}
	}, value)
	value = strings.Trim(value, ".-")
	if value == "" {
		return "evaluation"
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
