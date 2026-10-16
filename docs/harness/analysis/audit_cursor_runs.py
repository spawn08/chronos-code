#!/usr/bin/env python3
"""Read-only, metadata-only audit of local Cursor JSONL and Chronos SQLite runs.

Transcripts omit system prompts, tool results, token usage, and timing. Tool-use
blocks in one assistant record measure batching opportunity, not concurrency.
No transcript bodies, commands, source, or credentials are exported.
"""

import argparse
import collections
import datetime
import json
from pathlib import Path
import re
import sqlite3


def cursor_run(path):
    roles = collections.Counter()
    tools = collections.Counter()
    batch_sizes = []
    reads = collections.Counter()
    seen_reads = set()
    edits = edits_after_read = 0
    shell_yields = collections.Counter()
    dynamic_tools = collections.Counter()
    terminal_reads = 0
    long_shell_payloads = 0
    end_statuses = collections.Counter()
    timestamps = []
    references_quant = False
    tool_results = 0
    for line_number, line in enumerate(path.open(encoding="utf-8"), 1):
        record = json.loads(line)
        if record.get("type") == "turn_ended":
            end_statuses[record.get("status", "unknown")] += 1
            continue
        roles[record.get("role", "unknown")] += 1
        blocks = record.get("message", {}).get("content", [])
        if not isinstance(blocks, list):
            raise ValueError(f"unsupported content format: {path.name}:{line_number}")
        calls = [b for b in blocks if b.get("type") == "tool_use"]
        if calls:
            batch_sizes.append(len(calls))
        # A read in the same batch cannot establish a read-before-edit dependency.
        prior_reads = set(seen_reads)
        for block in blocks:
            if block.get("type") == "text":
                text = block.get("text", "")
                references_quant |= "quant-trade" in text
                if record.get("role") == "user":
                    timestamps.extend(re.findall(r"<timestamp>(.*?)</timestamp>", text))
            elif block.get("type") == "tool_result":
                tool_results += 1
        for call in calls:
            name = call.get("name", "unknown")
            args = call.get("input", {})
            tools[name] += 1
            references_quant |= "quant-trade" in json.dumps(args)
            target = args.get("path", "")
            if name == "Read":
                reads[(target, args.get("offset"), args.get("limit"))] += 1
                seen_reads.add(target)
                terminal_reads += "/terminals/" in target
            if name == "StrReplace":
                edits += 1
                edits_after_read += target in prior_reads
            if name == "Shell":
                shell_yields[str(args.get("block_until_ms", "omitted"))] += 1
                long_shell_payloads += len(args.get("command", "")) > 2000
            if name == "CallDynamicTool":
                dynamic_tools[args.get("namespace", "") + ":" + args.get("toolName", "")] += 1
    return {
        "id": path.stem,
        "source": str(path),
        "modified_utc": datetime.datetime.fromtimestamp(
            path.stat().st_mtime, datetime.timezone.utc
        ).isoformat(),
        "first_user_timestamp": timestamps[0] if timestamps else None,
        "last_user_timestamp": timestamps[-1] if timestamps else None,
        "references_quant_trade": references_quant,
        "records_by_role": dict(roles),
        "tool_results": tool_results,
        "tools": dict(sorted(tools.items())),
        "tool_calls": sum(tools.values()),
        "tool_call_records": len(batch_sizes),
        "calls_per_tool_record": round(sum(batch_sizes) / len(batch_sizes), 3)
        if batch_sizes else 0,
        "max_batch": max(batch_sizes, default=0),
        "multi_call_records": sum(n > 1 for n in batch_sizes),
        "repeated_exact_read_requests": sum(n - 1 for n in reads.values()),
        "strreplace_calls": edits,
        "strreplace_with_prior_read_path": edits_after_read,
        "shell_block_until_ms": dict(shell_yields),
        "dynamic_tool_calls": dict(dynamic_tools),
        "terminal_read_calls": terminal_reads,
        "shell_commands_over_2000_chars": long_shell_payloads,
        "turn_end_statuses": dict(end_statuses),
    }


def read_only_db(path):
    connection = sqlite3.connect(path.resolve().as_uri() + "?mode=ro", uri=True)
    connection.row_factory = sqlite3.Row
    connection.execute("PRAGMA query_only=ON")
    return connection


