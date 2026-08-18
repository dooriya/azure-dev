// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"azure.ai.evaluation/internal/version"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
)

const (
	foundryAPIVersion = "v1"
	foundryTokenScope = "https://ai.azure.com/.default" //nolint:gosec // Public OAuth resource scope, not a credential.
	foundryHostSuffix = ".services.ai.azure.com"
)

type foundryEvaluationClient struct {
	endpoint string
	pipeline runtime.Pipeline
}

func newFoundryEvaluationClient(
	endpoint string,
	credential azcore.TokenCredential,
) (*foundryEvaluationClient, error) {
	normalizedEndpoint, err := validateFoundryProjectEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	options := &policy.ClientOptions{
		Telemetry: policy.TelemetryOptions{
			ApplicationID: "azd-ext-azure-ai-evaluation",
		},
		Logging: policy.LogOptions{
			AllowedHeaders: []string{"X-Ms-Correlation-Request-Id", "X-Request-Id"},
			IncludeBody:    false,
		},
		PerCallPolicies: []policy.Policy{
			runtime.NewBearerTokenPolicy(credential, []string{foundryTokenScope}, nil),
		},
	}
	return &foundryEvaluationClient{
		endpoint: normalizedEndpoint,
		pipeline: runtime.NewPipeline(
			"azure-ai-evaluation",
			version.Version,
			runtime.PipelineOptions{},
			options,
		),
	}, nil
}

func newFoundryEvaluationClientWithPipeline(
	endpoint string,
	pipeline runtime.Pipeline,
) *foundryEvaluationClient {
	return &foundryEvaluationClient{endpoint: endpoint, pipeline: pipeline}
}

type foundryDataset struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

type foundryBlobCredential struct {
	SASUri string `json:"sasUri"`
}

type foundryBlobReference struct {
	BlobURI    string                 `json:"blobUri"`
	Credential *foundryBlobCredential `json:"credential"`
}

type foundryPendingUpload struct {
	BlobReference *foundryBlobReference `json:"blobReference"`
	Version       string                `json:"version"`
}

type foundryEvaluation struct {
	ID string `json:"id"`
}

type foundryDeployment struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	ModelName    string            `json:"modelName"`
	Capabilities map[string]string `json:"capabilities"`
}

type foundryEvaluationRun struct {
	ID        string         `json:"id"`
	EvalID    string         `json:"eval_id"`
	Status    string         `json:"status"`
	ReportURL string         `json:"report_url"`
	Error     any            `json:"error"`
	Document  map[string]any `json:"-"`
}

func (c *foundryEvaluationClient) getDataset(
	ctx context.Context,
	name string,
	datasetVersion string,
) (*foundryDataset, error) {
	path := fmt.Sprintf(
		"/datasets/%s/versions/%s",
		url.PathEscape(name),
		url.PathEscape(datasetVersion),
	)
	return doFoundryRequest[foundryDataset](
		c,
		ctx,
		http.MethodGet,
		path,
		map[string]string{"api-version": foundryAPIVersion},
		nil,
		"",
	)
}

func (c *foundryEvaluationClient) listDeployments(
	ctx context.Context,
) ([]foundryDeployment, error) {
	var response struct {
		Value []foundryDeployment `json:"value"`
	}
	content, err := c.doRequest(
		ctx,
		http.MethodGet,
		"/deployments",
		map[string]string{"api-version": foundryAPIVersion},
		nil,
		"",
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &response); err != nil {
		return nil, fmt.Errorf("parsing Foundry deployments: %w", err)
	}
	return response.Value, nil
}

func (c *foundryEvaluationClient) startPendingUpload(
	ctx context.Context,
	name string,
	datasetVersion string,
) (*foundryPendingUpload, error) {
	path := fmt.Sprintf(
		"/datasets/%s/versions/%s/startPendingUpload",
		url.PathEscape(name),
		url.PathEscape(datasetVersion),
	)
	return doFoundryRequest[foundryPendingUpload](
		c,
		ctx,
		http.MethodPost,
		path,
		map[string]string{"api-version": foundryAPIVersion},
		map[string]string{"pendingUploadType": "BlobReference"},
		"application/json",
	)
}

