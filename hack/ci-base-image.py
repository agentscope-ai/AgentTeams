"""Resolve a published prerequisite once; uncertain registry errors fail closed."""
import json
import os
import re
import subprocess
import sys

reference = sys.argv[1]
result = subprocess.run(['docker', 'buildx', 'imagetools', 'inspect', reference,
                         '--format', '{{.Manifest.Digest}}'],
                        capture_output=True, text=True, timeout=90)
rebuild = False
immutable = ''
if result.returncode:
    if result.stderr.strip() != f'ERROR: {reference}: not found':
        raise SystemExit(f'Cannot verify {reference}: {result.stderr.strip()}')
    rebuild = True
else:
    digest = result.stdout.strip()
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
        raise SystemExit(f'Cannot verify {reference}: missing immutable index digest')
    immutable = reference.rsplit(':', 1)[0] + '@' + digest
    result = subprocess.run(['docker', 'buildx', 'imagetools', 'inspect', immutable, '--raw'],
                            capture_output=True, text=True, timeout=90)
    if result.returncode:
        raise SystemExit(f'Cannot verify {immutable}: {result.stderr.strip()}')
    try:
        manifest = json.loads(result.stdout)
        architectures = {item.get('platform', {}).get('architecture')
                         for item in manifest['manifests']
                         if item.get('platform', {}).get('os') == 'linux'}
        assert manifest['schemaVersion'] == 2 and {'amd64', 'arm64'} <= architectures
    except (ValueError, KeyError, TypeError, AssertionError):
        raise SystemExit(f'Cannot verify {reference}: invalid or incomplete multi-architecture manifest')
output = {'openclaw_base': rebuild}
if not rebuild:
    output['openclaw_base_ref'] = immutable
print(json.dumps(output))
if os.environ.get('GITHUB_OUTPUT'):
    with open(os.environ['GITHUB_OUTPUT'], 'a') as file:
        for key, value in output.items():
            file.write(f'{key}={str(value).lower() if isinstance(value, bool) else value}\n')
