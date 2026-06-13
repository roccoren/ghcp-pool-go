#!/usr/bin/env python3
"""Small JSON worker that lets the Go gateway call the official Copilot SDK.

The worker is intentionally process-per-operation: the Go gateway keeps routing,
cache, auth, and metering in-process while this adapter isolates Python SDK
imports and session lifecycle. It implements real model listing and plain chat
turns; host/tool execution is not performed here.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path


TOKEN_ENV_VARS = ("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN")
SESSION_WAIT_TIMEOUT_SECONDS = 300.0


def _ok(result: object) -> None:
    print(json.dumps({"ok": True, "result": result}, ensure_ascii=False))


def _err(message: str) -> None:
    print(json.dumps({"ok": False, "error": message}, ensure_ascii=False))


def _env_without_token_overrides() -> dict[str, str]:
    env = dict(os.environ)
    for name in TOKEN_ENV_VARS:
        env.pop(name, None)
    return env


def _render_prompt(messages: list[dict]) -> str:
    systems = [m.get("content", "") for m in messages if m.get("role") == "system"]
    convo = [m for m in messages if m.get("role") != "system"]
    lines: list[str] = []
    if systems:
        lines.append("[system]\n" + "\n".join(str(s) for s in systems))
    for msg in convo:
        role = msg.get("role", "user")
        if role == "tool":
            lines.append(f"[tool result for {msg.get('tool_call_id', '')}]\n{msg.get('content', '')}")
            continue
        block = f"[{role}]\n{msg.get('content', '')}"
        for tc in msg.get("tool_calls") or []:
            fn = tc.get("function") if isinstance(tc.get("function"), dict) else tc
            block += (
                f"\n(called tool {fn.get('name', '')} [{tc.get('id', '')}] "
                f"with arguments {fn.get('arguments', tc.get('arguments', ''))})"
            )
        lines.append(block)
    lines.append("[assistant]")
    return "\n\n".join(lines)


def _event_type(ev) -> str:
    t = getattr(ev, "type", "")
    return str(getattr(t, "value", t))


def _usage_from_event(event) -> dict:
    data = getattr(event, "data", event)
    d: dict = {}
    if hasattr(data, "to_dict"):
        try:
            d = data.to_dict() or {}
        except Exception:
            d = {}
    elif isinstance(data, dict):
        d = data

    def g(*names, default=None):
        for name in names:
            if isinstance(d, dict) and d.get(name) is not None:
                return d[name]
        for name in names:
            val = getattr(data, name, None)
            if val is not None:
                return val
        return default

    copilot_usage = g("copilotUsage", "copilot_usage", default={}) or {}
    if not isinstance(copilot_usage, dict) and hasattr(copilot_usage, "to_dict"):
        try:
            copilot_usage = copilot_usage.to_dict() or {}
        except Exception:
            copilot_usage = {}
    nano_aiu = 0.0
    if isinstance(copilot_usage, dict):
        nano_aiu = float(
            copilot_usage.get("totalNanoAiu")
            or copilot_usage.get("total_nano_aiu")
            or 0
        )
    cost = float(g("cost", "credits", "credit_cost", "creditCost", default=0) or 0)
    credits = (nano_aiu / 1e9) if nano_aiu else cost

    def s(v):
        return None if v is None else str(getattr(v, "value", v))

    return {
        "input_tokens": int(g("inputTokens", "input_tokens", "prompt_tokens", default=0) or 0),
        "output_tokens": int(g("outputTokens", "output_tokens", "completion_tokens", default=0) or 0),
        "cached_tokens": int(g("cacheReadTokens", "cache_read_tokens", "cached_tokens", default=0) or 0),
        "credits": credits,
        "api_endpoint": s(g("apiEndpoint", "api_endpoint")),
        "provider_call_id": s(g("providerCallId", "provider_call_id")),
        "duration_ms": int(g("duration", "api_call_duration_ms", "apiCallDurationMs", default=0) or 0),
    }


def _content_from_response(response) -> str:
    if response is None:
        return ""
    data = getattr(response, "data", None)
    if data is not None:
        content = getattr(data, "content", None)
        if content:
            return content
    return getattr(response, "content", "") or ""


async def _close_session(session) -> None:
    for attr in ("disconnect", "close", "stop"):
        fn = getattr(session, attr, None)
        if fn is None:
            continue
        try:
            result = fn()
            if asyncio.iscoroutine(result):
                await result
        finally:
            return


async def _new_client(req: dict):
    from copilot import CopilotClient  # type: ignore

    token = req.get("github_token") or None
    base_directory = req.get("base_directory") or None
    options: dict = {"mode": "empty"}
    if base_directory:
        Path(base_directory).mkdir(parents=True, exist_ok=True)
        options["base_directory"] = str(Path(base_directory).resolve())
    if token:
        options["github_token"] = token
        options["use_logged_in_user"] = False
    elif base_directory:
        options["env"] = _env_without_token_overrides()
    client = CopilotClient(**options)
    start = getattr(client, "start", None)
    if start is not None:
        result = start()
        if asyncio.iscoroutine(result):
            await result
    return client


async def _stop_client(client) -> None:
    stop = getattr(client, "stop", None)
    if stop is None:
        return
    result = stop()
    if asyncio.iscoroutine(result):
        await result


async def list_models(req: dict) -> dict:
    client = await _new_client(req)
    try:
        for attr in ("list_models", "get_models", "models"):
            fn = getattr(client, attr, None)
            if fn is None:
                continue
            result = fn() if callable(fn) else fn
            if asyncio.iscoroutine(result):
                result = await result
            models = []
            for item in result or []:
                if isinstance(item, dict):
                    mid = item.get("id")
                else:
                    mid = getattr(item, "id", None) or str(item)
                if mid:
                    models.append(str(mid))
            if models:
                return {"models": models}
        return {"models": []}
    finally:
        await _stop_client(client)


async def chat(req: dict) -> dict:
    payload = req.get("payload") or {}
    params = payload.get("params") or {}
    if params.get("tools"):
        raise RuntimeError("Copilot SDK worker backend does not support tool calls yet")

    model = payload["model"]
    messages = payload.get("messages") or []
    client = await _new_client(req)
    session = None
    usage_holder: dict = {}
    try:
        kwargs: dict = {
            "model": model,
            "streaming": False,
            "available_tools": [],
        }
        if req.get("github_token"):
            kwargs["github_token"] = req["github_token"]
        if params.get("reasoning_effort"):
            kwargs["reasoning_effort"] = params["reasoning_effort"]
        session = await client.create_session(**kwargs)
        session.on(lambda ev: usage_holder.update(usage=_usage_from_event(ev)) if _event_type(ev).endswith("usage") else None)
        response = await session.send_and_wait(
            _render_prompt(messages),
            timeout=SESSION_WAIT_TIMEOUT_SECONDS,
        )
        content = _content_from_response(response)
    finally:
        if session is not None:
            await _close_session(session)
        await _stop_client(client)

    usage = usage_holder.get("usage") or {
        "input_tokens": max(1, len(_render_prompt(messages).split())),
        "output_tokens": max(1, len(content.split())) if content else 0,
    }
    usage["total_tokens"] = usage.get("total_tokens") or (
        int(usage.get("input_tokens") or 0) + int(usage.get("output_tokens") or 0)
    )
    return {
        "content": content,
        "model": model,
        "usage": usage,
        "finish_reason": "stop",
        "tool_calls": [],
    }


async def main() -> None:
    req = json.load(sys.stdin)
    op = req.get("op")
    if op == "models":
        _ok(await list_models(req))
    elif op == "chat":
        _ok(await chat(req))
    else:
        raise RuntimeError(f"unknown operation: {op}")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except Exception as exc:
        _err(str(exc))
        sys.exit(1)
