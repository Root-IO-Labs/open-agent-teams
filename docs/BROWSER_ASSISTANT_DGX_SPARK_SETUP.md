# OAT Browser Agent, Assistant, and DGX Spark Setup

This is a practical setup guide for getting OAT running with:

- the personal assistant in the Chrome side panel;
- the repo-scoped browser agent for web tasks;
- the internal NVIDIA DGX Spark box as an OpenAI-compatible model provider.

The goal is to get to a working smoke test first, then tune models after that.

## 1. Install OAT

Prerequisites:

- Go 1.24.2+
- Python 3.11+
- `uv`
- `git`
- GitHub CLI (`gh`)
- access to the relevant GitHub repos

Install OAT:

```bash
git clone https://github.com/Root-IO-Labs/open-agent-teams.git
cd open-agent-teams
./scripts/install.sh
```

Make sure the binaries are on your `PATH`:

```bash
which oat oat-agent
oat version
```

If `oat` is not found, add Go's bin directory to your shell profile:

```bash
echo 'export PATH="$PATH:$(go env GOPATH 2>/dev/null)/bin"' >> ~/.zshrc
source ~/.zshrc
```

Run the read-only setup check:

```bash
oat doctor
```

## 2. Start the OAT Daemon

OAT runs a background daemon that owns agent lifecycles, messages, logs, and browser-agent wiring.

```bash
oat start
oat status
```

If anything looks wrong, run:

```bash
oat doctor
oat daemon logs -f
```

## 3. Install the Browser Bridge and Chrome Extension

The assistant and browser agent both depend on the separate `oat-browser-agent` project:

- the Chrome extension provides the side panel and browser control surface;
- the Node bridge exposes Chrome tools to OAT over MCP;
- OAT auto-writes each browser-capable agent's `.oat/mcp.json` when the agent starts.

Install from the GitHub source repo:

```bash
git clone https://github.com/Root-IO-Labs/oat-browser-agent.git
cd oat-browser-agent
npm install
npm run build
npm run install-host
```

Then load the Chrome extension:

1. Open `chrome://extensions`.
2. Enable **Developer mode**.
3. Click **Load unpacked**.
4. Select the built extension directory from the repo, usually `extension/dist` or `extension/`.
5. Pin or open the `oat-browser-agent` extension side panel.

If the repo uses a different package manager or build output directory, follow the scripts in its `package.json` and select the generated unpacked-extension directory.

After install, OAT resolves the bridge in this order:

1. `OAT_BROWSER_AGENT_BRIDGE_PATH`, if set;
2. `oat-browser-agent` on `PATH`;
3. `~/.oat/oat-browser-agent/dist/bridge/index.js`.

For a development checkout, set the bridge path explicitly:

```bash
export OAT_BROWSER_AGENT_BRIDGE_PATH=/absolute/path/to/oat-browser-agent/dist/bridge/index.js
```

On macOS, the Chrome native-messaging host should also be installed. If browser tools load but every tool call fails with a trust/session-token error, reinstall the native host from the `oat-browser-agent` checkout and reload Chrome.

## 4. Start a Personal Assistant

Use the assistant for persistent side-panel chat: research, web browsing, lightweight help, and follow-up conversations.

```bash
oat assistant start
```

The default assistant name is `personal`. You can also start a named assistant:

```bash
oat assistant start work
```

Open Chrome, click the `oat-browser-agent` extension icon, and use the side-panel chat box.

Useful assistant commands:

```bash
oat assistant status
oat assistant logs personal --follow
oat assistant restart personal
oat assistant restart personal --fresh
oat assistant stop personal
```

Use `--fresh` only when you want to rotate away the current chat history and start clean.

## 5. Add a Repo-Scoped Browser Agent

Use the browser agent when you want an OAT repo team to control Chrome for a concrete web task: QA a page, inspect a dashboard, scrape a table, or navigate an authenticated web app.

Initialize or select a repo first:

```bash
oat init https://github.com/<org>/<repo> --model <model-id>
oat repo use <repo-name>
```

Then add the browser agent:

```bash
oat agent add browser-agent --repo <repo-name>
```

Send it a task:

```bash
oat message send browser-agent "Navigate to https://example.com and summarize the page."
```

Watch it work:

```bash
oat attach browser-agent --read-only --repo <repo-name>
```

Browser-agent audit and download locations:

```text
~/.oat/output/<repo-name>/browser-agent-actions.jsonl
~/.oat/downloads/<repo-name>/
```

## 6. Configure the DGX Spark Box

Spark is an internal/custom OAT provider. Treat the DGX endpoint as OpenAI-compatible, usually backed by vLLM or another OpenAI-style server.

You need three values:

- the DGX base URL, ending in `/v1`;
- the model name exposed by the server;
- the API key or bearer token, if the endpoint requires one.

Add the API key to OAT's global env file. Use a placeholder if the DGX endpoint does not enforce auth, but prefer a real token if one exists:

```bash
mkdir -p ~/.oat
echo 'SPARK_API_KEY=<token-for-dgx-spark>' >> ~/.oat/.env
```

Add a custom provider to `~/.oat/config.toml`:

```toml
[models.providers.spark]
models = [
  "bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4",
  "nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4",
]
api_key_env = "SPARK_API_KEY"
base_url = "https://<dgx-spark-host-or-tailscale-name>:<port>/v1"
class_path = "langchain_openai:ChatOpenAI"

[models.providers.spark.params]
temperature = 0
request_timeout = 120

[models.providers.spark.profile]
max_input_tokens = 96000
```

