"""Choose CI cost from the actual diff; release tags keep the full matrix."""
import argparse
import json
import os
import re
import subprocess
from pathlib import Path


def git(*args):
    return subprocess.check_output(['git', *args], stderr=subprocess.DEVNULL)


def plan(base, head, event, ref):
    full = {'run_builds': True, 'run_models': True, 'base_changed': True}
    if event == 'workflow_dispatch' or ref.startswith('refs/tags/') or not base:
        return {**full, 'reason': 'release/manual or unavailable diff; full coverage'}
    try:
        if event == 'pull_request':
            base = git('merge-base', base, head).decode().strip()
        files = git('diff', '--name-only', '--no-renames', '-z', base, head).decode().rstrip('\0').split('\0')
    except subprocess.CalledProcessError:
        return {**full, 'reason': 'diff unavailable; full coverage'}
    pipeline = False
    runtime = False
    base_changed = False
    version = re.compile(r'(?<![\w.])v?\d+\.\d+\.\d+(?:[-+][\w.-]+)?(?![\w.])')
    for path in filter(None, files):
        base_changed |= path.startswith('openclaw-base/') or path in {
            'hack/versions.env', 'Makefile', '.github/workflows/test-integration.yml'}
        if path.startswith(('docs/', 'blog/', 'changelog/')) or re.fullmatch(r'README(?:\.[\w-]+)?\.md', path):
            continue
        if path.startswith(('install/', 'helm/agentteams/')):
            try:
                before, after = (git('show', f'{revision}:{path}').decode() for revision in (base, head))
                if version.sub('<version>', before) == version.sub('<version>', after):
                    continue
            except subprocess.CalledProcessError:
                pass
        if path.startswith('.github/workflows/') or path in {
            'hack/ci-integration-plan.py', 'hack/ci-base-image.py', 'hack/install-mc.py',
            'tests/test-build-workflows.py', 'tests/test-ci-release-tools.py', 'tests/check-integration-scheduling.py',
            'tests/test-release-metrics.py', 'tests/test-install-mc.py', 'tests/lib/agent-metrics.sh',
            'qwenpaw/tests/test_integration_workflow.py'}:
            pipeline = True
        else:
            runtime = True
    return {'run_builds': pipeline or runtime, 'run_models': runtime,
            'base_changed': base_changed,
            'reason': 'runtime/unknown changes' if runtime else 'pipeline contracts' if pipeline else 'docs/version pins only'}


INTEGRATION_HARNESSES = ('qwenpaw', 'hermes', 'deepseek', 'openclaw')
NATIVE_HARNESSES = ('codex', 'claudecode', 'openhuman')
HARNESS_LABELS = {f'ci:harness-{runtime}': runtime for runtime in INTEGRATION_HARNESSES + NATIVE_HARNESSES}


def select_harnesses(event, labels):
    names = {label.strip().casefold().replace('：', ':') for label in labels}
    requested = set(HARNESS_LABELS.values()) if 'ci:all' in names else {
        HARNESS_LABELS[name] for name in names if name in HARNESS_LABELS
    }
    all_requested = event == 'pull_request' and 'ci:all' in names
    if event == 'pull_request':
        selected = ('qwenpaw',) + tuple(
            runtime for runtime in INTEGRATION_HARNESSES[1:] + NATIVE_HARNESSES
            if runtime in requested)
    else:
        # Existing main, release tag and manual integration coverage.
        selected = INTEGRATION_HARNESSES
    return {
        'harnesses': list(selected),
        'native_harnesses': [runtime for runtime in NATIVE_HARNESSES if runtime in selected],
        'full_matrix': event != 'pull_request' or all_requested,
        'explicit_harnesses': event == 'pull_request' and bool(requested),
    }


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('base', 'head', 'event', 'ref'):
        parser.add_argument('--' + name, default='HEAD' if name == 'head' else '')
    args = parser.parse_args()
    result = plan(args.base, args.head, args.event, args.ref)
    labels = []
    if args.event == 'pull_request' and os.environ.get('GITHUB_EVENT_PATH'):
        payload = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
        labels = [label['name'] for label in payload['pull_request']['labels']]
    selection = select_harnesses(args.event, labels)
    if selection.pop('explicit_harnesses'):
        result.update(run_builds=True, run_models=True, reason='explicit Harness CI labels')
    result.update(selection)
    print(json.dumps(result))
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            for key, value in result.items():
                encoded = json.dumps(value) if isinstance(value, (bool, list)) else value
                output.write(f'{key}={encoded}\n')
