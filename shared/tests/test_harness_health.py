import datetime
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

MODULE = Path(__file__).resolve().parents[1] / "lib/harness_health.py"
spec = importlib.util.spec_from_file_location("harness_health", MODULE)
health = importlib.util.module_from_spec(spec)
spec.loader.exec_module(health)


class HarnessHealthTests(unittest.TestCase):
    def test_only_normalized_fields_cross_the_boundary(self):
        for result, expected in [
                ({"status":"ok","reason":"ok","api_key":"private"}, {"status":"ok","reason":"ok","configuration":""}),
                ({"status":"failed","reason":"private-key"}, {"status":"failed","reason":"invalid_response"}),
                ({"status":"ok","reason":"network"}, {"status":"failed","reason":"invalid_response"}),
                ({"status":"unknown","reason":"authentication"}, {"status":"failed","reason":"invalid_response"})]:
            with self.subTest(result=result), patch.object(health.subprocess,"run",return_value=Mock(returncode=0,stdout=json.dumps(result).encode())):
                self.assertEqual(health.invoke(),expected)

    def test_timeout_and_missing_adapter_cannot_report_success(self):
        for error, expected in [(FileNotFoundError(),"unsupported"),(subprocess.TimeoutExpired("adapter",25),"timeout")]:
            with patch.object(health.subprocess,"run",side_effect=error):
                result=health.invoke()
            self.assertNotEqual(result["status"],"ok")
            self.assertEqual(result["reason"],expected)

    def test_readiness_never_probes_a_model_or_guesses_by_runtime(self):
        with patch.object(health.subprocess,"run",return_value=Mock(returncode=0,stdout=b'{"version":1,"status":"ready","evidence":"native_status"}')) as run:
            self.assertEqual(health.readiness()["status"],"ready")
            self.assertEqual(run.call_args.args[0], [health.COMMAND,"--readiness"])
        with patch.object(health.subprocess,"run",return_value=Mock(returncode=0,stdout=b'{"ok":true}')):
            self.assertEqual(health.readiness()["status"],"starting")

    def test_report_is_member_scoped_and_not_published_after_process_exit(self):
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX="test/bucket"), patch.object(health,"invoke",return_value={"status":"ok","reason":"ok"}), patch.object(health.subprocess,"run") as run:
            child=Mock();child.poll.return_value=None
            reporter=health.Reporter("example","alice",child)
            reporter.check("a"*32)
            command=run.call_args.args[0]
            self.assertEqual(command,["mc","pipe","test/bucket/agents/alice/runtime/health.json"])
            report=json.loads(run.call_args.kwargs["input"])
            self.assertEqual((report["version"],report["runtime"],report["member"],report["request_id"]),(1,"example","alice","a"*32))
            run.reset_mock();child.poll.return_value=0
            with self.assertRaises(OSError):reporter.check()
            run.assert_not_called()

    def test_expired_or_malformed_probe_requests_are_ignored(self):
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX="test/bucket"):
            reporter=health.Reporter("example","alice",Mock())
            for version, request_id, expires, expected in [(1,"a"*32,60,"a"*32),(2,"a"*32,60,None),(1,"secret-key",60,None),(1,"a"*32,-1,None)]:
                data=json.dumps({"version":version,"id":request_id,"expires_at":(health.now()+datetime.timedelta(seconds=expires)).isoformat()}).encode()
                with patch.object(health.subprocess,"run",return_value=Mock(returncode=0,stdout=data)):
                    self.assertEqual(reporter.request(),expected)

    def test_lost_upload_ack_reuses_probe_result(self):
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX="test/bucket"), patch.object(health,"invoke",return_value={"status":"ok","reason":"ok"}) as probe, patch.object(health.subprocess,"run",side_effect=[subprocess.CalledProcessError(1,"mc"),Mock()]) as upload:
            child=Mock();child.poll.return_value=None
            reporter=health.Reporter("example","alice",child)
            with self.assertRaises(subprocess.SubprocessError):reporter.check("a"*32)
            reporter.check("a"*32)
            self.assertEqual(probe.call_count,1)
            self.assertEqual(upload.call_args_list[0].kwargs["input"],upload.call_args_list[1].kwargs["input"])

    def test_removed_configuration_invalidates_success(self):
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX="test/bucket"), patch.object(health.time,"sleep"), patch.object(health.time,"monotonic",return_value=10):
            child=Mock();child.poll.side_effect=[None,None,0]
            reporter=health.Reporter("example","alice",child)
            reporter.request=Mock(return_value=None)
            reporter.configuration=Mock(side_effect=["a"*64,None])
            def check(_):
                reporter.last={"status":"ok","configuration":"a"*64} if reporter.check.call_count==1 else {"status":"unknown","reason":"configuration"}
                return reporter.last
            reporter.check=Mock(side_effect=check)
            reporter.run()
            self.assertEqual(reporter.check.call_count,2)

    def test_configuration_change_bypasses_success_cache(self):
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX="test/bucket"), patch.object(health.time,"sleep"), patch.object(health.time,"monotonic",return_value=10):
            child=Mock();child.poll.side_effect=[None,None,0]
            reporter=health.Reporter("example","alice",child)
            reporter.request=Mock(return_value=None)
            reporter.configuration=Mock(side_effect=["a"*64,"b"*64])
            def check(_):
                reporter.last={"status":"ok","configuration":("a" if reporter.check.call_count==1 else "b")*64}
                return reporter.last
            reporter.check=Mock(side_effect=check)
            reporter.run()
            self.assertEqual(reporter.check.call_count,2)


if __name__ == "__main__":
    unittest.main()