def chronos_runs(directory, since):
    with read_only_db(directory / "sessions.db") as db:
        counts = [dict(row) for row in db.execute(
            "SELECT kind, name, count(*) AS spans, "
            "sum(ended_at IS NULL OR ended_at LIKE '0001-%') AS unclosed "
            "FROM traces WHERE started_at >= ? GROUP BY kind, name ORDER BY spans DESC",
            (since,),
        )]
        sessions = [dict(row) for row in db.execute(
            "SELECT id, agent_id, status, created_at, updated_at FROM sessions "
            "WHERE created_at >= ? ORDER BY created_at", (since,),
        )]
        model_counts = collections.Counter()
        usage = collections.Counter()
        for row in db.execute(
            "SELECT output FROM traces WHERE kind='model_call' AND started_at >= ?", (since,)
        ):
            output = json.loads(row["output"] or "{}")
            model_counts[output.get("model", "unknown")] += 1
            for key, value in (output.get("usage") or {}).items():
                if isinstance(value, (int, float)):
                    usage[key] += value
        nesting = []
        for row in db.execute(
            "SELECT session_id, seq_num, payload FROM events "
            "WHERE type='chat_message' AND created_at >= ?", (since,)
        ):
            payload = json.loads(row["payload"] or "{}")
            text = payload.get("content", "")
            if payload.get("role") == "user" and isinstance(text, str):
                depth = peak = 0
                for match in re.finditer(r"</?turn_context>", text):
                    depth += -1 if match.group() == "</turn_context>" else 1
                    peak = max(peak, depth)
                if peak:
                    nesting.append({"session": row["session_id"], "seq": row["seq_num"], "depth": peak})
        summaries = db.execute(
            "SELECT count(*) FROM events WHERE type='chat_summary' AND created_at >= ?", (since,)
        ).fetchone()[0]
    with read_only_db(directory / "telemetry.db") as db:
        telemetry = [dict(row) for row in db.execute(
            "SELECT id, started_at, ended_at, model, turns, input_tokens, "
            "output_tokens, cost_usd FROM sessions WHERE started_at >= ? "
            "ORDER BY started_at", (since,),
        )]
    return {"since": since, "session_metadata": sessions,
            "trace_counts": counts, "telemetry_session_metadata": telemetry,
            "model_calls_by_model": dict(model_counts), "model_usage": dict(usage),
            "max_turn_context_depth": max((n["depth"] for n in nesting), default=0),
            "deepest_turn_context_event": max(nesting, key=lambda n: n["depth"], default=None),
            "chat_summary_events": summaries}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cursor-root", type=Path, required=True)
    parser.add_argument("--ids", nargs="*", help="Explicit transcript IDs; omit to inventory")
    parser.add_argument("--chronos-dir", type=Path)
    parser.add_argument("--since", default="2026-10-13")
    parser.add_argument("--summary-only", action="store_true", help="Omit individual Chronos session rows")
    args = parser.parse_args()
    runs = []
    for path in sorted(args.cursor_root.glob("*/*.jsonl")):
        if args.ids is None or path.stem in args.ids:
            runs.append(cursor_run(path))
    if args.ids and set(args.ids) != {r["id"] for r in runs}:
        parser.error("one or more requested transcript IDs were not found")
    output = {"cursor": runs}
    if args.chronos_dir:
        output["chronos"] = chronos_runs(args.chronos_dir, args.since)
        if args.summary_only:
            sessions = output["chronos"].pop("session_metadata")
            telemetry = output["chronos"].pop("telemetry_session_metadata")
            output["chronos"]["sessions_by_agent"] = dict(collections.Counter(s["agent_id"] for s in sessions))
            output["chronos"]["telemetry_sessions"] = len(telemetry)
            output["chronos"]["telemetry_sessions_with_cost"] = sum(s["cost_usd"] > 0 for s in telemetry)
            output["chronos"]["telemetry_sessions_with_end_timestamp"] = sum(s["ended_at"] is not None for s in telemetry)
    print(json.dumps(output, indent=2))


if __name__ == "__main__":
    main()
