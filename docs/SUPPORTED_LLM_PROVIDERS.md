# Supported LLM Providers

OAT agents are powered by the OAT Agent Runtime, which supports a wide range of LLM providers through LangChain. You can use any provider you have credentials for.

## Model format

The `--model` flag accepts two formats:

```bash
# Provider-prefixed (explicit)
oat init <url> --model anthropic:claude-sonnet-4-6

# Bare model name (provider auto-detected from name)
oat init <url> --model claude-sonnet-4-6
```

Auto-detection works for well-known model name prefixes (`claude-*` → Anthropic, `gpt-*`/`o1`/`o3`/`o4` → OpenAI, `gemini-*` → Google). For other providers, use the `provider:model` format.

## Setting up API keys

OAT loads environment variables from multiple sources. Use whichever method you prefer:

**OAT's built-in `.env` file** (recommended — persists across sessions, no shell config needed):

```bash
mkdir -p ~/.oat
echo 'ANTHROPIC_API_KEY=sk-ant-...' >> ~/.oat/.env
```

**Per-repo override** (use a different provider or key for a specific project):

```bash
mkdir -p ~/.oat/repos/<repo-name>
echo 'OPENAI_API_KEY=sk-...' >> ~/.oat/repos/<repo-name>/.env
```

**Shell profile export** (OAT auto-sources `~/.zshrc` and `~/.bashrc`):

```bash
echo 'export ANTHROPIC_API_KEY=sk-ant-...' >> ~/.zshrc
```

Local models (e.g. Ollama) don't need an API key — see [Custom providers](#custom-providers-via-config-file) below.

## Built-in providers

These providers are supported out of the box. Set the corresponding environment variable to enable them.

| Provider | `--model` prefix | Environment Variable |
|---|---|---|
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` |
| OpenAI | `openai` | `OPENAI_API_KEY` |
| Google GenAI | `google_genai` | `GOOGLE_API_KEY` |
| Google VertexAI | `google_vertexai` | `GOOGLE_CLOUD_PROJECT` |
| Azure OpenAI | `azure_openai` | `AZURE_OPENAI_API_KEY` |
| DeepSeek | `deepseek` | `DEEPSEEK_API_KEY` |
| Fireworks | `fireworks` | `FIREWORKS_API_KEY` |
| Together | `together` | `TOGETHER_API_KEY` |
| Groq | `groq` | `GROQ_API_KEY` |
| Mistral | `mistralai` | `MISTRAL_API_KEY` |
| NVIDIA NIM | `nvidia` | `NVIDIA_API_KEY` |
| OpenRouter | `openrouter` | `OPENROUTER_API_KEY` |
| Perplexity | `perplexity` | `PPLX_API_KEY` |
| xAI (Grok) | `xai` | `XAI_API_KEY` |
| Cohere | `cohere` | `COHERE_API_KEY` |
| HuggingFace | `huggingface` | `HUGGINGFACEHUB_API_TOKEN` |
| IBM watsonx | `ibm` | `WATSONX_APIKEY` |

Any provider with an installed `langchain-*` package is also auto-discovered.

## Auto-detect priority

When no `--model` flag is provided, the CLI picks the first available provider in this order:

1. OpenAI (`OPENAI_API_KEY`)
2. Anthropic (`ANTHROPIC_API_KEY`)
3. Google GenAI (`GOOGLE_API_KEY`)
4. Google VertexAI (`GOOGLE_CLOUD_PROJECT`)

This is why `--model` is recommended — adding a new API key to your environment can silently change which model your agents use.

## Custom providers via config file

You can add providers not in the built-in list (e.g., Ollama, a self-hosted endpoint) by editing `~/.oat/config.toml`:

```toml
[models.providers.ollama]
models = ["llama3:70b", "qwen3:4b"]
class_path = "langchain_ollama:ChatOllama"

[models.providers.ollama.params]
base_url = "http://localhost:11434"
```

This registers `ollama` as a provider so you can use:

```bash
oat init <url> --model ollama:llama3:70b
```

The `class_path` field points to any `BaseChatModel` subclass. The `params` table passes extra kwargs to the constructor.

## Examples

```bash
# Anthropic
oat init <url> --model claude-sonnet-4-6
oat worker create "task" --model anthropic:claude-opus-4-6

# OpenAI
oat init <url> --model gpt-5.2

# Google
oat init <url> --model gemini-3.1-pro-preview

# DeepSeek (requires provider prefix — not auto-detected from name)
oat worker create "task" --model deepseek:deepseek-chat

# Groq
oat worker create "task" --model groq:llama-3.3-70b-versatile

# Mix providers: cheap model for repo default, powerful for specific workers
oat init <url> --model claude-sonnet-4-6
oat worker create "complex refactor" --model anthropic:claude-opus-4-6
```

## Model resolution order

When launching an agent, OAT resolves the model in priority order:

1. **Agent-level override** — set via `oat worker create --model <model>`
2. **Repository default** — set via `oat init --model <model>`
3. **Auto-detect** — CLI picks from available API keys (see priority above)
