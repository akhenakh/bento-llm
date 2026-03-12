package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/warpstreamlabs/bento/public/service"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func TestLLMProcessorWithMCP(t *testing.T) {
	// Setup Fake MCP Server (Weather Service)
	mcpServer := server.NewMCPServer("weather-service", "1.0.0")

	// Register a mock tool
	mcpServer.AddTool(mcp.NewTool("get_weather",
		mcp.WithDescription("Get the weather for a location"),
		mcp.WithString("location", mcp.Required(), mcp.Description("The city to get weather for")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		loc := req.GetString("location", "Unknown")
		// Return a simulated tool execution
		return mcp.NewToolResultText("It is sunny and 75F in " + loc), nil
	})

	// Serve the MCP server over SSE using httptest
	sseServer := server.NewSSEServer(mcpServer)
	mcpTs := httptest.NewServer(sseServer)
	defer mcpTs.Close()

	// By default, SSEServer mounts the SSE stream on "/sse"
	mcpURL := mcpTs.URL + "/sse"

	// Setup Fake LLM Server (OpenAI Compatible / Ollama)
	llmTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)

		var req map[string]any
		err := json.NewDecoder(r.Body).Decode(&req)
		require.NoError(t, err)

		messages := req["messages"].([]any)
		lastMsg := messages[len(messages)-1].(map[string]any)
		role := lastMsg["role"].(string)

		w.Header().Set("Content-Type", "application/json")

		// First pass: User asks a question -> LLM decides to use the tool
		if role == "user" {
			resp := map[string]any{
				"id":      "chatcmpl-123",
				"object":  "chat.completion",
				"created": 12345,
				"model":   req["model"],
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": nil,
							"tool_calls": []map[string]any{
								{
									"id":   "call_abc123",
									"type": "function",
									"function": map[string]any{
										"name":      "get_weather",
										"arguments": `{"location":"San Francisco"}`,
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Second pass: Fantasy sends the tool result back -> LLM gives final answer
		if role == "tool" {
			resp := map[string]any{
				"id":      "chatcmpl-456",
				"object":  "chat.completion",
				"created": 12346,
				"model":   req["model"],
				"choices": []map[string]any{
					{
						"index": 0,
						"message": map[string]any{
							"role":    "assistant",
							"content": "Based on the tool, it is sunny and 75F in San Francisco.",
						},
						"finish_reason": "stop",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		t.Fatalf("Unexpected role in mock LLM server: %s", role)
	}))
	defer llmTs.Close()

	//  Configure the Bento LLM Processor
	confStr := `
provider: "openai-compat"
model: "fake-ollama-model"
api_key: "fake-key"
base_url: "` + llmTs.URL + `/v1"
prompt: "What is the weather in San Francisco?"
system_prompt: "You are a weather bot."
mcp_servers:
  - type: "sse"
    url: "` + mcpURL + `"
`

	spec := llmProcessorSpec()
	parsedConf, err := spec.ParseYAML(confStr, nil)
	require.NoError(t, err)

	// Create our custom processor using MockResources (which provides a mock logger)
	proc, err := newLLMProcessor(parsedConf, service.MockResources().Logger())
	require.NoError(t, err)
	defer proc.Close(context.Background())

	// Run the Processor and Assert
	// Initial message simulates an empty payload triggering the generation
	msg := service.NewMessage([]byte(`{}`))

	// Execute the processor
	batch, err := proc.Process(context.Background(), msg)
	require.NoError(t, err)
	require.Len(t, batch, 1) // batch is a []*service.Message, we expect 1 message out

	// Verify the final payload matches the LLM's final response
	resBytes, err := batch[0].AsBytes()

	require.NoError(t, err)
	require.Equal(t, "Based on the tool, it is sunny and 75F in San Francisco.", string(resBytes))
}
