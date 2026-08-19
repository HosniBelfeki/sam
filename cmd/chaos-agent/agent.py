import os
import sys
import asyncio
import argparse
import json
from typing import Any, Dict

from langchain_openai import ChatOpenAI
from langchain.agents import AgentExecutor, create_tool_calling_agent
from langchain_core.prompts import ChatPromptTemplate
from langchain_core.tools import tool

from mcp.client.session import ClientSession
from mcp.client.sse import sse_client

async def main():
    parser = argparse.ArgumentParser(description="Chaos Monkey Agent via LangChain & MCP")
    parser.add_argument("--mcp-url", default="http://127.0.0.1:8080/mcp", help="MCP SSE endpoint")
    parser.add_argument("--inference-url", default="http://127.0.0.1:8080/v1", help="OpenAI-compatible endpoint")
    parser.add_argument("--auth", default="", help="Authorization header (e.g. Bearer <token>)")
    parser.add_argument("--prompt", default="You are a chaos monkey testing a distributed mesh network. Use all tools available to you. Pass extreme, invalid, or adversarial inputs (like huge strings, negative numbers, SQL injection strings) to see if the tools or network crash. Report any errors you discover.", help="Agent instruction prompt")
    args = parser.parse_args()

    headers = {}
    if args.auth:
        headers["X-Sam-Authentication"] = args.auth
        headers["Authorization"] = args.auth

    print(f"Connecting to MCP at {args.mcp_url}...")
    
    # Connect to MCP via SSE
    async with sse_client(args.mcp_url, headers=headers) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            
            # Fetch tools
            tools_response = await session.list_tools()
            print(f"Discovered {len(tools_response.tools)} MCP tools.")
            
            # Build LangChain tools dynamically
            lc_tools = []
            for t in tools_response.tools:
                # We use a factory function to capture the tool name correctly in the closure
                def make_tool(tool_name, tool_desc):
                    @tool(tool_name)
                    async def mcp_tool(arguments_json: str) -> str:
                        """Use this tool by passing a JSON string of arguments matching the schema."""
                        try:
                            args = json.loads(arguments_json) if arguments_json else {}
                            res = await session.call_tool(tool_name, arguments=args)
                            return str(res.content)
                        except Exception as e:
                            return f"Tool Execution Error: {e}"
                    # Append the actual schema to the description so the LLM knows how to use it
                    mcp_tool.description = f"{tool_desc}\nSchema: {t.inputSchema}"
                    return mcp_tool
                
                lc_tools.append(make_tool(t.name, t.description))
            
            # Initialize LangChain Chat Model (OpenAI compatible)
            llm = ChatOpenAI(
                model="default",
                openai_api_base=args.inference_url,
                openai_api_key=args.auth if args.auth else "none",
                default_headers=headers
            )
            
            # Define the agent prompt
            prompt_template = ChatPromptTemplate.from_messages([
                ("system", "You are an autonomous adversarial AI agent. You have access to tools."),
                ("human", "{input}"),
                ("placeholder", "{agent_scratchpad}"),
            ])
            
            # Create the LangChain Agent
            print("Initializing LangChain Tool-Calling Agent...")
            agent = create_tool_calling_agent(llm, lc_tools, prompt_template)
            
            # AgentExecutor runs the ReAct/Tool loop automatically!
            agent_executor = AgentExecutor(agent=agent, tools=lc_tools, verbose=True, max_iterations=10)
            
            print(f"\n--- Starting Chaos Monkey Agent Loop ---")
            print(f"Instruction: {args.prompt}\n")
            
            try:
                result = await agent_executor.ainvoke({"input": args.prompt})
                print("\n--- Final Agent Result ---")
                print(result["output"])
            except Exception as e:
                print(f"Agent Loop crashed (this might be a successful chaos test!): {e}")

if __name__ == "__main__":
    asyncio.run(main())
