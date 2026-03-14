package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/warpstreamlabs/bento/public/service"
)

const (
	llmFieldProvider     = "provider"
	llmFieldModel        = "model"
	llmFieldAPIKey       = "api_key"
	llmFieldBaseURL      = "base_url"
	llmFieldPrompt       = "prompt"
	llmFieldSystemPrompt = "system_prompt"
	llmFieldMCPServers   = "mcp_servers"
	llmFieldTimeout      = "timeout"

	mcpFieldType    = "type"
	mcpFieldCommand = "command"
	mcpFieldArgs    = "args"
	mcpFieldEnv     = "env"
	mcpFieldURL     = "url"
)

type mcpConfig struct {
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
}

func llmProcessorSpec() *service.ConfigSpec {
	return service.NewConfigSpec().
		Summary("Queries an LLM using the charm.land/fantasy library.").
		Description("Accepts a prompt and replaces the message contents with the fully generated non-streamed response. Supports external tools via the Model Context Protocol (MCP).").
		Fields(
			service.NewStringField(llmFieldProvider).
				Description("The LLM provider to use (e.g., 'openai', 'anthropic', 'openrouter', 'openai-compat').").
				Default("openai"),
			service.NewStringField(llmFieldModel).
				Description("The name of the model to query (e.g., 'gpt-4o', 'claude-3-5-sonnet-20241022', 'moonshotai/kimi-k2')."),
			service.NewStringField(llmFieldAPIKey).
				Description("The API key for the selected provider.").
				Secret().
				Default(""),
			service.NewStringField(llmFieldBaseURL).
				Description("An optional base URL for the provider (useful for 'openai-compat' endpoints).").
				Optional(),
			service.NewInterpolatedStringField(llmFieldPrompt).
				Description("The prompt to send to the model. This field supports interpolation functions."),
			service.NewInterpolatedStringField(llmFieldSystemPrompt).
				Description("An optional system prompt to set the behavior of the assistant.").
				Optional(),
			service.NewDurationField(llmFieldTimeout).
				Description("The maximum duration to wait for the LLM (and any tool calls) to complete.").
				Default("5m"),
			service.NewObjectListField(llmFieldMCPServers,
				service.NewStringField(mcpFieldType).Description("Type of connection: 'stdio', 'sse', or 'streamable'"),
				service.NewStringField(mcpFieldCommand).Description("Command to execute for stdio transport.").Optional(),
				service.NewStringListField(mcpFieldArgs).Description("Arguments for the command.").Optional(),
				service.NewStringMapField(mcpFieldEnv).Description("Environment variables for the command.").Optional(),
				service.NewStringField(mcpFieldURL).Description("URL for the SSE transport.").Optional(),
			).
				Description("A list of MCP servers to connect to and expose as tools to the LLM.").
				Optional(),
		)
}

func init() {
	err := service.RegisterProcessor("llm", llmProcessorSpec(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.Processor, error) {
			return newLLMProcessor(conf, mgr.Logger())
		})
	if err != nil {
		panic(err)
	}
}

type llmProcessor struct {
	provider     fantasy.Provider
	model        fantasy.LanguageModel // Cached language model
	modelID      string
	prompt       *service.InterpolatedString
	systemPrompt *service.InterpolatedString
	timeout      time.Duration
	logger       *service.Logger

	mcpConfigs     []mcpConfig
	mu             sync.Mutex
	mcpInitialized bool
	mcpClients     []*client.Client
	mcpTools       []fantasy.AgentTool
}

