import asyncio
import os
from typing import Any, Dict, List, Optional
import httpx2
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


def _dump(model: Any) -> Any:
    # mcp 2.x fields are snake_case in Python; by_alias keeps the MCP wire shape
    # (inputSchema, isError) that callers and the adapters were written against.
    if hasattr(model, "model_dump"):
        return model.model_dump(by_alias=True, mode="json")
    return model


class SamClient:
    """High-level developer interface for SAM MCP using official SDK."""
    
    def __init__(self, server_url: Optional[str] = None, token: Optional[str] = None):
        if server_url is None:
            server_url = os.environ.get("SAM_MCP_URL", "http://localhost:8080/mcp")
        if token is None:
            token = os.environ.get("SAM_API_TOKEN")
        self.server_url = server_url
        self.token = token
        self.session: Optional[ClientSession] = None
        self._sh_cm = None

    async def connect(self):
        """Connects to the SAM node via Streamable HTTP."""
        headers = {"Accept": "application/json, text/event-stream"}
        if self.token:
            headers["X-Sam-Authentication"] = f"Bearer {self.token}"

        # mcp 2.x speaks httpx2; an httpx client here is accepted but drops
        # server-initiated messages.
        self._http_client = httpx2.AsyncClient(
            headers=headers,
            follow_redirects=True,
            # The SDK's SSE-friendly defaults; the default 5s read timeout drops the stream.
            timeout=httpx2.Timeout(30.0, read=300.0),
        )
        try:
            self._sh_cm = streamable_http_client(self.server_url, http_client=self._http_client)
            read_stream, write_stream = await self._sh_cm.__aenter__()
            self.session = ClientSession(read_stream, write_stream)
            await self.session.__aenter__()
            await self.session.initialize()
        except Exception:
            await self.close()
            raise

    async def close(self):
        """Closes the connection."""
        if self.session:
            await self.session.__aexit__(None, None, None)
        if self._sh_cm:
            await self._sh_cm.__aexit__(None, None, None)
        if hasattr(self, '_http_client') and self._http_client:
            await self._http_client.aclose()
        self.session = None
        self._sh_cm = None
        self._http_client = None

    async def get_tools(self) -> List[Dict[str, Any]]:
        """Returns available mesh tools as wire-format dicts (camelCase keys such as inputSchema)."""
        if not self.session:
            raise RuntimeError("Not connected")
        resp = await self.session.list_tools()
        return [_dump(t) for t in resp.tools]

    async def call_tool(self, name: str, arguments: Dict[str, Any]) -> Dict[str, Any]:
        """Executes a tool over the mesh and returns the wire-format result dict."""
        if not self.session:
            raise RuntimeError("Not connected")
        resp = await self.session.call_tool(name, arguments)
        return _dump(resp)

    async def __aenter__(self):
        await self.connect()
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        await self.close()
