"""Gemini-backed A2A chat agent hosted by a node in the local dev mesh."""
import os
import time
import uuid

import uvicorn
from a2a.server.agent_execution.agent_executor import AgentExecutor
from a2a.server.agent_execution.context import RequestContext
from a2a.server.events.event_queue import EventQueue
from a2a.server.request_handlers import DefaultRequestHandler
from a2a.server.routes import create_agent_card_routes, create_jsonrpc_routes
from a2a.server.tasks.inmemory_task_store import InMemoryTaskStore
from a2a.types import (
    AgentCapabilities,
    AgentCard,
    AgentInterface,
    AgentSkill,
    Message,
    Part,
    Role,
)
from google import genai
from google.genai import types
from starlette.applications import Starlette

PORT = 7777
MODEL = os.environ.get("GEMINI_MODEL", "models/gemini-3.5-flash-lite")

class ChatExecutor(AgentExecutor):
    """One Gemini chat session per A2A contextId; the session carries the history."""

    def __init__(self):
        self.gemini = genai.Client()
        self.chats = {}

    async def execute(self, context: RequestContext, event_queue: EventQueue) -> None:
        chat = self.chats.get(context.context_id)
        if chat is None:
            # Gemini 3 Flash defaults to thinking_level=high, which dominates latency.
            chat = self.gemini.aio.chats.create(
                model=MODEL,
                config=types.GenerateContentConfig(
                    thinking_config=types.ThinkingConfig(thinking_level="minimal")
                ),
            )
            self.chats[context.context_id] = chat
        started = time.monotonic()
        reply = await chat.send_message(context.get_user_input())
        print(
            f"[chat] context={context.context_id} gemini took "
            f"{time.monotonic() - started:.1f}s usage={reply.usage_metadata}",
            flush=True,
        )
        await event_queue.enqueue_event(
            Message(
                role=Role.ROLE_AGENT,
                message_id=str(uuid.uuid4()),
                parts=[Part(text=reply.text or "")],
                context_id=context.context_id,
                task_id=context.task_id,
            )
        )

    async def cancel(self, context: RequestContext, event_queue: EventQueue) -> None:
        pass


agent_card = AgentCard(
    name="chat",
    description="Gemini-backed conversational agent; remembers the conversation per contextId",
    version="0.1.0",
    capabilities=AgentCapabilities(streaming=False),
    default_input_modes=["text"],
    default_output_modes=["text"],
    skills=[
        AgentSkill(
            id="chat",
            name="chat",
            description="Multi-turn small talk",
            tags=["chat"],
            examples=["hi, my name is Ada", "what is my name?"],
        )
    ],
    supported_interfaces=[
        AgentInterface(
            protocol_binding="JSONRPC",
            protocol_version="1.0",
            url=f"http://127.0.0.1:{PORT}/",
        )
    ],
)

handler = DefaultRequestHandler(
    agent_executor=ChatExecutor(),
    task_store=InMemoryTaskStore(),
    agent_card=agent_card,
)
# Starlette over FastAPI: the SDK generates the routes, so FastAPI would add nothing.
# JSON-RPC at "/": the mesh card regeneration drops URL subpaths, so clients land on the root.
app = Starlette(
    routes=[
        *create_jsonrpc_routes(request_handler=handler, rpc_url="/"),
        *create_agent_card_routes(agent_card=agent_card),
    ]
)

if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=PORT)
