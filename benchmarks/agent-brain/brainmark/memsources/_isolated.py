"""Run environment-configured legacy clients in a dedicated process."""
from __future__ import annotations
import base64
import dataclasses
import importlib
import json
import os
import pathlib
import subprocess
import sys
from .base import MemoryPacket, MemorySourceError


def build(arm: str, environment: dict[str, str], arguments: dict) -> MemoryPacket:
    payload = {**arguments, 'transcript_bytes': base64.b64encode(arguments['transcript_bytes']).decode()}
    env = {**os.environ, **environment}
    package_root = str(pathlib.Path(__file__).resolve().parents[2])
    env['PYTHONPATH'] = package_root + os.pathsep + env.get('PYTHONPATH', '')
    try:
        proc = subprocess.run([sys.executable, '-m', 'brainmark.memsources._isolated', arm],
                              input=json.dumps(payload), capture_output=True, text=True,
                              env=env, timeout=1800, check=False)
        if proc.returncode:
            raise MemorySourceError(f'{arm} isolated preparation failed (exit {proc.returncode})')
        return MemoryPacket(**json.loads(proc.stdout))
    except (subprocess.TimeoutExpired, ValueError) as exc:
        raise MemorySourceError(f'{arm} isolated preparation failed: {type(exc).__name__}') from exc


if __name__ == '__main__':
    import contextlib
    arguments = json.load(sys.stdin)
    arguments['transcript_bytes'] = base64.b64decode(arguments['transcript_bytes'])
    module = importlib.import_module(f'brainmark.memsources.{sys.argv[1]}_source')
    with contextlib.redirect_stdout(sys.stderr):
        packet = module.build(**arguments, _isolated=True)
    print(json.dumps(dataclasses.asdict(packet)))
