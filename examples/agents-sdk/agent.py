"""OpenAI Agents SDK + local Ollama; model-generated tools always run in Cell."""
import argparse
import asyncio
import base64
import json
import os
import signal
import time

import httpx2 as httpx
from agents import Agent, ModelSettings, OpenAIChatCompletionsModel, Runner, function_tool, set_tracing_disabled
from openai import AsyncOpenAI

DEMO_TASK = (
    "Use the shell tool to create /workspace/add.py defining a Python function "
    "add(a, b) that returns their sum. Then use the shell tool to run Python, "
    "import that function, assert add(2, 3) == 5, and print CELL_MODEL_OK. "
    "You must actually call the tool to create and test the file."
)
VERIFY_COMMAND = "python -c \"import runpy; f=runpy.run_path('/workspace/add.py')['add']; assert f(2,3)==5; assert f(-4,9)==5; assert f(0,0)==0; print('CELL_MODEL_VERIFIED')\""


def emit(event, **fields):
    print(json.dumps({"event": event, **fields}), flush=True)


async def local_shell(command):
    """Called only when the entire SDK agent is already inside a Cell."""
    if os.getuid() != 65532 or not os.path.isfile('/cell-init'):
        raise RuntimeError('Contained mode must run inside Relayna Cell')
    process = await asyncio.create_subprocess_exec(
        '/bin/sh', '-c', command, cwd='/workspace', start_new_session=True,
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
    )
    try:
        async def read_bounded(stream):
            chunks, size = [], 0
            while chunk := await stream.read(8192):
                size += len(chunk)
                if size > 65536:
                    raise RuntimeError('tool output exceeds 64 KiB')
                chunks.append(chunk)
            return b''.join(chunks).decode(errors='replace')
        out, err, code = await asyncio.wait_for(asyncio.gather(
            read_bounded(process.stdout), read_bounded(process.stderr), process.wait(),
        ), timeout=30)
        return {'exit_code': code, 'stdout': out, 'stderr': err}
    finally:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        await process.wait()


async def delegated_shell(client, cell_id, command):
    response = await client.post(f'/v1alpha1/cells/{cell_id}/exec', json={
        'command': ['/bin/sh', '-c', command], 'timeout_seconds': 30,
    })
    response.raise_for_status()
    frames = [json.loads(line) for line in response.text.splitlines()]
    errors = [frame['error'] for frame in frames if frame.get('error')]
    code = next((frame['exit_code'] for frame in frames if 'exit_code' in frame), None)
    if errors or code is None:
        raise RuntimeError(f'Cell execution failed: {errors}')
    result = {'exit_code': code}
    for stream in ('stdout', 'stderr'):
        data = b''.join(base64.b64decode(frame.get('data', '')) for frame in frames if frame['stream'] == stream)
        if len(data) > 65536:
            raise RuntimeError('tool output exceeds 64 KiB')
        result[stream] = data.decode(errors='replace')
    return result


async def run(args):
    set_tracing_disabled(True)
    started = time.monotonic()
    cell_api = httpx.AsyncClient(
        transport=httpx.AsyncHTTPTransport(uds=args.socket), base_url='http://cell', timeout=45,
    ) if args.mode == 'delegated' else None
    async def model_response(response):
        # Small transport diagnostic; do not print hidden reasoning or request headers.
        if response.status_code == 200:
            payload = json.loads(await response.aread())
            choice = payload.get('choices', [{}])[0]
            message = choice.get('message', {})
            emit('agent.model_response', finish_reason=choice.get('finish_reason'),
                 tool_calls=len(message.get('tool_calls') or []),
                 content_chars=len(message.get('content') or ''),
                 usage=payload.get('usage'))

    model_client = AsyncOpenAI(
        base_url=args.endpoint, api_key='ollama-local', max_retries=0,
        http_client=httpx.AsyncClient(timeout=120, trust_env=True, event_hooks={'response': [model_response]}),
    )
    calls = 0
    lock = asyncio.Lock()

    async def execute(command):
        if not command or len(command.encode()) > 8192 or '\0' in command:
            raise ValueError('invalid shell command')
        # Serialize even if a provider emits multiple tool calls in one turn.
        async with lock:
            if args.mode == 'delegated':
                return await delegated_shell(cell_api, args.cell_id, command)
            return await local_shell(command)

    @function_tool(failure_error_function=None)
    async def shell(command: str) -> str:
        """Run a shell command in the Linux Cell workspace and return exit code and output.

        Args:
            command: Shell command to execute inside /workspace.
        """
        nonlocal calls
        calls += 1
        if calls > 8:
            raise RuntimeError('maximum 8 model tool calls reached')
        emit('agent.tool_call', command=command, mode=args.mode)
        result = await execute(command)
        emit('agent.tool_result', **result)
        return json.dumps(result)

    agent = Agent(
        name='Relayna local coding agent',
        instructions=(
            'You implement small Python tasks in /workspace on Linux. Use the shell tool '
            'to create files and run tests. Do not just describe commands. '
            'The workspace starts empty. Python, sh, cat and printf are installed. '
            'Write files using python -c or a shell heredoc. If a command fails, '
            'read its error and correct the command before continuing. '
            'After a successful test, answer briefly and stop. /no_think'
        ),
        model=OpenAIChatCompletionsModel(model=args.model, openai_client=model_client),
        model_settings=ModelSettings(
            temperature=0, max_tokens=512, parallel_tool_calls=False, tool_choice='required',
            extra_body={'reasoning_effort': 'none'},
        ),
        tools=[shell],
    )
    try:
        emit('agent.started', framework='openai-agents', model=args.model, mode=args.mode)
        result = await asyncio.wait_for(Runner.run(agent, args.task, max_turns=6), timeout=180)
        if calls == 0:
            raise RuntimeError(f'model completed without executing a tool: {result.final_output!r}')
        if args.verify_demo:
            verification = await execute(VERIFY_COMMAND)
            if verification['exit_code'] != 0 or 'CELL_MODEL_VERIFIED' not in verification['stdout']:
                raise RuntimeError(f'Generated implementation failed independent tests: {verification}')
            emit('agent.verified', **verification)
        emit('agent.result', output=result.final_output, tool_calls=calls,
             elapsed_seconds=round(time.monotonic()-started, 3),
             input_tokens=result.context_wrapper.usage.input_tokens,
             output_tokens=result.context_wrapper.usage.output_tokens)
    finally:
        await model_client.close()
        if cell_api:
            await cell_api.aclose()


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['contained', 'delegated'], required=True)
    parser.add_argument('--endpoint', required=True, help='Local inference gateway URL ending in /v1')
    parser.add_argument('--model', default='qwen3:4b-instruct')
    parser.add_argument('--cell-id')
    parser.add_argument('--socket', default='/run/relayna-cell/api.sock')
    parser.add_argument('--task', default=DEMO_TASK)
    parser.add_argument('--verify-demo', action='store_true')
    options = parser.parse_args()
    if options.mode == 'delegated' and not options.cell_id:
        parser.error('--cell-id is required in delegated mode')
    asyncio.run(run(options))
