// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var evaluationEnvironmentReference = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type evaluationConfig struct {
	Version     int                     `yaml:"version"`
	Name        string                  `yaml:"name"`
	Profile     string                  `yaml:"profile"`
	Dataset     evaluationDatasetConfig `yaml:"dataset"`
	Target      evaluationTargetConfig  `yaml:"target"`
	Judge       evaluationJudgeConfig   `yaml:"judge"`
	Evaluators  []evaluationEvaluator   `yaml:"evaluators"`
	QualityGate evaluationQualityGate   `yaml:"qualityGate"`
	Remote      evaluationRemoteConfig  `yaml:"remote"`
	Output      evaluationOutputConfig  `yaml:"output"`
}

type evaluationDatasetConfig struct {
	Path    string                  `yaml:"path"`
	Format  string                  `yaml:"format"`
	Name    string                  `yaml:"name"`
	Version string                  `yaml:"version"`
	Fields  evaluationDatasetFields `yaml:"fields"`
}

type evaluationDatasetFields struct {
	Query       string `yaml:"query"`
	GroundTruth string `yaml:"groundTruth"`
	Context     string `yaml:"context"`
}

type evaluationTargetConfig struct {
	Model        string                   `yaml:"model"`
	SystemPrompt string                   `yaml:"systemPrompt"`
	Sampling     evaluationSamplingConfig `yaml:"sampling"`
}

type evaluationSamplingConfig struct {
	TopP                *float64 `yaml:"topP"`
	MaxCompletionTokens *int     `yaml:"maxCompletionTokens"`
}

type evaluationJudgeConfig struct {
	Model string `yaml:"model"`
}

type evaluationEvaluator struct {
	Name        string            `yaml:"name"`
	ID          string            `yaml:"id"`
	Threshold   *float64          `yaml:"threshold"`
	Direction   string            `yaml:"direction"`
	DataMapping map[string]string `yaml:"dataMapping"`
}

type evaluationQualityGate struct {
	Enforce      bool     `yaml:"enforce"`
	MinPassRate  *float64 `yaml:"minPassRate"`
	MaxErrorRate *float64 `yaml:"maxErrorRate"`
}

type evaluationRemoteConfig struct {
	TimeoutSeconds *int `yaml:"timeoutSeconds"`
	PollSeconds    *int `yaml:"pollSeconds"`
	NoWait         bool `yaml:"noWait"`
}

type evaluationOutputConfig struct {
	Path *string `yaml:"path"`
}

func loadEvaluationConfig(path string) (*evaluationConfig, error) {
	content, err := os.ReadFile(path) //nolint:gosec // The path is validated against the project root.
	if err != nil {
		return nil, fmt.Errorf("reading evaluation configuration: %w", err)
	}

	var config evaluationConfig
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("parsing evaluation configuration: %w", err)
	}
	config.applyDefaults()
	if err := config.validate(path); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *evaluationConfig) applyDefaults() {
	if c.Target.Sampling.TopP == nil {
		c.Target.Sampling.TopP = new(1.0)
	}
	if c.Target.Sampling.MaxCompletionTokens == nil {
		c.Target.Sampling.MaxCompletionTokens = new(2048)
	}
	if c.QualityGate.MinPassRate == nil {
		c.QualityGate.MinPassRate = new(0.0)
	}
	if c.QualityGate.MaxErrorRate == nil {
		c.QualityGate.MaxErrorRate = new(1.0)
	}
	if c.Remote.TimeoutSeconds == nil {
		c.Remote.TimeoutSeconds = new(3600)
	}
	if c.Remote.PollSeconds == nil {
		c.Remote.PollSeconds = new(5)
	}
	if c.Output.Path == nil {
		c.Output.Path = new("results")
	}
	for index := range c.Evaluators {
		if c.Evaluators[index].Direction == "" {
			c.Evaluators[index].Direction = "increase"
		}
		if c.Evaluators[index].Threshold == nil {
			c.Evaluators[index].Threshold = evaluatorDefaultThreshold(
				c.Evaluators[index].ID,
			)
		}
	}
}

func evaluatorDefaultThreshold(evaluatorID string) *float64 {
	switch evaluatorID {
	case "builtin.relevance", "builtin.coherence", "builtin.fluency", "builtin.similarity":
		return new(3.0)
	case "builtin.f1_score":
		return new(0.5)
	default:
		return nil
	}
}

