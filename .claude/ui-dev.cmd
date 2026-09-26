@echo off
rem Dev helper: start the mcpfault UI with the examples' Python venv on PATH.
set "PATH=D:\AgentMCP\.venv\Scripts;D:\AgentMCP\bin;%PATH%"
cd /d D:\AgentMCP
bin\mcpfault.exe ui --open=false
