// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFoundryEvaluationClientWorkflow(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet &&
			request.URL.Path == "/api/projects/sample/deployments":
			assert.Equal(t, foundryAPIVersion, request.URL.Query().Get("api-version"))
			writeTestJSON(t, writer, map[string]any{
				"value": []map[string]any{
					{
						"name":         "target",
						"type":         "ModelDeployment",
						"modelName":    "gpt-5",
						"capabilities": map[string]string{"chat_completion": "true"},
					},
				},
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/projects/sample/datasets/starter/versions/1/startPendingUpload":
			assert.Equal(t, foundryAPIVersion, request.URL.Query().Get("api-version"))
			var body map[string]string
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(t, "BlobReference", body["pendingUploadType"])
			writeTestJSON(t, writer, map[string]any{
				"blobReference": map[string]any{
					"blobUri": server.URL + "/blob/container",
					"credential": map[string]string{
						"sasUri": server.URL + "/blob/container?sig=test",
					},
				},
				"version": "1",
			})
		case request.Method == http.MethodPut &&
			request.URL.Path == "/blob/container/evaluation.jsonl":
			assert.Equal(t, "BlockBlob", request.Header.Get("x-ms-blob-type"))
			content, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			assert.JSONEq(t, `{"query":"hello","ground_truth":"world"}`, strings.TrimSpace(string(content)))
			writer.WriteHeader(http.StatusCreated)
		case request.Method == http.MethodPatch &&
			request.URL.Path == "/api/projects/sample/datasets/starter/versions/1":
			var body map[string]any
			require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
			assert.Equal(t, "uri_file", body["type"])
			assert.Equal(t, server.URL+"/blob/container/evaluation.jsonl", body["dataUri"])
			writeTestJSON(t, writer, map[string]string{
				"id":      "azureai://dataset/1",
				"name":    "starter",
				"version": "1",
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/projects/sample/openai/v1/evals":
			writeTestJSON(t, writer, map[string]string{"id": "eval_1"})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/projects/sample/openai/v1/evals/eval_1/runs":
			writeTestJSON(t, writer, map[string]string{
				"id":      "run_1",
				"eval_id": "eval_1",
				"status":  "queued",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/api/projects/sample/openai/v1/evals/eval_1/runs/run_1":
			writeTestJSON(t, writer, map[string]string{
				"id":         "run_1",
				"eval_id":    "eval_1",
				"status":     "completed",
				"report_url": "https://ai.azure.com/report",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/api/projects/sample/openai/v1/evals/eval_1/runs/run_1/output_items" &&
			request.URL.Query().Get("after") == "":
			writeTestJSON(t, writer, map[string]any{
				"data":     []map[string]string{{"id": "item_1"}},
				"has_more": true,
				"last_id":  "item_1",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/api/projects/sample/openai/v1/evals/eval_1/runs/run_1/output_items" &&
			request.URL.Query().Get("after") == "item_1":
			writeTestJSON(t, writer, map[string]any{
				"data":     []map[string]string{{"id": "item_2"}},
				"has_more": false,
				"last_id":  "item_2",
			})
		default:
			http.Error(writer, "unexpected request: "+request.Method+" "+request.URL.String(), http.StatusNotFound)
		}
	}))
	defer server.Close()

	pipeline := runtime.NewPipeline(
		"test",
		"v0.0.0",
		runtime.PipelineOptions{},
		&policy.ClientOptions{},
	)
	client := newFoundryEvaluationClientWithPipeline(server.URL+"/api/projects/sample", pipeline)
	deployments, err := client.listDeployments(t.Context())
	require.NoError(t, err)
	require.Len(t, deployments, 1)
	assert.Equal(t, "target", deployments[0].Name)
	pending, err := client.startPendingUpload(t.Context(), "starter", "1")
	require.NoError(t, err)
	require.NotNil(t, pending.BlobReference)
	require.NotNil(t, pending.BlobReference.Credential)
	dataURI, err := client.uploadBlob(
		t.Context(),
		pending.BlobReference.Credential.SASUri,
		"evaluation.jsonl",
		[]byte("{\"query\":\"hello\",\"ground_truth\":\"world\"}\n"),
	)
	require.NoError(t, err)
	dataset, err := client.finalizeDataset(t.Context(), "starter", "1", dataURI)
	require.NoError(t, err)
	assert.Equal(t, "azureai://dataset/1", dataset.ID)

	evaluation, err := client.createEvaluation(t.Context(), map[string]string{"name": "test"})
	require.NoError(t, err)
	run, err := client.createRun(t.Context(), evaluation.ID, map[string]string{"name": "test"})
	require.NoError(t, err)
	assert.Equal(t, "queued", run.Status)
	run, err = client.getRun(t.Context(), evaluation.ID, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status)
	items, err := client.listOutputItems(t.Context(), evaluation.ID, run.ID)
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestSanitizedURLErrorRemovesCredentialURL(t *testing.T) {
	err := sanitizedURLError(&url.Error{
		Op:  "Put",
		URL: "https://storage.example/container?sig=secret",
		Err: errors.New("connection failed"),
	})

	assert.EqualError(t, err, "connection failed")
	assert.NotContains(t, err.Error(), "secret")
}

func TestValidateFoundryProjectEndpoint(t *testing.T) {
	normalized, err := validateFoundryProjectEndpoint(
		" HTTPS://ACCOUNT.Services.AI.Azure.Com/api/projects/sample/ ",
	)
	require.NoError(t, err)
	assert.Equal(
		t,
		"https://account.services.ai.azure.com/api/projects/sample",
		normalized,
	)

	for _, endpoint := range []string{
		"http://account.services.ai.azure.com/api/projects/sample",
		"https://example.com/api/projects/sample",
		"https://services.ai.azure.com.evil.com/api/projects/sample",
		"https://user@account.services.ai.azure.com/api/projects/sample",
		"https://account.services.ai.azure.com:443/api/projects/sample",
		"https://account.services.ai.azure.com/api/projects/sample?token=value",
		"https://account.services.ai.azure.com/api/projects/sample#fragment",
		"https://account.services.ai.azure.com/api/projects",
		"https://account.services.ai.azure.com/api/projects/one/two",
	} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := validateFoundryProjectEndpoint(endpoint)
			require.Error(t, err)
		})
	}
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(writer).Encode(value))
}

func TestFoundryClientRejectsStalledPagination(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, writer, map[string]any{
			"data":     []any{},
			"has_more": true,
			"last_id":  "",
		})
	}))
	defer server.Close()
	pipeline := runtime.NewPipeline(
		"test",
		"v0.0.0",
		runtime.PipelineOptions{},
		&policy.ClientOptions{},
	)
	client := newFoundryEvaluationClientWithPipeline(server.URL, pipeline)

	_, err := client.listOutputItems(context.Background(), "eval", "run")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cursor")
}
