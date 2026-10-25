# Copyright (c) 2026 Arnaud Guiovanna
# SPDX-License-Identifier: MIT
import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest

spec=importlib.util.spec_from_file_location('packaged_agents',Path(__file__).with_name('verify-packaged-agents.py'))
agents=importlib.util.module_from_spec(spec)
spec.loader.exec_module(agents)

class PackagedProcessTests(unittest.TestCase):
    def test_stop_releases_the_parent_and_its_native_child_working_directory(self):
        with tempfile.TemporaryDirectory() as root:
            work=Path(root)/'work';work.mkdir()
            marker=work/'heartbeat'
            child="import pathlib,sys,time\np=pathlib.Path(sys.argv[1])\nwhile True:\n p.write_text(str(time.time_ns()))\n time.sleep(.02)"
            parent="import subprocess,sys,time\nsubprocess.Popen([sys.executable,'-c',sys.argv[1],sys.argv[2]])\ntime.sleep(60)"
            owner=agents.AgentProcess([sys.executable,'-c',parent,child,str(marker)],cwd=work,env=os.environ.copy())
            try:
                deadline=time.monotonic()+10
                while not marker.exists() and time.monotonic()<deadline:
                    time.sleep(.02)
                self.assertTrue(marker.exists(),'native child never started')
            finally:
                owner.stop()
            self.assertIsNotNone(owner.process.poll())
            last=marker.read_text()
            time.sleep(.15)
            self.assertEqual(marker.read_text(),last,'a child survived the test supervisor')
            marker.unlink()
            work.rmdir()  # Windows refuses this while any descendant keeps cwd open.

if __name__=='__main__':
    unittest.main()
