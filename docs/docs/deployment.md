---
sidebar_position: 11
title: Running as a Server
description: Run Chronos Code as an HTTP service for your team or your own tooling.
---

# Running as a Server

`chronos-code serve` runs Chronos Code as an HTTP service. Use it when you want a shared instance for a team, or want to call it from your own tools, bots or internal platforms.

## Start the server

```bash
export CHRONOS_CODE_API_KEY="choose-a-long-random-secret"
chronos-code serve --listen :8430 --auth api_key
```

Run it from the folder of the repository it should work on. That repository's `.chronos-code/` settings, `AGENTS.md` and safety policy apply as usual.

## Send a task

```bash
curl -s http://localhost:8430/v1/chat \
  -H "Authorization: Bearer $CHRONOS_CODE_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"message": "summarize what changed in the last 5 commits"}'
```

The response has the same shape as the [headless JSON result](./headless#json-output-for-pipelines). Pass back the returned `session_id` to continue the conversation, and use `"agent_id": "reviewer"` to address a specialist. `POST /v1/chat/stream` streams the reply instead.

Other useful endpoints: `GET /v1/agents`, `GET /v1/sessions`, and `POST /v1/teams/{id}/run`.

## Settings

Set these in `.chronos-code/config.yaml` or pass them as flags to `serve`:

```yaml
server:
  listen: ":8430"
  auth_type: api_key          # or OIDC for single sign-on
  max_concurrent: 1           # tasks running at the same time
  request_timeout_sec: 300
  rate_limit_per_min: 60
```

Keep the API key in the `CHRONOS_CODE_API_KEY` environment variable, never in a committed file.

## Recommendations

- **One task at a time per checkout.** Leave `max_concurrent: 1` unless you're sure overlapping tasks won't step on each other's files.
- **No one is there to approve.** As with [headless runs](./headless#permissions-when-nobody-is-watching), anything that would ask for approval is refused. Choose the tasks and permissions accordingly, and don't auto-approve on repositories you don't trust.
- **Protect the endpoint.** Always enable authentication. Listen only on localhost when a reverse proxy sits in front.
- **Health checks.** `GET /live` reports that the process is up, and `GET /ready` that it can take work. Use them for your load balancer or container platform. `GET /metrics` provides Prometheus metrics.
- **Privacy.** Logs and metrics never contain your prompts, code or tool output.

## Several instances

You can run several servers behind a load balancer. Each conversation belongs to exactly one instance. Give every instance the same member list and its own name:

```yaml
server:
  instance_id: node-a
  fleet_instances: [node-a, node-b, node-c]
```

When a request reaches the wrong instance, it is rejected with HTTP 409, and the `X-Chronos-Session-Owner` header names the right one, so your proxy can route it there. Change the member list only when all instances are drained, and update every instance at the same time.