func (c *evaluationConfig) validate(path string) error {
	configError := func(message string) error {
		return evaluationConfigError(fmt.Sprintf("%s: %s", path, message))
	}
	switch {
	case c.Version != 1:
		return configError("version must be 1")
	case strings.TrimSpace(c.Name) == "":
		return configError("name cannot be empty")
	case strings.TrimSpace(c.Dataset.Path) == "":
		return configError("dataset.path cannot be empty")
	case !strings.EqualFold(c.Dataset.Format, "jsonl"):
		return configError("dataset.format must be jsonl")
	case strings.TrimSpace(c.Dataset.Name) == "":
		return configError("dataset.name cannot be empty")
	case strings.TrimSpace(c.Dataset.Version) == "":
		return configError("dataset.version cannot be empty")
	case strings.TrimSpace(c.Dataset.Fields.Query) == "":
		return configError("dataset.fields.query cannot be empty")
	case strings.TrimSpace(c.Dataset.Fields.GroundTruth) == "":
		return configError("dataset.fields.groundTruth cannot be empty")
	case strings.TrimSpace(c.Target.Model) == "":
		return configError("target.model cannot be empty")
	case c.Target.Sampling.TopP == nil ||
		math.IsNaN(*c.Target.Sampling.TopP) ||
		math.IsInf(*c.Target.Sampling.TopP, 0) ||
		*c.Target.Sampling.TopP < 0 ||
		*c.Target.Sampling.TopP > 1:
		return configError("target.sampling.topP must be between 0 and 1")
	case c.Target.Sampling.MaxCompletionTokens == nil ||
		*c.Target.Sampling.MaxCompletionTokens <= 0:
		return configError("target.sampling.maxCompletionTokens must be greater than zero")
	case len(c.Evaluators) == 0:
		return configError("at least one evaluator is required")
	case c.QualityGate.MinPassRate == nil ||
		*c.QualityGate.MinPassRate < 0 ||
		*c.QualityGate.MinPassRate > 1:
		return configError("qualityGate.minPassRate must be between 0 and 1")
	case c.QualityGate.MaxErrorRate == nil ||
		*c.QualityGate.MaxErrorRate < 0 ||
		*c.QualityGate.MaxErrorRate > 1:
		return configError("qualityGate.maxErrorRate must be between 0 and 1")
	case c.Remote.TimeoutSeconds == nil || *c.Remote.TimeoutSeconds <= 0:
		return configError("remote.timeoutSeconds must be greater than zero")
	case c.Remote.PollSeconds == nil || *c.Remote.PollSeconds <= 0:
		return configError("remote.pollSeconds must be greater than zero")
	case c.Remote.NoWait && c.QualityGate.Enforce:
		return configError("remote.noWait cannot be true when qualityGate.enforce is true")
	case c.Output.Path == nil || strings.TrimSpace(*c.Output.Path) == "":
		return configError("output.path cannot be empty")
	}

	names := make(map[string]struct{}, len(c.Evaluators))
	for index, evaluator := range c.Evaluators {
		location := fmt.Sprintf("evaluators[%d]", index)
		switch {
		case strings.TrimSpace(evaluator.Name) == "":
			return configError(location + ".name cannot be empty")
		case strings.TrimSpace(evaluator.ID) == "":
			return configError(location + ".id cannot be empty")
		case evaluator.Direction != "increase" && evaluator.Direction != "decrease":
			return configError(location + ".direction must be increase or decrease")
		case evaluator.Threshold != nil &&
			(math.IsNaN(*evaluator.Threshold) || math.IsInf(*evaluator.Threshold, 0)):
			return configError(location + ".threshold must be finite")
		}
		if _, exists := names[evaluator.Name]; exists {
			return configError(fmt.Sprintf("evaluator name %q must be unique", evaluator.Name))
		}
		names[evaluator.Name] = struct{}{}
	}
	return nil
}

func (c *evaluationConfig) resolvePaths(configPath string) (datasetPath, outputPath string, err error) {
	configDir := filepath.Dir(configPath)
	datasetPath, err = joinProjectPath(configDir, c.Dataset.Path)
	if err != nil {
		return "", "", evaluationConfigError(fmt.Sprintf("evaluation dataset path is invalid: %s", err))
	}
	outputPath, err = joinProjectPath(configDir, *c.Output.Path)
	if err != nil {
		return "", "", evaluationConfigError(fmt.Sprintf("evaluation output path is invalid: %s", err))
	}
	return datasetPath, outputPath, nil
}

func expandEvaluationEnvironment(value string, environment []string) (string, error) {
	var missing string
	expanded := evaluationEnvironmentReference.ReplaceAllStringFunc(value, func(reference string) string {
		match := evaluationEnvironmentReference.FindStringSubmatch(reference)
		resolved := environmentValue(environment, match[1])
		if resolved == "" && missing == "" {
			missing = match[1]
		}
		return resolved
	})
	if missing != "" {
		return "", evaluationConfigError(fmt.Sprintf("environment variable %s is not configured", missing))
	}
	return strings.TrimSpace(expanded), nil
}
