#!/usr/bin/env python3
# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
"""Real ACP initialize of both installed agents, with no external Node on PATH."""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import signal
import subprocess
import sys
import tempfile
import time

class AgentProcess:
    """Own the complete test process tree, including native vendor children."""
    def __init__(self, argv, **kwargs):
        self.job = None
        self.process = subprocess.Popen(argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, encoding='utf-8', start_new_session=os.name != 'nt',
            creationflags=0x4 if os.name == 'nt' else 0, **kwargs)  # CREATE_SUSPENDED
        if os.name == 'nt':
            try:
                self._assign_windows_job()
                self._resume_windows_process()
            except BaseException:
                if self.job:
                    self.job[0].CloseHandle(self.job[1])
                    self.job = None
                self.process.kill()
                self.process.wait(timeout=10)
                raise

    def _assign_windows_job(self):
        # Same kill-on-close ownership as the application's Rust ACP supervisor.
        # ctypes preserves Windows' pointer-sized fields on the x64 CI runner.
        import ctypes
        from ctypes import wintypes
        class Basic(ctypes.Structure):
            _fields_ = [('process_time',ctypes.c_int64),('job_time',ctypes.c_int64),('flags',wintypes.DWORD),
                ('min_working_set',ctypes.c_size_t),('max_working_set',ctypes.c_size_t),('active_processes',wintypes.DWORD),
                ('affinity',ctypes.c_size_t),('priority',wintypes.DWORD),('scheduling',wintypes.DWORD)]
        class Counters(ctypes.Structure):
            _fields_ = [(name,ctypes.c_uint64) for name in ['reads','writes','other','read_bytes','write_bytes','other_bytes']]
        class Extended(ctypes.Structure):
            _fields_ = [('basic',Basic),('io',Counters),('process_memory',ctypes.c_size_t),('job_memory',ctypes.c_size_t),
                ('peak_process_memory',ctypes.c_size_t),('peak_job_memory',ctypes.c_size_t)]
        kernel = ctypes.WinDLL('kernel32', use_last_error=True)
        kernel.CreateJobObjectW.argtypes = [ctypes.c_void_p,wintypes.LPCWSTR]
        kernel.CreateJobObjectW.restype = wintypes.HANDLE
        kernel.SetInformationJobObject.argtypes = [wintypes.HANDLE,ctypes.c_int,ctypes.c_void_p,wintypes.DWORD]
        kernel.SetInformationJobObject.restype = wintypes.BOOL
        kernel.AssignProcessToJobObject.argtypes = [wintypes.HANDLE,wintypes.HANDLE]
        kernel.AssignProcessToJobObject.restype = wintypes.BOOL
        kernel.CloseHandle.argtypes = [wintypes.HANDLE]
        kernel.CloseHandle.restype = wintypes.BOOL
        handle = kernel.CreateJobObjectW(None,None)
        if not handle:
            raise ctypes.WinError(ctypes.get_last_error())
        info = Extended()
        info.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not kernel.SetInformationJobObject(handle,9,ctypes.byref(info),ctypes.sizeof(info)) or not kernel.AssignProcessToJobObject(handle,wintypes.HANDLE(int(self.process._handle))):
            error = ctypes.WinError(ctypes.get_last_error())
            kernel.CloseHandle(handle)
            raise error
        self.job = (kernel,handle)

    def _resume_windows_process(self):
        # Popen closes the primary thread handle. Find that still-suspended
        # thread through the documented Toolhelp API; no agent code has run yet.
        import ctypes
        from ctypes import wintypes
        class Thread(ctypes.Structure):
            _fields_=[('size',wintypes.DWORD),('usage',wintypes.DWORD),('id',wintypes.DWORD),
                ('process',wintypes.DWORD),('priority',wintypes.LONG),('delta',wintypes.LONG),('flags',wintypes.DWORD)]
        kernel,_=self.job
        kernel.CreateToolhelp32Snapshot.argtypes=[wintypes.DWORD,wintypes.DWORD]
        kernel.CreateToolhelp32Snapshot.restype=wintypes.HANDLE
        kernel.Thread32First.argtypes=[wintypes.HANDLE,ctypes.POINTER(Thread)]
        kernel.Thread32First.restype=wintypes.BOOL
        kernel.Thread32Next.argtypes=[wintypes.HANDLE,ctypes.POINTER(Thread)]
        kernel.Thread32Next.restype=wintypes.BOOL
        kernel.OpenThread.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD]
        kernel.OpenThread.restype=wintypes.HANDLE
        kernel.ResumeThread.argtypes=[wintypes.HANDLE]
        kernel.ResumeThread.restype=wintypes.DWORD
        snapshot=kernel.CreateToolhelp32Snapshot(0x4,0)  # TH32CS_SNAPTHREAD
        if snapshot==ctypes.c_void_p(-1).value:
            raise ctypes.WinError(ctypes.get_last_error())
        try:
            thread=Thread();thread.size=ctypes.sizeof(Thread)
            found=kernel.Thread32First(snapshot,ctypes.byref(thread))
            while found:
                if thread.process==self.process.pid:
                    handle=kernel.OpenThread(0x2,False,thread.id)  # THREAD_SUSPEND_RESUME
                    if not handle:
                        raise ctypes.WinError(ctypes.get_last_error())
                    try:
                        if kernel.ResumeThread(handle)==0xffffffff:
                            raise ctypes.WinError(ctypes.get_last_error())
                        return
                    finally:
                        kernel.CloseHandle(handle)
                found=kernel.Thread32Next(snapshot,ctypes.byref(thread))
            raise OSError('Suspended agent primary thread not found')
        finally:
            kernel.CloseHandle(snapshot)

    def _stop_windows_job(self):
        import ctypes
        from ctypes import wintypes
        class Accounting(ctypes.Structure):
            _fields_=[(name,ctypes.c_int64) for name in ['user','kernel','period_user','period_kernel']]+[
                (name,wintypes.DWORD) for name in ['page_faults','total','active','terminated']]
        kernel,handle=self.job
        kernel.TerminateJobObject.argtypes=[wintypes.HANDLE,wintypes.UINT]
        kernel.TerminateJobObject.restype=wintypes.BOOL
        kernel.QueryInformationJobObject.argtypes=[wintypes.HANDLE,ctypes.c_int,ctypes.c_void_p,wintypes.DWORD,ctypes.c_void_p]
        kernel.QueryInformationJobObject.restype=wintypes.BOOL
        try:
            if not kernel.TerminateJobObject(handle,1):
                raise ctypes.WinError(ctypes.get_last_error())
            deadline=time.monotonic()+10
            while True:
                info=Accounting()
                if not kernel.QueryInformationJobObject(handle,1,ctypes.byref(info),ctypes.sizeof(info),None):
                    raise ctypes.WinError(ctypes.get_last_error())
                if info.active==0:
                    return
                if time.monotonic()>=deadline:
                    raise TimeoutError('Agent descendants did not terminate')
                time.sleep(.02)
        finally:
            kernel.CloseHandle(handle)
            self.job=None

    def stop(self):
        try:
            self.process.stdin.close()  # Let the ACP transport close cleanly first.
        except (BrokenPipeError,OSError):
            pass
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            pass
        finally:
            if self.job:
                self._stop_windows_job()
            elif os.name != 'nt':
                try:
                    os.killpg(self.process.pid,signal.SIGKILL)
                except ProcessLookupError:
                    pass
            if self.process.poll() is None:
                self.process.kill()
            self.process.wait(timeout=10)
            self.process.stdout.close()