func newLLMProcessor(conf *service.ParsedConfig, logger *service.Logger) (*llmProcessor, error) {
	providerName, err := conf.FieldString(llmFieldProvider)
	if err != nil {
		return nil, err
	}

	modelID, err := conf.FieldString(llmFieldModel)
	if err != nil {
		return nil, err
	}

	apiKey, _ := conf.FieldString(llmFieldAPIKey)
	baseURL, _ := conf.FieldString(llmFieldBaseURL)

	prompt, err := conf.FieldInterpolatedString(llmFieldPrompt)
	if err != nil {
		return nil, err
	}

	timeout, err := conf.FieldDuration(llmFieldTimeout)
	if err != nil {
		return nil, err
	}

	var systemPrompt *service.InterpolatedString
	if conf.Contains(llmFieldSystemPrompt) {
		systemPrompt, _ = conf.FieldInterpolatedString(llmFieldSystemPrompt)
	}

	var mcpConfigs []mcpConfig
	if conf.Contains(llmFieldMCPServers) {
		servers, err := conf.FieldObjectList(llmFieldMCPServers)
		if err != nil {
			return nil, err
		}
		for _, s := range servers {
			typ, _ := s.FieldString(mcpFieldType)

			var cmd, urlStr string
			var args []string
			var env map[string]string

			if s.Contains(mcpFieldCommand) {
				cmd, _ = s.FieldString(mcpFieldCommand)
			}
			if s.Contains(mcpFieldArgs) {
				args, _ = s.FieldStringList(mcpFieldArgs)
			}
			if s.Contains(mcpFieldEnv) {
				env, _ = s.FieldStringMap(mcpFieldEnv)
			}
			if s.Contains(mcpFieldURL) {
				urlStr, _ = s.FieldString(mcpFieldURL)
			}

			mcpConfigs = append(mcpConfigs, mcpConfig{
				Type:    typ,
				Command: cmd,
				Args:    args,
				Env:     env,
				URL:     urlStr,
			})
		}
	}

	// Initialize the requested Fantasy provider
	provider, err := buildProvider(providerName, apiKey, baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize provider '%s': %w", providerName, err)
	}

	// Initialize the Language Model once to save resources
	model, err := provider.LanguageModel(context.Background(), modelID)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize language model: %w", err)
	}

	return &llmProcessor{
		provider:     provider,
		model:        model,
		modelID:      modelID,
		prompt:       prompt,
		systemPrompt: systemPrompt,
		timeout:      timeout,
		mcpConfigs:   mcpConfigs,
		logger:       logger,
	}, nil
}

func buildProvider(name, apiKey, baseURL string) (fantasy.Provider, error) {
	switch name {
	case "openai":
		var opts []openai.Option
		if apiKey != "" {
			opts = append(opts, openai.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, openai.WithBaseURL(baseURL))
		}
		return openai.New(opts...)

	case "anthropic":
		var opts []anthropic.Option
		if apiKey != "" {
			opts = append(opts, anthropic.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(baseURL))
		}
		return anthropic.New(opts...)

	case "openrouter":
		var opts []openrouter.Option
		if apiKey != "" {
			opts = append(opts, openrouter.WithAPIKey(apiKey))
		}
		return openrouter.New(opts...)

	case "openai-compat":
		var opts []openaicompat.Option
		if apiKey != "" {
			opts = append(opts, openaicompat.WithAPIKey(apiKey))
		}
		if baseURL != "" {
			opts = append(opts, openaicompat.WithBaseURL(baseURL))
		}
		return openaicompat.New(opts...)

	default:
		return nil, fmt.Errorf("unsupported provider: %s", name)
	}
}

// initMCP lazy-initializes MCP servers using the first message's Context.
// If it fails, it cleans up partial connections and returns the error.
func (p *llmProcessor) initMCP(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.mcpInitialized {
		return nil
	}

	var newClients []*client.Client
	var newTools []fantasy.AgentTool

	// Ensure cleanup if we fail halfway
	defer func() {
		if !p.mcpInitialized {
			for _, c := range newClients {
				_ = c.Close()
			}
		}
	}()

	for _, cfg := range p.mcpConfigs {
		var c *client.Client
		var err error

		switch cfg.Type {
		case "stdio":
			var env []string
			for k, v := range cfg.Env {
				env = append(env, fmt.Sprintf("%s=%s", k, v))
			}
			t := transport.NewStdio(cfg.Command, env, cfg.Args...)
			c = client.NewClient(t)
			err = c.Start(ctx)
		case "sse":
			t, errTransport := transport.NewSSE(cfg.URL)
			if errTransport != nil {
				return fmt.Errorf("failed to create SSE transport for %s: %v", cfg.URL, errTransport)
			}
			c = client.NewClient(t)
			err = c.Start(ctx)
		case "streamable":
			c, err = client.NewStreamableHttpClient(cfg.URL)
			if err == nil {
				err = c.Start(ctx)
			}
		default:
			return fmt.Errorf("unknown MCP server type: %s", cfg.Type)
		}

		if err != nil {
			return fmt.Errorf("failed to start MCP client for %s: %v", cfg.Type, err)
		}
		newClients = append(newClients, c)

		initReq := mcp.InitializeRequest{
			Params: mcp.InitializeParams{
				ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
				ClientInfo: mcp.Implementation{
					Name:    "bento-llm-plugin",
					Version: "1.0.0",
				},
				Capabilities: mcp.ClientCapabilities{},
			},
		}

		_, err = c.Initialize(ctx, initReq)
		if err != nil {
			return fmt.Errorf("failed to initialize MCP client: %v", err)
		}

		toolsReq := mcp.ListToolsRequest{}
		toolsRes, err := c.ListTools(ctx, toolsReq)
		if err != nil {
			return fmt.Errorf("failed to list tools: %v", err)
		}

		for _, t := range toolsRes.Tools {
			newTools = append(newTools, &mcpToolWrapper{
				mcpClient: c,
				toolDef:   t,
			})
		}
	}

	p.mcpClients = newClients
	p.mcpTools = newTools
	p.mcpInitialized = true
	return nil
}

