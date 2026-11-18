import copy
import io
import json
import unittest
from unittest.mock import patch

import manage as e
import test_evaluate as fixtures


class VideoTest(unittest.TestCase):
    def setUp(self):
        self.f = fixtures.EvaluationTest('runTest')
        self.f.setUp()
        self.addCleanup(self.f.doCleanups)
        data = self.f.data
        data['version'].update(engine='ltx', base_model='ltx-2.3-distilled', binding={'model': 'ltx'})
        item = data['target']['instance']
        item['protocol'] = 'video_generation'
        item['endpoint'] += '/gpu0'
        for field in ('health_url', 'local_health_url'):
            item[field] = item['endpoint'] + '/v1/videos/models/' + data['model_id'] + '/health'
        self.topology = self.f.root / 'instances.json'
        self.topology.write_text(json.dumps({'instances': [{'name': 'ltx', 'model': 'ltx', 'gpus': [0]}]}))
        data['version']['assets'].append({'name': 'topology', 'role': 'config', 'sha256': e.digest(self.topology), 'source': 'git://example/instances.json'})
        data['target']['asset_paths']['topology'] = str(self.topology)
        data['version']['environment']['VG_INSTANCES'] = '/weights/instances.json'
        case = data['acceptance'][0]
        case.update(expected_frames=25, expected_fps=24)
        case['expected_generation']['model_version.lora.sha256'] = e.digest(self.f.asset)
        item['artifact_digest'] = 'sha256-' + e.fingerprint(data['version'])
        self.f.d.path.write_text(json.dumps(data))
        self.d = e.Deployment(self.f.d.path)
        self.f.d = self.d
        self.runner = self.f.runner
        self.runner.d = self.d
        self.runner.timeout = 1
        self.gateway = self.f.gateway
        self.good = {'state': 'succeeded', 'instance_id': self.d.id, 'result': {'generation': {
            'model_version': {'sha256': e.digest(self.f.asset), 'lora': {'sha256': e.digest(self.f.asset)}}}}}

    def config(self):
        config = self.f.config()
        env = config['services']['worker']['environment']
        for name in ('VMIND_IMAGE_MODELS', 'VMIND_GENERATION_STATE', 'VMIND_WARMUP_READY_FILE'):
            env.pop(name)
        env.update(VG_DATA_DIR='/state/data', VG_JOB_STATE_DIR='/state/jobs', VG_INSTANCES='/weights/instances.json',
                   VMIND_VIDEO_MODELS=json.dumps({k: {'model': 'ltx'} for k in (self.d.item.model_key, self.d.validation_model_id)}))
        return config

    def test_video_bindings_persistent_state_and_single_gpu_topology(self):
        self.assertIn('/v1/videos/models/verify_', self.d.probe.health_url)
        with patch.object(e, 'run', return_value=json.dumps(self.config())):
            self.d.config()
        for problem in ('shared', 'state', 'binding', 'untracked'):
            config = self.config()
            env = config['services']['worker']['environment']
            if problem == 'shared':
                self.topology.write_text('{"instances":[{"model":"ltx","gpus":[0,1]}]}')
            else:
                self.topology.write_text('{"instances":[{"model":"ltx","gpus":[0]}]}')
            if problem == 'state': env['VG_JOB_STATE_DIR'] = '/ephemeral/jobs'
            if problem == 'binding': env['VMIND_VIDEO_MODELS'] = '{"production":{"model":"ltx"}}'
            if problem == 'untracked': env['VG_INSTANCES'] = '/other.json'
            with patch.object(e, 'run', return_value=json.dumps(config)), self.assertRaises(ValueError):
                self.d.config()

    def test_async_success_polls_same_request_and_checks_receipt(self):
        with patch.object(e, 'Gateway') as api, patch.object(self.gateway, 'call', side_effect=[{'state': 'queued'}, {'state': 'processing'}, self.good]) as get, patch.object(e.time, 'sleep'), patch.object(e.OPENER, 'open', return_value=io.BytesIO(b'video')), patch.object(e, 'run', return_value='{"decoded":true}'):
            self.runner.accept()
        self.assertEqual(api.return_value.call_with_headers.call_count, 1)
        self.assertEqual(api.return_value.call_with_headers.call_args.args[1], '/v1/videos')
        self.assertEqual(len({c.args[1] for c in get.call_args_list}), 1)
        self.assertEqual(self.f.audit.state['events'][-1]['phase'], 'acceptance_passed')

    def test_failure_unknown_execution_wrong_dispatch_and_lora_never_pass(self):
        for problem in ('failed', 'processing', 'dispatch', 'lora'):
            detail = copy.deepcopy(self.good)
            if problem in ('failed', 'processing'): detail['state'] = problem
            if problem == 'dispatch': detail['instance_id'] = 'production'
            if problem == 'lora': detail['result']['generation']['model_version']['lora']['sha256'] = 'wrong'
            self.runner.timeout = 0
            self.runner.audit = fixtures.MemoryAudit()
            with patch.object(e, 'Gateway') as api, patch.object(self.gateway, 'call', return_value=detail), patch.object(e.OPENER, 'open') as download, self.assertRaises(ValueError):
                self.runner.accept()
            self.assertEqual(api.return_value.call_with_headers.call_count, 1)
            download.assert_not_called()
            self.assertEqual([x['phase'] for x in self.runner.audit.state['events']], ['acceptance_intent'])

    def test_video_stop_requires_durable_and_worker_idle(self):
        idle = {'instances': [{'name': 'ltx', 'model': 'ltx', 'gpus': [0]}], 'queued_total': 0,
                'readiness': {'admitted': 0, 'workers': [{'name': 'ltx', 'busy': False}]}}
        self.runner.timeout = 0
        for problem in (None, 'busy', 'admitted', 'missing', 'multiple', 'queued'):
            data = copy.deepcopy(idle)
            if problem == 'busy': data['readiness']['workers'][0]['busy'] = True
            if problem == 'admitted': data['readiness']['admitted'] = 1
            if problem == 'missing': data.pop('readiness')
            if problem == 'multiple': data['instances'] *= 2
            if problem == 'queued': data['queued_total'] = 1
            self.gateway.rows = [self.f.row()]
            with patch.object(self.d, 'config'), patch.object(self.runner, 'containers', return_value=[]), patch.object(self.runner, 'owned', return_value={'State': {'Running': True}}), patch.object(e, 'read_json', return_value=data), patch.object(e, 'run') as run:
                if problem:
                    with self.assertRaises(TimeoutError): self.runner.stop()
                    run.assert_not_called()
                else:
                    self.runner.stop()
                    self.assertIn('stop', run.call_args.args[0])