If the server only supports plain HTTP on a private network, use `http://.../v1` instead. If access depends on Proton VPN, Tailscale, or company network routing, connect that first and verify the endpoint is reachable before starting agents.

Quick endpoint smoke test:

```bash
export DGX_SPARK_BASE_URL="https://<dgx-spark-host-or-tailscale-name>:<port>/v1"
set -a
source ~/.oat/.env
set +a
curl "$DGX_SPARK_BASE_URL/models" \
  -H "Authorization: Bearer $SPARK_API_KEY"
```

If you do not export `DGX_SPARK_BASE_URL`, replace it with the literal base URL from `config.toml`.

## 7. Onboard the Spark Models

Before using a model with an agent, run OAT's model onboarding probe. This writes the model capability profile OAT uses for context-window and routing decisions.

Recommended first model:

```bash
oat model onboard spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4
```

The current checked-in profile marks this Gemma Spark model as worker-eligible and generally healthier for autonomous tool use.

Nemotron is available but should be treated cautiously:

```bash
oat model onboard spark:nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4
```

The current checked-in profile for Nemotron is `restricted`: it had strong basic tool reliability, but failed shell roundtrip and shell recovery probes. That makes it a poor default for autonomous coding workers. It can still be useful for assistant/browser conversations where the task is mostly reading, summarizing, and using browser tools under supervision.

## 8. Run OAT with Spark

Use Spark explicitly when initializing a repo or starting an assistant:

```bash
oat assistant start work --model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4
```

For a repo:

```bash
oat init https://github.com/<org>/<repo> --model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4
oat repo use <repo-name>
oat agent add browser-agent --repo <repo-name>
```

To switch an existing assistant or browser agent:

```bash
oat assistant set-model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4 work
oat assistant restart work

oat agent set-model browser-agent \
  --repo <repo-name> \
  --model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4 \
  --restart
```

## 9. Smoke-Test Checklist

1. OAT binaries work:

   ```bash
   oat version
   oat doctor
   ```

2. The daemon is running:

   ```bash
   oat start
   oat status
   ```

3. The Spark endpoint responds:

   ```bash
   export DGX_SPARK_BASE_URL="https://<dgx-spark-host-or-tailscale-name>:<port>/v1"
   set -a
   source ~/.oat/.env
   set +a
   curl "$DGX_SPARK_BASE_URL/models" \
     -H "Authorization: Bearer $SPARK_API_KEY"
   ```

4. OAT can create a Spark model:

   ```bash
   oat model onboard spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4
   ```

5. The assistant appears in the Chrome side panel:

   ```bash
   oat assistant start work --model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4 --open-panel
   ```

6. The repo browser agent can load tools:

   ```bash
   oat agent add browser-agent --repo <repo-name>
   oat message send browser-agent "Open https://example.com and tell me the page title."
   oat attach browser-agent --read-only --repo <repo-name>
   ```

## 10. Troubleshooting

### `oat agent add browser-agent` says the bridge was not found

Install `oat-browser-agent`, ensure the bridge is on `PATH`, or set:

```bash
export OAT_BROWSER_AGENT_BRIDGE_PATH=/absolute/path/to/oat-browser-agent/dist/bridge/index.js
```

Then retry:

```bash
oat agent add browser-agent --repo <repo-name>
```

### The side panel opens but cannot talk to OAT

Check that the daemon is running:

```bash
oat status
```

Then check assistant/browser logs:

```bash
oat assistant logs work --follow
oat logs list
```

If the error mentions native messaging or a missing session token, reinstall the Chrome native-messaging host from the `oat-browser-agent` repo and reload Chrome.

### Spark model fails with missing credentials

Confirm `SPARK_API_KEY` is present in `~/.oat/.env` or your shell:

```bash
set -a
source ~/.oat/.env
set +a
test -n "$SPARK_API_KEY" && echo "SPARK_API_KEY is set"
```

Then restart the agent so it picks up the environment:

```bash
oat assistant restart work
```

### Spark endpoint hangs or drops

Make sure Proton VPN, Tailscale, or company routing is connected and the DGX base URL is reachable:

```bash
export DGX_SPARK_BASE_URL="https://<dgx-spark-host-or-tailscale-name>:<port>/v1"
set -a
source ~/.oat/.env
set +a
curl "$DGX_SPARK_BASE_URL/models" \
  -H "Authorization: Bearer $SPARK_API_KEY"
```

For remote OpenAI-compatible endpoints, OAT injects connect-timeout and TCP keepalive handling when the provider is backed by `langchain_openai:ChatOpenAI`, which is why the `class_path` above matters.

### Nemotron behaves strangely in autonomous work

Use Gemma Spark as the default OAT model and reserve Nemotron for supervised assistant/browser tasks. The current Nemotron profile is restricted because it failed shell execution and shell recovery probes.

## Suggested First Call Flow

On the setup call, do this in order:

1. Confirm the operator has repo access, `gh auth status`, and OAT installed.
2. Confirm `oat-browser-agent` bridge and Chrome extension are installed.
3. Configure `~/.oat/config.toml` with the Spark provider.
4. Run the Spark `/models` curl smoke test.
5. Run `oat model onboard spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4`.
6. Start `oat assistant start work --model spark:bg-digitalservices/Gemma-4-26B-A4B-it-NVFP4 --open-panel`.
7. Add `browser-agent` to one test repo and ask it to open `https://example.com`.

Once those pass, the setup has the full path working: OAT daemon, assistant side panel, browser tools, and DGX Spark model access.