func (p *llmProcessor) Process(ctx context.Context, msg *service.Message) (service.MessageBatch, error) {
	// Ensure the context is strictly bounded and cleaned up
	// This ensures neither the LLM nor external MCP tools can hang indefinitely
	execCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// Ensure MCP servers are initialized dynamically
	if err := p.initMCP(execCtx); err != nil {
		return nil, fmt.Errorf("failed to initialize MCP connections: %w", err)
	}

	// Resolve prompt interpolations for the current message
	promptStr, err := p.prompt.TryString(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to interpolate prompt: %w", err)
	}

	// Prepare fresh Agent options
	var agentOpts []fantasy.AgentOption
	if p.systemPrompt != nil {
		sysPromptStr, err := p.systemPrompt.TryString(msg)
		if err != nil {
			return nil, fmt.Errorf("failed to interpolate system prompt: %w", err)
		}
		if sysPromptStr != "" {
			agentOpts = append(agentOpts, fantasy.WithSystemPrompt(sysPromptStr))
		}
	}

	if len(p.mcpTools) > 0 {
		agentOpts = append(agentOpts, fantasy.WithTools(p.mcpTools...))
	}

	// Create a new stateless agent and fire the Generate request.
	// Since we strictly pass only the new 'Prompt' (and no history),
	// the LLM conversational context is guaranteed to be 100% clean per message.
	agent := fantasy.NewAgent(p.model, agentOpts...)
	result, err := agent.Generate(execCtx, fantasy.AgentCall{
		Prompt:   promptStr,
		Messages: nil, // explicitly ensure no history bleeding
	})

	if err != nil {
		p.logger.Errorf("LLM generation failed: %v", err)
		return nil, err
	}

	// Replace the message payload with the LLM response
	respText := result.Response.Content.Text()
	msg.SetBytes([]byte(respText))

	return []*service.Message{msg}, nil
}

func (p *llmProcessor) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.mcpClients {
		_ = c.Close()
	}
	return nil
}

// MCP Bridge: Tool Wrapper bridging MCP into Fantasy's Tool interface

type mcpToolWrapper struct {
	mcpClient *client.Client
	toolDef   mcp.Tool
}

func (m *mcpToolWrapper) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        m.toolDef.Name,
		Description: m.toolDef.Description,
		Parameters:  m.toolDef.InputSchema.Properties,
		Required:    m.toolDef.InputSchema.Required,
		Parallel:    false,
	}
}

func (m *mcpToolWrapper) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args map[string]any
	if params.Input != "" {
		if err := json.Unmarshal([]byte(params.Input), &args); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to parse tool arguments: %w", err)
		}
	} else {
		args = make(map[string]any)
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      m.toolDef.Name,
			Arguments: args,
		},
	}

	res, err := m.mcpClient.CallTool(ctx, req)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	var outStr string
	for _, content := range res.Content {
		outStr += mcp.GetTextFromContent(content) + "\n"
	}

	if res.IsError {
		return fantasy.NewTextErrorResponse(outStr), nil
	}

	return fantasy.NewTextResponse(outStr), nil
}

func (m *mcpToolWrapper) ProviderOptions() fantasy.ProviderOptions {
	return nil
}

func (m *mcpToolWrapper) SetProviderOptions(opts fantasy.ProviderOptions) {}
