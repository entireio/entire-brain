#!/usr/bin/env python3
"""Hermetic cross-binary contract test. No real plugins, state, or daemons are touched."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

parser = argparse.ArgumentParser()
parser.add_argument('--graph-binary', required=True, type=Path)
parser.add_argument('--brain-binary', required=True, type=Path)
args = parser.parse_args()
binaries = {'graph': args.graph_binary.resolve(), 'brain': args.brain_binary.resolve()}
with tempfile.TemporaryDirectory(prefix='agent-coordination-') as tmp:
    root = Path(tmp)
    env = {k: v for k, v in os.environ.items() if not k.startswith(('ENTIRE_', 'GIT_'))}
    for key in ('HOME', 'XDG_STATE_HOME', 'XDG_CONFIG_HOME', 'XDG_DATA_HOME', 'XDG_CACHE_HOME'):
        env[key] = str(root / key)
        Path(env[key]).mkdir()
    stub = root / 'bin'
    stub.mkdir()
    log = root / 'calls'
    log.write_text('')
    (stub / 'entire').write_text('''#!/bin/sh
printf '%s\\n' "$*" >> "$TEST_CALLS"
if [ "$#" != 2 ] || [ "$1" != plugin ] || [ "$2" != list ]; then exit 97; fi
if [ "$TEST_LIST_FAILURE" = 1 ]; then exit 98; fi
if [ -z "$TEST_PLUGINS" ]; then
  echo "No plugins installed in /fixture."
  echo "Install one with 'entire plugin install <name|url|path>', or drop an entire-<name> binary anywhere on \u0024PATH."
else
  echo "Managed plugin directory: /fixture"
  echo
  for name in $TEST_PLUGINS; do printf '  %-20s %-18s → /fixture/%s\\n' "$name" v1.0.0 "$name"; done
fi
''')
    (stub / 'entire').chmod(0o755)
    env['PATH'] = str(stub) + os.pathsep + env['PATH']
    env['TEST_CALLS'] = str(log)
    env['TEST_PLUGINS'] = 'graph brain'

    def run(tool, repo, command, success=True, explicit=True, cwd=None, extra=()):
        argv = [str(binaries[tool]), command, *extra]
        if explicit:
            argv += ['--repo', str(repo)]
        result = subprocess.run(argv, cwd=cwd or root, env=env, capture_output=True, text=True)
        assert (result.returncode == 0) == success, (argv, result.stdout, result.stderr)
        return result.stdout

    def snapshot(repo):
        return {str(p.relative_to(repo)): p.read_bytes() for p in repo.rglob('*') if p.is_file() and '.git' not in p.parts}

    def project(name, brain=False):
        repo = root / name
        repo.mkdir()
        subprocess.run(['git', 'init', '-q', str(repo)], env=env, check=True)
        subprocess.run(['git', '-C', str(repo), 'remote', 'add', 'origin', 'https://github.com/test/' + name], env=env, check=True)
        setup = Path(env['XDG_STATE_HOME']) / 'entire/repos/gh/test' / name / 'setup.json'
        if brain:
            setup.parent.mkdir(parents=True)
            setup.write_text(json.dumps({'schema_version': 1, 'updated_at': '2000-01-01T00:00:00Z'}))
        return repo, setup

    def check(repo, product, mode):
        text = (repo / '.entire/agent-guide.md').read_text()
        assert run(product, repo, 'agent-guide') == text
        assert text.splitlines()[0] == '# Entire repository agent guide — ' + mode
        # 'FIRST action' and 'SEARCH FIRST' used to be banned here too. They are
        # not runtime probes; they are the directive that makes the product get
        # used, and banning them was benchmark arm-fairness doctrine applied to
        # shipped text. The bans that remain are the genuine ones: a guide must
        # not send an agent probing its own installation.
        for forbidden in ('entire plugin list', 'command -v', 'setup.json', 'if Brain is installed', 'entire graph version', 'entire brain version'):
            assert forbidden not in text
        # The directive itself, and only verbs a released graph exposes (#323).
        assert 'MUST be ONE Graph search' in text or product == 'brain', text[:400]
        assert 'entire graph query' not in text
        for name in ('AGENTS.md', 'CLAUDE.md'):
            content = (repo / name).read_text()
            assert content.count('<!-- entire-agent:begin -->') == 1
            assert '<!-- entire-graph:begin -->' not in content
            assert '<!-- entire-brain:begin -->' not in content

    for first in ('graph', 'brain'):
        repo, setup = project(first, brain=True)
        original = 'User prefix\n<!-- entire-graph:begin -->\nold graph\n<!-- entire-graph:end -->\nMiddle\n<!-- entire-brain:begin -->\nold brain\n<!-- entire-brain:end -->\nUser suffix\n'
        (repo / 'AGENTS.md').write_text(original)
        (repo / 'CLAUDE.md').write_text('@AGENTS.md\n')
        (repo / '.entire').mkdir()
        for legacy in ('graph-agent.md', 'brain-agent.md'):
            (repo / '.entire' / legacy).write_text('# Entire repository agent guide — Graph and Brain\n')
        second = 'brain' if first == 'graph' else 'graph'
        before = None
        for product in (first, second, first, second):
            run(product, repo, 'init-agents')
            check(repo, product, 'Graph and Brain')
            assert run('graph', repo, 'agent-guide') == run('brain', repo, 'agent-guide')
            if before is not None:
                assert snapshot(repo) == before
            before = snapshot(repo)
        content = (repo / 'AGENTS.md').read_text()
        for user_text in ('User prefix\n', '\nMiddle\n', '\nUser suffix\n'):
            assert user_text in content
        assert sum('Begin substantive tasks' in p.read_text() for p in (repo / '.entire').glob('*.md')) == 1
        setup.unlink()
        run('graph', repo, 'init-agents')
        check(repo, 'graph', 'Graph and Brain')
        # Runtime state removal must not remove repository activation.
        run('brain', repo, 'init-agents')
        check(repo, 'brain', 'Graph and Brain')

    for product in ('graph', 'brain'):
        env['TEST_PLUGINS'] = product
        repo, _ = project('only-' + product)
        run(product, repo, 'init-agents')
        check(repo, product, product.title())

    env['TEST_PLUGINS'] = 'brain'
    repo, _ = project('json-report')
    report = json.loads(run('brain', repo, 'init-agents', extra=('--json',)))
    assert report['changed_files'] == ['.entire/agent-guide.md', 'AGENTS.md', 'CLAUDE.md']
    report = json.loads(run('brain', repo, 'init-agents', extra=('--json',)))
    assert report['changed_files'] == []

    env['TEST_PLUGINS'] = 'graph brain'
    for first in ('graph', 'brain'):
        repo, _ = project('empty-' + first)
        second = 'brain' if first == 'graph' else 'graph'
        # Preview must not persist activation.
        run(second, repo, 'agent-guide')
        assert snapshot(repo) == {}
        run(first, repo, 'init-agents')
        check(repo, first, first.title())
        run(second, repo, 'init-agents')
        check(repo, second, 'Graph and Brain')
        before = snapshot(repo)
        env['TEST_LIST_FAILURE'] = '1'
        for product in (first, second):
            run(product, repo, 'init-agents')
            assert snapshot(repo) == before
        del env['TEST_LIST_FAILURE']

    repo, setup = project('runtime-independent', brain=True)
    setup.write_text('{malformed runtime state')
    run('graph', repo, 'init-agents')
    check(repo, 'graph', 'Graph')
    before = snapshot(repo)
    guide_path = repo / '.entire/agent-guide.md'
    guide_path.write_text(guide_path.read_text().replace('"schema_version":1', '"schema_version":99'))
    damaged = snapshot(repo)
    for product in ('graph', 'brain'):
        run(product, repo, 'init-agents', success=False)
        run(product, repo, 'agent-guide', success=False)
        assert snapshot(repo) == damaged
    guide_path.write_bytes(before['.entire/agent-guide.md'])

    # Strict mode is shared and sticky, in either initialization order.
    for first in ('graph', 'brain'):
        second = 'brain' if first == 'graph' else 'graph'
        repo, _ = project('strict-' + first)
        before = snapshot(repo)
        preview = run(first, repo, 'agent-guide', extra=('--strict',))
        assert snapshot(repo) == before
        run(first, repo, 'init-agents', extra=('--strict',))
        assert (repo / '.entire/agent-guide.md').read_text() == preview
        run(second, repo, 'init-agents')
        strict = (repo / '.entire/agent-guide.md').read_text()
        assert '"mode":"strict"' in strict
        assert 'ALWAYS run impact before editing' in strict
        assert 'ALWAYS begin a substantive task' in strict
        before = snapshot(repo)
        for product in (first, second):
            assert run(product, repo, 'agent-guide') == strict
            normal = run(product, repo, 'agent-guide', extra=('--normal',))
            assert '"mode":"strict"' not in normal
            assert 'Skip ceremonial queries' in normal
            assert snapshot(repo) == before
            run(product, repo, 'init-agents')
            assert snapshot(repo) == before
            for command in ('init-agents', 'agent-guide'):
                run(product, repo, command, extra=('--strict', '--normal'), success=False)
                assert snapshot(repo) == before
        assert run('brain', repo, 'guide') == strict
        assert run('brain', repo, 'guide', extra=('--normal',)) == normal
        run(second, repo, 'init-agents', extra=('--normal',))
        assert (repo / '.entire/agent-guide.md').read_text() == normal
        run(first, repo, 'init-agents')
        assert run(first, repo, 'agent-guide') == normal
        assert run(second, repo, 'agent-guide') == normal
        run(first, repo, 'init-agents', extra=('--strict',))
        assert run(second, repo, 'agent-guide') == strict

    # Standalone binaries need neither the host nor any other executable on PATH.
    saved_path = env['PATH']
    no_host = root / 'no-host'
    no_host.mkdir()
    for product in ('graph', 'brain'):
        repo, _ = project('no-host-' + product, brain=True)
        env['PATH'] = str(no_host)
        calls_before = log.read_text()
        run(product, repo, 'init-agents')
        check(repo, product, product.title())
        before = snapshot(repo)
        run(product, repo, 'init-agents')
        assert snapshot(repo) == before
        assert log.read_text() == calls_before
        env['PATH'] = saved_path

    # Default context and explicit overrides, including read-only outside-repo preview.
    repo, _ = project('context', brain=True)
    sub = repo / 'sub'
    sub.mkdir()
    run('graph', repo, 'init-agents', explicit=False, cwd=sub)
    check(repo, 'graph', 'Graph')
    env['ENTIRE_REPO_ROOT'] = str(repo)
    assert run('graph', repo, 'agent-guide', explicit=False) == (repo / '.entire/agent-guide.md').read_text()
    del env['ENTIRE_REPO_ROOT']
    calls_before = log.read_text()
    for product in ('graph', 'brain'):
        assert run(product, None, 'agent-guide', explicit=False).startswith('# Entire repository agent guide — ' + product.title() + '\n')
        run(product, None, 'init-agents', explicit=False, success=False)
    assert log.read_text() == calls_before
    assert log.read_text() == '', 'agent activation queried or dispatched a plugin'

# Activation, rendering, and filesystem protections must remain mirrored.
# Historical runtime-detection helpers already differ between these repositories
# and no longer participate in agent activation.
brain_source = Path(__file__).resolve().parents[1] / 'internal/agentsetup'
graph_source = Path(__file__).resolve().parents[2] / 'entire-graph/internal/agentsetup'
if graph_source.exists():
    legacy_runtime = {'brain_state.go', 'brain_manifest_test.go'}
    brain_files = {p.name for p in brain_source.iterdir() if p.is_file()} - legacy_runtime
    graph_files = {p.name for p in graph_source.iterdir() if p.is_file()} - legacy_runtime
    assert brain_files == graph_files, (brain_files ^ graph_files)
    for name in sorted(brain_files):
        assert (brain_source / name).read_bytes() == (graph_source / name).read_bytes(), name
print('PASS: compiled CLI modes, standalone without host, both orders, migration, regeneration, removal, strict/normal persistence, failures, preview parity, context, byte stability, no plugin dispatch')