def remove_temporary_home(path):
    import shutil
    deadline = time.monotonic() + 5
    while True:
        try:
            shutil.rmtree(path)
            return
        except PermissionError:
            if time.monotonic() >= deadline:
                raise
            time.sleep(.1)  # Windows may still be releasing terminated handles.

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
    home = tempfile.mkdtemp(prefix='tutor-packaged-acp-')
    try:
        env=os.environ.copy()
        for key in list(env):
            if any(word in key.upper() for word in ['TOKEN','API_KEY','CREDENTIAL','ANTHROPIC','OPENAI','CLAUDE','CODEX']):
                del env[key]
        env.update({'PATH':str(root/'node'),'CODEX_HOME':home,'CLAUDE_CONFIG_DIR':home,'HOME':home,'USERPROFILE':home})
        for agent in manifest['agents']:
            owner=AgentProcess([str(root/manifest['node']),str(root/agent['entry'])],cwd=home,env=env)
            process=owner.process
            pool=concurrent.futures.ThreadPoolExecutor(max_workers=1)
            try:
                process.stdin.write(json.dumps({'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':1,'clientCapabilities':{},'clientInfo':{'name':'tutor-package-test','version':'0.1.0'}}})+'\n')
                process.stdin.flush()
                response=json.loads(pool.submit(process.stdout.readline).result(timeout=45))
                if response.get('id')!=1 or response.get('result',{}).get('protocolVersion')!=1 or not response['result'].get('agentInfo'):
                    raise ValueError(f"packaged {agent['id']} failed ACP initialize")
                print(f"Packaged {agent['id']} ACP v1: OK, no external Node")
            finally:
                try:
                    owner.stop()
                finally:
                    pool.shutdown(wait=True,cancel_futures=True)
    finally:
        remove_temporary_home(home)

if __name__=='__main__':
    verify(Path(sys.argv[1]).resolve())
