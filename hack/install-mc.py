"""Fetch the pinned Linux/amd64 mc, with one official API fallback and SHA check."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import urllib.error
import urllib.request

VERSION = 'RELEASE.2025-08-13T08-35-41Z'
ASSET = 'mc.linux-amd64.' + VERSION
URL = f'https://github.com/minio/mc/releases/download/{VERSION}/{ASSET}'
API = 'https://api.github.com/repos/minio/mc/releases'
SHA256 = '01f866e9c5f9b87c2b09116fa5d7c06695b106242d829a8bb32990c00312e891'


def request(url, binary=False):
    headers = {'User-Agent': 'AgentTeams-CI'}
    if url.startswith('https://api.github.com/'):
        headers['Accept'] = 'application/octet-stream' if binary else 'application/vnd.github+json'
        if os.environ.get('GITHUB_TOKEN'):
            headers['Authorization'] = 'Bearer ' + os.environ['GITHUB_TOKEN']
    return urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30)


def install(destination):
    destination = Path(destination)
    with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as temporary:
        path = Path(temporary.name)
    try:
        try:
            response = request(URL)
        except (urllib.error.URLError, TimeoutError):
            with request(f'{API}/tags/{VERSION}') as metadata:
                assets = json.load(metadata)['assets']
            asset = next(item for item in assets if item['name'] == ASSET)
            response = request(f'{API}/assets/{int(asset["id"])}', binary=True)
        with response, path.open('wb') as output:
            shutil.copyfileobj(response, output)
        if hashlib.sha256(path.read_bytes()).hexdigest() != SHA256:
            raise ValueError('mc SHA256 mismatch; refusing to install')
        path.chmod(0o755)
        os.replace(path, destination)
    finally:
        path.unlink(missing_ok=True)


if __name__ == '__main__':
    install(sys.argv[1])
    print(f'Installed verified mc {VERSION} (linux/amd64)')
