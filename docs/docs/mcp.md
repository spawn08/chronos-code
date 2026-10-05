---
sidebar_position: 8
title: MCP Servers
description: Connect external tools such as issue trackers, databases and documentation search through MCP servers.
---

# MCP Servers

MCP (Model Context Protocol) servers give Chronos Code extra tools: your issue tracker, a database, internal documentation search, a browser, and more. Any MCP server that runs as a local command, or is reachable over HTTPS, can be used.

## Add a server

**A local command:**

```bash
chronos-code mcp add github --command github-mcp-server --arg stdio
```

**A remote server over HTTPS:**

```bash
chronos-code mcp add docs --url https://mcp.example.com/sse
```

By default, the server is saved in `.mcp.json` in the current folder, so you can commit it and share it with your team. Add `--scope user` to save it in `~/.chronos-code/mcp.json` instead, and use it in every project.

Then check that it works:

```bash
chronos-code mcp test github
```

## Use it

Start Chronos Code and run `/mcp` to see your servers and their status. If a server is listed as waiting for approval, connect it:

```text
/mcp connect github
```

After that, its tools are available to the agent. Just ask:

```text
Look up issue #482 and fix the bug it describes
```

To connect servers at startup, or in a headless run, pass `--mcp-connect <name>` (repeatable): `chronos-code run --mcp-connect github "..."`.

Each call to an MCP tool asks for your approval, like edits and shell commands do. Press `a` to allow a server's tool for the rest of the session.

## Servers you already have

Chronos Code also picks up MCP servers you've configured for other tools:

- `.mcp.json` (project)
- `.cursor/mcp.json`
- `.vscode/mcp.json`
- `.claude/mcp.json`
- `~/.chronos-code/mcp.json` (personal)

If two files define a server with the same name, the first one in this list wins.

## The `.mcp.json` format

```json
{
  "mcpServers": {
    "github": {
      "transport": "stdio",
      "command": "github-mcp-server",
      "args": ["stdio", "--token", "${GITHUB_TOKEN}"]
    },
    "docs": {
      "transport": "sse",
      "url": "https://mcp.example.com/sse?key=${DOCS_API_KEY}"
    }
  }
}
```

:::warning Credentials
Anything that looks like a token or password must be written as a `${VARIABLE}` reference, and set in your environment before starting Chronos Code. Servers with a pasted-in secret are refused, so secrets can't end up in your repository by accident. `mcp list` and `mcp test` hide credential values in their output.
:::

Only local commands (`stdio`) and HTTPS servers (`sse`) are supported.

## Managing servers

| Command | What it does |
|---|---|
| `chronos-code mcp list` | Show configured servers (secrets hidden) |
| `chronos-code mcp test <name>` | Connect, list the server's tools, and disconnect |
| `chronos-code mcp remove <name>` | Remove a server |
| `/mcp` (in the app) | Show status and tool counts |
| `/mcp connect <name>` | Connect a server that is waiting for approval |

If one server fails to start, the others and the rest of Chronos Code keep working.

## Turning MCP off

To ignore all MCP configuration, for example in CI:

```yaml
# config.yaml
mcp:
  discovery_enabled: false
```

To block specific servers in a project, list them in `security.yaml`:

```yaml
mcp:
  denied_servers: ["browser"]
```
