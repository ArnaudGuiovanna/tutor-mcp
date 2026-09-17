# Connect your AI client

[Back to README](../README.md#documentation) · [Installation](installation.md) · [Profiles](profiles.md)

Tutor MCP exposes the same learning tools over local **stdio** and remote
**Streamable HTTP**. Choose the connection your client supports:

| Client | Connection | Setup |
|---|---|---|
| **Claude Code** | Local stdio or remote HTTPS | [Local command](installation.md#claude-code) · [Official MCP guide](https://code.claude.com/docs/en/mcp) |
| **Claude Desktop** | Local stdio; remote connectors where available | [Local JSON configuration](installation.md#claude-desktop) · [Official local-server guide](https://modelcontextprotocol.io/docs/develop/connect-local-servers) |
| **Claude on the web** | Remote HTTPS with OAuth | Add your VPS `/mcp` URL as a custom connector. [Claude connector documentation](https://claude.com/docs/connectors/building/authentication) |
| **ChatGPT** | Remote HTTPS with OAuth | Add your VPS `/mcp` URL through the MCP app/developer-mode flow available to your account. [Official guide](https://help.openai.com/en/articles/12584461-developer-mode-and-mcp-apps-in-chatgpt-beta) |
| **Hermes** | Local stdio or remote HTTPS with OAuth | [Local YAML configuration](installation.md#hermes) · [Official MCP guide](https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp) |
| **Pi** | Local stdio through an MCP extension/bridge | Configure the extension to launch `tutor-mcp --local`. Pi's core delegates MCP integration to extensions; use the configuration format of your chosen extension. [Pi extension model](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md#extensions) |
| **Gemini CLI** | Local stdio or remote Streamable HTTP | Use `command`/`args` for local mode or `httpUrl` for HTTP. [Official MCP guide](https://geminicli.com/docs/tools/mcp-server/) |
| **Le Chat** | Remote HTTPS connector | Register your VPS `/mcp` URL as a custom MCP connector. [Mistral connector guide](https://mistral.ai/news/connectors/) |
| **Gemini Enterprise** | Remote custom MCP data store | Follow the administrator's OAuth/client-registration setup. [Official setup guide](https://docs.cloud.google.com/gemini/enterprise/docs/connectors/custom-mcp-server/set-up-custom-mcp-server) |

Other clients can connect if they support the relevant MCP transport and,
for remote access, Tutor's OAuth flow. Account plans and administrator policies
can affect connector availability; the linked client documentation is authoritative.

## Local connection

The MCP client starts the Tutor executable with `--local` and exchanges messages
over stdin/stdout. Supply the full executable path if the client cannot find it
on PATH. Use `--data-dir` when choosing a different private profile directory.

Multiple local clients can share the same profile. They use one persistent
learner identity, so switching clients on the same machine can preserve learning
history. Separate directories or servers remain independent; there is no automatic
synchronization.

For clients using `mcpServers` JSON, including Gemini CLI, the shape is:

```json
{
  "mcpServers": {
    "tutor": {
      "command": "/absolute/path/to/tutor-mcp",
      "args": ["--local"]
    }
  }
}
```

Hermes uses YAML. Pi's MCP extension defines its own configuration. See the
table above before copying a configuration into a client.

## Remote connection

Install a hobby or institutional profile and expose its MCP endpoint at
`https://your.domain/mcp`. A hosted client needs a reachable endpoint with a
publicly trusted HTTPS certificate.

The client discovers the authorization server, registers or resolves its client
metadata, and opens the sign-in and consent flow. Hobby accounts use an operator
invitation and a username/password; institutional accounts use verified email.
PKCE, resource binding and refresh-token rotation protect the remote session.
Institutional operators can restrict dynamic registration; see
[OAuth registration](oauth-dcr-production.md).

## Help the client use the learning loop

Ask the assistant to use Tutor MCP explicitly when starting or resuming a lesson.
Tutor exposes a `tutor_mcp` prompt for clients that support MCP prompts, together
with tool descriptions that guide the learning loop. The assistant must call the
tools to read context and record work; plain conversation alone does not update
the learning record. See the [tool catalog](mcp-tools.md).

## Compatibility evidence

The table describes integration routes supported by the clients and Tutor's
transports. Automated fixtures exercise Hermes metadata and Claude/ChatGPT-style
OAuth flows. Native CI tests local stdio on Linux, macOS and Windows; Compose
acceptance covers HTTPS, OAuth, account management and persistence.

These checks are distinct from an end-to-end test inside every listed product.
Live Claude and ChatGPT sessions have not yet been verified for v0.6.0; no live
acceptance result is claimed here for the other clients. Pi additionally depends
on the MCP extension selected by the user.
