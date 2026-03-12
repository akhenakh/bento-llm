# Bento LLM Plugin

A custom [Bento](https://bento.dev/) processor plugin that allows you to query Large Language Models (LLMs) directly within your data pipelines. 

Powered by [charm.land/fantasy](https://github.com/charmbracelet/fantasy), this plugin provides a unified interface to multiple AI providers. Furthermore, it supports the **Model Context Protocol (MCP)**, allowing your LLM agent to seamlessly access external tools and data sources autonomously.

## Purpose

Use this plugin to enrich, transform, or generate data streams dynamically using AI. You pass in a prompt (which supports Bento's Bloblang interpolation), the LLM processes it, and the resulting text replaces the message payload.

## Supported Providers

You can configure the `provider` field to use various AI backends. The currently supported providers are:

* `openai` - Official OpenAI API.
* `anthropic` - Official Anthropic API.
* `openrouter` - OpenRouter multi-model routing.
* `openai-compat` - Generic provider for any OpenAI-compatible endpoint (e.g., **Ollama**, vLLM, LM Studio, local models). Requires setting the `base_url`.

## Configuration Example

```yaml
pipeline:
  processors:
    - llm:
        provider: "openai"
        model: "gpt-4o-mini"
        api_key: "${OPENAI_API_KEY}"
        system_prompt: "You are a helpful data processing assistant."
        prompt: "Extract the names from this text: ${! content() }"
```

## Adding external tools via MCP

The Model Context Protocol (MCP) allows you to attach external tools to your LLM. The LLM will automatically figure out when and how to call these tools based on the prompt.

You can configure MCP servers under the `mcp_servers` list. The plugin supports two types of connections: `stdio` (local subprocesses), `http` (streamable HTTP), and `sse` (remote HTTP endpoints).

### STDIO (Local Scripts/Binaries)
Use this when your MCP server is a local script or executable.

```yaml
mcp_servers:
  - type: "stdio"
    command: "mcp-zim"
    args: 
      - "-z"
      - "/opt/zim/wikipedia.zim"
    env:
      "API_TOKEN": "${SECRET_TOKEN}"
```

### SSE (Remote HTTP Servers)
Use this when your MCP server is hosted remotely over Server-Sent Events.

```yaml
mcp_servers:
  - type: "sse"
    url: "http://api.internal.mycompany.com:8080/mcp/sse"
```

### Full Example with MCP

```yaml
pipeline:
  processors:
    - llm:
        provider: "openrouter"
        model: "openai/gpt-4o"
        api_key: "${OPENROUTER_API_KEY}"
        prompt: "Analyze the weather situation for ${! json('city') } and recommend clothes."
        mcp_servers:
          - type: "sse"
            url: "http://my-weather-mcp.local/sse"
```