class VideoSwitchTest(unittest.TestCase):
    def setUp(self):
        import test_switch
        self.f = test_switch.SwitchTest('runTest')
        self.f.setUp()
        self.addCleanup(self.f.doCleanups)
        topology = self.f.root / 'video-topology.json'
        topology.write_text('{"instances":[{"name":"ltx","model":"ltx","gpus":[0]}]}')
        for runner in (self.f.old, self.f.new):
            data = copy.deepcopy(runner.d.data)
            version = data['version']
            version.update(engine='ltx', binding={'model': 'ltx'})
            version['environment']['VG_INSTANCES'] = '/weights/video-topology.json'
            version['assets'].append({'name': 'topology', 'role': 'config', 'source': 'git://example/topology.json', 'sha256': e.digest(topology)})
            data['target']['asset_paths']['topology'] = str(topology)
            item = data['target']['instance']
            item['protocol'] = 'video_generation'
            for field in ('health_url', 'local_health_url'):
                item[field] = item['endpoint'] + '/gpu0/v1/videos/models/' + data['model_id'] + '/health'
            item['artifact_digest'] = 'sha256-' + e.fingerprint(version)
            case = data['acceptance'][0]
            case.update(expected_frames=25, expected_fps=24)
            case['expected_generation']['model_version.lora.sha256'] = next(x['sha256'] for x in version['assets'] if x['role'] == 'lora')
            path = self.f.root / (runner.d.id + '.json')
            path.write_text(json.dumps(data))
            runner.d = e.Deployment(path)
        self.f.switch = e.Switch(self.f.old, self.f.new)
        row = self.f.old.d.item.registration()
        self.f.gateway.rows = [{'id': row.pop('instance_id'), **row, 'draining': False, 'eligible': True, 'healthy': True, 'reserved_requests': 0, 'reserved_units': 0}]
        config_for = self.f.config_for
        def config(d):
            value = config_for(d)
            env = value['services']['worker']['environment']
            for name in ('VMIND_IMAGE_MODELS', 'VMIND_GENERATION_STATE', 'VMIND_WARMUP_READY_FILE'):
                env.pop(name)
            env.update(VG_DATA_DIR='/state/data', VG_JOB_STATE_DIR='/state/jobs', VG_INSTANCES='/weights/video-topology.json',
                       VMIND_VIDEO_MODELS=json.dumps({k: {'model': 'ltx'} for k in (d.item.model_key, d.validation_model_id)}))
            return value
        self.f.config_for = config
        p = patch.object(e, 'read_json', return_value={'instances': [{'name': 'ltx', 'model': 'ltx', 'gpus': [0]}], 'queued_total': 0, 'readiness': {'admitted': 0, 'workers': [{'name': 'ltx', 'busy': False}]}})
        p.start(); self.addCleanup(p.stop)

    def test_stable_video_model_id_switches_real_lora(self):
        self.f.test_same_gpu_switch_preserves_model_id_and_drains_before_restart()

    def test_failed_video_candidate_restores_predecessor(self):
        self.f.test_failed_validation_restores_old_version_and_id()

    def test_unknown_video_execution_never_restarts_old_owner(self):
        self.f.test_unknown_candidate_execution_never_restarts_old_gpu_owner()

    def test_lora_switch_still_rejects_changed_base_weights(self):
        self.f.new.d.version['assets'][0]['sha256'] = 'f' * 64
        with self.assertRaisesRegex(ValueError, 'preserving base weights'):
            self.f.switch.plan()


if __name__ == '__main__':
    unittest.main()
