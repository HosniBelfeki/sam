# SAM Python SDK (sam-mcp-python)

The official Python SDK for SAM.

This SDK acts as a "Thin Client" that connects to the local Go node via a Unix Domain Socket and communicates using the Model Context Protocol (MCP) over JSON-RPC 2.0.

## Installation

```bash
pip install .
```

## Usage

```python
import asyncio
from sam_mcp.client import SamClient

async def main():
    async with SamClient() as client:
        tools = await client.get_tools()
        print("Available tools:", tools)
        
        result = await client.call_tool("echo", {"message": "hello"})
        print("Result:", result)

if __name__ == "__main__":
    asyncio.run(main())
```

## Development

### Running Tests

```bash
pytest tests/unit
pytest tests/e2e
```
