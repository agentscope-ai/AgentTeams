"""Choose CI cost from the actual diff; release tags keep the full matrix."""
import argparse
import json
import os
import re
import subprocess


def git(*args):
    return subprocess.check_output(['git', *args], stderr=subprocess.DEVNULL)


def plan(base, head, event, ref):
    full = {'run_builds': True, 'run_models': True, 'base_changed': True}
    if event == 'workflow_dispatch' or ref.startswith('refs/tags/') or not base:
        return {**full, 'reason': 'release/manual or unavailable diff; full coverage'}
    try:
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


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('base', 'head', 'event', 'ref'):
        parser.add_argument('--' + name, default='HEAD' if name == 'head' else '')
    args = parser.parse_args()
    result = plan(args.base, args.head, args.event, args.ref)
    print(json.dumps(result))
    if os.environ.get('GITHUB_OUTPUT'):
        with open(os.environ['GITHUB_OUTPUT'], 'a') as output:
            for key, value in result.items():
                output.write(f'{key}={str(value).lower() if isinstance(value, bool) else value}\n')