func (c *foundryEvaluationClient) uploadBlob(
	ctx context.Context,
	containerSASUri string,
	fileName string,
	content []byte,
) (string, error) {
	uploadURL, err := url.Parse(containerSASUri)
	if err != nil {
		return "", fmt.Errorf("parsing dataset upload URL: %w", sanitizedURLError(err))
	}
	uploadURL.Path = strings.TrimSuffix(uploadURL.Path, "/") + "/" + url.PathEscape(filepath.Base(fileName))

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL.String(), bytes.NewReader(content))
	if err != nil {
		return "", fmt.Errorf("creating dataset upload request: %w", sanitizedURLError(err))
	}
	request.Header.Set("x-ms-blob-type", "BlockBlob")
	request.Header.Set("Content-Type", "application/jsonl")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("uploading evaluation dataset: %w", sanitizedURLError(err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("uploading evaluation dataset: Blob Storage returned %s", response.Status)
	}

	uploadURL.RawQuery = ""
	return uploadURL.String(), nil
}

func sanitizedURLError(err error) error {
	if urlError, ok := errors.AsType[*url.Error](err); ok {
		return urlError.Err
	}
	return err
}

func validateFoundryProjectEndpoint(raw string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parsing Foundry project endpoint: %w", sanitizedURLError(err))
	}
	switch {
	case !strings.EqualFold(endpoint.Scheme, "https"):
		return "", fmt.Errorf("Foundry project endpoint must use HTTPS")
	case endpoint.User != nil:
		return "", fmt.Errorf("Foundry project endpoint must not contain user information")
	case endpoint.Port() != "":
		return "", fmt.Errorf("Foundry project endpoint must not contain a port")
	case endpoint.RawQuery != "" || endpoint.ForceQuery:
		return "", fmt.Errorf("Foundry project endpoint must not contain a query string")
	case endpoint.Fragment != "":
		return "", fmt.Errorf("Foundry project endpoint must not contain a fragment")
	}
	host := strings.ToLower(endpoint.Hostname())
	if !strings.HasSuffix(host, foundryHostSuffix) ||
		len(host) <= len(foundryHostSuffix) {
		return "", fmt.Errorf(
			"Foundry project endpoint host %q must end with %s",
			host,
			foundryHostSuffix,
		)
	}
	path := strings.TrimRight(endpoint.EscapedPath(), "/")
	const projectPathPrefix = "/api/projects/"
	if !strings.HasPrefix(path, projectPathPrefix) {
		return "", fmt.Errorf("Foundry project endpoint path must match /api/projects/<project>")
	}
	projectSegment := strings.TrimPrefix(path, projectPathPrefix)
	decodedProject, err := url.PathUnescape(projectSegment)
	if err != nil || decodedProject == "" || strings.Contains(decodedProject, "/") {
		return "", fmt.Errorf("Foundry project endpoint path must identify exactly one project")
	}
	return "https://" + host + path, nil
}

func (c *foundryEvaluationClient) finalizeDataset(
	ctx context.Context,
	name string,
	datasetVersion string,
	dataURI string,
) (*foundryDataset, error) {
	path := fmt.Sprintf(
		"/datasets/%s/versions/%s",
		url.PathEscape(name),
		url.PathEscape(datasetVersion),
	)
	return doFoundryRequest[foundryDataset](
		c,
		ctx,
		http.MethodPatch,
		path,
		map[string]string{"api-version": foundryAPIVersion},
		map[string]any{
			"dataUri": dataURI,
			"type":    "uri_file",
		},
		"application/merge-patch+json",
	)
}

func (c *foundryEvaluationClient) createEvaluation(
	ctx context.Context,
	request any,
) (*foundryEvaluation, error) {
	return doFoundryRequest[foundryEvaluation](
		c,
		ctx,
		http.MethodPost,
		"/openai/v1/evals",
		nil,
		request,
		"application/json",
	)
}

