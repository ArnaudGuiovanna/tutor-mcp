#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Real ACP initialize of both installed agents, with no external Node on PATH."""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import tempfile

def verify(root):
    manifest=json.loads((root/'runtime-manifest.json').read_text(encoding='utf-8'))
    for asset in manifest['files']:
        relative=PurePosixPath(asset['path'])
        if relative.is_absolute() or '..' in relative.parts or '\\' in asset['path'] or ':' in asset['path']:
            raise ValueError('unsafe packaged asset')
        path=root.joinpath(*relative.parts)
        if not path.is_file() or path.is_symlink() or not path.resolve().is_relative_to(root.resolve()) or path.stat().st_size!=asset['bytes']:
            raise ValueError('missing/escaped asset')
        with path.open('rb') as source:
            if hashlib.file_digest(source,'sha256').hexdigest()!=asset['sha256']:
                raise ValueError('changed asset')
    with tempfile.TemporaryDirectory(prefix='tutor-packaged-acp-') as home:
        env=os.environ.copy()
        for key in list(env):
            if any(word in key.upper() for word in ['TOKEN','API_KEY','CREDENTIAL','ANTHROPIC','OPENAI','CLAUDE','CODEX']):
                del env[key]
        env.update({'PATH':str(root/'node'),'CODEX_HOME':home,'CLAUDE_CONFIG_DIR':home,'HOME':home,'USERPROFILE':home})
        for agent in manifest['agents']:
            process=subprocess.Popen([str(root/manifest['node']),str(root/agent['entry'])],cwd=home,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,text=True,encoding='utf-8')
            pool=concurrent.futures.ThreadPoolExecutor(max_workers=1)
            try:
                process.stdin.write(json.dumps({'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':1,'clientCapabilities':{},'clientInfo':{'name':'tutor-package-test','version':'0.1.0'}}})+'\n')
                process.stdin.flush()
                response=json.loads(pool.submit(process.stdout.readline).result(timeout=45))
                if response.get('id')!=1 or response.get('result',{}).get('protocolVersion')!=1 or not response['result'].get('agentInfo'):
                    raise ValueError(f"packaged {agent['id']} failed ACP initialize")
                print(f"Packaged {agent['id']} ACP v1: OK, no external Node")
            finally:
                process.kill()
                process.wait(timeout=10)
                pool.shutdown(wait=True,cancel_futures=True)

if __name__=='__main__':
    verify(Path(sys.argv[1]).resolve())
