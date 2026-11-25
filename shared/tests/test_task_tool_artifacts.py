"""Actual shared completion API retains uploaded native output references."""
import os
from pathlib import Path
import sys
import unittest
from unittest.mock import Mock,patch
sys.path.insert(0,str(Path(__file__).resolve().parents[1]/'lib'))
from task_recovery import Recovery
from task_operation import begin

class ToolArtifactTests(unittest.TestCase):
    def test_terminal_receipt_and_output_are_committed_in_the_same_checkpoint(self):
        r=Recovery.__new__(Recovery)
        identity={'task_id':'child','team_id':'team','generation':2,'instance_id':'i','process_id':'p'}
        op,cp=begin(None,['team','child'],[{'id':'write','name':'Write','arguments':{'file_path':'result.json'}}])
        r.tools_current=Mock(return_value={'execution':{'checkpoint':cp}})
        r.checkpoint=Mock(side_effect=lambda _task,_team,value,**_kw:value)
        artifact={'key':'teams/team/shared/tasks/child/checkpoints/outputs/result.json','sha256':'a'*64,'size':11}
        with patch.dict(os.environ,AGENTTEAMS_STORAGE_PREFIX='private/bucket'),patch('task_recovery.subprocess.run') as upload:
            result=r.finish_tools(identity,op,[{'id':'write','content':[artifact]}],artifacts=[artifact,artifact])
        self.assertEqual(result['operation']['state'],'verified')
        self.assertEqual(result['artifacts'].count(artifact),1)
        self.assertEqual(len(result['artifacts']),2)
        self.assertEqual(r.checkpoint.call_args.kwargs['expected_identity'],identity)
        upload.assert_called_once()