func (c *foundryEvaluationClient) createRun(
	ctx context.Context,
	evaluationID string,
	request any,
) (*foundryEvaluationRun, error) {
	path := fmt.Sprintf("/openai/v1/evals/%s/runs", url.PathEscape(evaluationID))
	content, err := c.doRequest(ctx, http.MethodPost, path, nil, request, "application/json")
	if err != nil {
		return nil, err
	}
	return decodeFoundryRun(content)
}

func (c *foundryEvaluationClient) getRun(
	ctx context.Context,
	evaluationID string,
	runID string,
) (*foundryEvaluationRun, error) {
	path := fmt.Sprintf(
		"/openai/v1/evals/%s/runs/%s",
		url.PathEscape(evaluationID),
		url.PathEscape(runID),
	)
	content, err := c.doRequest(ctx, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return nil, err
	}
	return decodeFoundryRun(content)
}

func (c *foundryEvaluationClient) listOutputItems(
	ctx context.Context,
	evaluationID string,
	runID string,
) ([]json.RawMessage, error) {
	path := fmt.Sprintf(
		"/openai/v1/evals/%s/runs/%s/output_items",
		url.PathEscape(evaluationID),
		url.PathEscape(runID),
	)
	var items []json.RawMessage
	after := ""
	for {
		query := map[string]string{"limit": "100"}
		if after != "" {
			query["after"] = after
		}
		var page struct {
			Data    []json.RawMessage `json:"data"`
			HasMore bool              `json:"has_more"`
			LastID  string            `json:"last_id"`
		}
		content, err := c.doRequest(ctx, http.MethodGet, path, query, nil, "")
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(content, &page); err != nil {
			return nil, fmt.Errorf("parsing evaluation output items: %w", err)
		}
		items = append(items, page.Data...)
		if !page.HasMore {
			return items, nil
		}
		if page.LastID == "" || page.LastID == after {
			return nil, fmt.Errorf("evaluation output pagination did not return a new cursor")
		}
		after = page.LastID
	}
}

func decodeFoundryRun(content []byte) (*foundryEvaluationRun, error) {
	var run foundryEvaluationRun
	if err := json.Unmarshal(content, &run); err != nil {
		return nil, fmt.Errorf("parsing evaluation run: %w", err)
	}
	if err := json.Unmarshal(content, &run.Document); err != nil {
		return nil, fmt.Errorf("parsing evaluation run document: %w", err)
	}
	return &run, nil
}

func (c *foundryEvaluationClient) doRequest(
	ctx context.Context,
	method string,
	path string,
	query map[string]string,
	body any,
	contentType string,
) ([]byte, error) {
	endpoint, err := url.Parse(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing Foundry project endpoint: %w", err)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + path
	values := endpoint.Query()
	for key, value := range query {
		values.Set(key, value)
	}
	endpoint.RawQuery = values.Encode()

	request, err := runtime.NewRequest(ctx, method, endpoint.String())
	if err != nil {
		return nil, fmt.Errorf("creating Foundry request: %w", err)
	}
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding Foundry request: %w", err)
		}
		if contentType == "" {
			contentType = "application/json"
		}
		if err := request.SetBody(streaming.NopCloser(bytes.NewReader(payload)), contentType); err != nil {
			return nil, fmt.Errorf("setting Foundry request body: %w", err)
		}
	}

	response, err := c.pipeline.Do(request)
	if err != nil {
		return nil, fmt.Errorf("sending Foundry request: %w", err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("reading Foundry response: %w", err)
	}
	if !runtime.HasStatusCode(
		response,
		http.StatusOK,
		http.StatusCreated,
		http.StatusAccepted,
		http.StatusNoContent,
	) {
		response.Body = io.NopCloser(bytes.NewReader(content))
		return nil, runtime.NewResponseError(response)
	}
	return content, nil
}

func doFoundryRequest[T any](
	client *foundryEvaluationClient,
	ctx context.Context,
	method string,
	path string,
	query map[string]string,
	body any,
	contentType string,
) (*T, error) {
	content, err := client.doRequest(ctx, method, path, query, body, contentType)
	if err != nil {
		return nil, err
	}
	var response T
	if len(content) > 0 {
		if err := json.Unmarshal(content, &response); err != nil {
			return nil, fmt.Errorf("parsing Foundry response: %w", err)
		}
	}
	return &response, nil
}
