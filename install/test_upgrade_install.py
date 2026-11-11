"""Data-preserving upgrade contracts; no real engine or user configuration."""
import json
from pathlib import Path
import tempfile
import unittest

from test_one_click_install import SOURCE, functions, run


class UpgradeInstallTest(unittest.TestCase):
    def test_image_failure_leaves_saved_configuration_untouched(self):
        self.image_phase(fail=True)

    def test_fresh_image_failure_leaves_no_orphan_data_volume(self):
        self.image_phase(fail=True, fresh=True)

    def test_success_backs_up_and_keeps_external_storage_and_extra_settings(self):
        self.image_phase(fail=False)

    def image_phase(self, fail, fresh=False):
        start = SOURCE.index('    # Prepare service images before saving settings')
        end = SOURCE.index('    # --- Pre-upgrade: extract Matrix passwords', start)
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'manager.env'
            original = 'AGENTTEAMS_ADMIN_PASSWORD=secret#with=equals\nAGENTTEAMS_FS_ENDPOINT=https://storage.example\nAGENTTEAMS_STORAGE_PROVIDER=s3\nAGENTTEAMS_CUSTOM_OPTION=with space#and=equals\n'
            if not fresh:
                path.write_text(original)
            script = functions('load_upgrade_config') + '\nset -e\n'
            script += 'AGENTTEAMS_ENV_FILE=' + json.dumps(str(path)) + '\n'
            script += '''
_INSTALL_ENV_KEYS=' '
load_upgrade_config "$AGENTTEAMS_ENV_FILE"
AGENTTEAMS_UPGRADE=1
AGENTTEAMS_INSTALL_EMBEDDED_IMAGE=agentteams/embedded:test
AGENTTEAMS_DASHBOARD=1
AGENTTEAMS_DASHBOARD_IMAGE=agentteams/dashboard:test
DOCKER_CMD=docker
log() { :; }
msg() { :; }
die() { exit 7; }
sleep() { :; }
resolve_embedded_image() { AGENTTEAMS_USE_EMBEDDED=1; EMBEDDED_IMAGE=agentteams/embedded:test; }
manager_image_for_runtime() { echo agentteams/manager:test; }
prepare_data_volume() { touch "${AGENTTEAMS_ENV_FILE}.storage-prepared"; }
'''
            if fresh:
                script += 'AGENTTEAMS_UPGRADE=0\n'
            script += 'docker() { return ' + ('1' if fail else '0') + '; }\n'
            # Execute the actual image preparation and configuration-writing phase.
            script += 'image_phase() {\n' + SOURCE[start:end] + '\n}\nimage_phase\n'
            result = run(script)
            backups = list(Path(directory).glob('manager.env.before-upgrade-*'))
            if fail:
                self.assertEqual(result.returncode, 7, result.stderr)
                if fresh:
                    self.assertFalse(path.exists())
                else:
                    self.assertEqual(path.read_text(), original)
                self.assertEqual(backups, [])
                self.assertFalse(Path(str(path) + ".storage-prepared").exists())
            else:
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(len(backups), 1)
                self.assertTrue(Path(str(path) + ".storage-prepared").exists())
                self.assertEqual(backups[0].read_text(), original)
                for line in original.splitlines():
                    self.assertIn(line, path.read_text().splitlines())

    def test_normal_upgrade_does_not_remove_workers(self):
        start = SOURCE.index('    # Only legacy architecture migration needs Worker containers')
        end = SOURCE.index('\n    fi', start) + len('\n    fi')
        for legacy in (0, 1):
            script = '''
set -e
DOCKER_CMD=docker
UPGRADE_EXISTING_WORKERS='agentteams-worker-first agentteams-worker-second'
log() { :; }
msg() { :; }
docker() { echo "$*"; }
'''
            script += '_is_old_arch=' + str(legacy) + '\n' + SOURCE[start:end]
            result = run(script)
            self.assertEqual(result.returncode, 0, result.stderr)
            if legacy:
                self.assertEqual(result.stdout.splitlines(), ['stop agentteams-worker-first', 'rm agentteams-worker-first', 'stop agentteams-worker-second', 'rm agentteams-worker-second'])
            else:
                self.assertEqual(result.stdout, '')

    def existing(self, answer="", unattended=False, mismatch=False, incomplete=False):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "manager.env"
            config.write_text("AGENTTEAMS_ADMIN_USER=operator\nAGENTTEAMS_ADMIN_PASSWORD=secret#with=equals\nAGENTTEAMS_MANAGER_PASSWORD=manager-secret\nAGENTTEAMS_REGISTRATION_TOKEN=registration-secret\nAGENTTEAMS_MINIO_USER=storage-admin\nAGENTTEAMS_MINIO_PASSWORD=storage-secret\nAGENTTEAMS_MANAGER_GATEWAY_KEY=gateway-secret\nAGENTTEAMS_MATRIX_APPSERVICE_ENABLED=false\nAGENTTEAMS_DATA_DIR=original-data\nAGENTTEAMS_WORKSPACE_DIR=/original/work space\nAGENTTEAMS_DASHBOARD=0\nAGENTTEAMS_DOCKER_PROXY=0\n")
            if incomplete:
                config.write_text(config.read_text().replace('AGENTTEAMS_MANAGER_PASSWORD=manager-secret\n', ''))
            script = functions("step_existing", "load_upgrade_config", "prepare_upgrade_dashboard")
            script += """
set -e
log() { :; }
msg() { printf '%s' "$1"; }
die() { echo "$1" >&2; exit 7; }
inherit_legacy_copaw_worker_image() { :; }
_ver_lt() { return 0; }
docker() { case "$1 $2" in 'ps --format'|'ps -a') echo agentteams-manager ;; 'inspect agentteams-controller'|'inspect agentteams-manager') echo old-image:v1.2.5 ;; 'volume inspect') return 0 ;; *) echo "UNEXPECTED MUTATION $*" >&2; exit 99 ;; esac; }
DOCKER_CMD=docker
AGENTTEAMS_QUICKSTART=1
AGENTTEAMS_VERSION=v1.2.6
AGENTTEAMS_DOCKER_PROXY=1
AGENTTEAMS_REGISTRY=registry.example
_INSTALL_ENV_KEYS=' '
prompts=0
"""
            script += "AGENTTEAMS_ENV_FILE=" + json.dumps(str(config)) + "\n"
            script += "AGENTTEAMS_NON_INTERACTIVE=" + ("1" if unattended else "0") + "\n"
            script += "detect_installed_data_volume() { echo " + ("wrong-data" if mismatch else "original-data") + "; }\n"
            script += "read() { case \"$*\" in *'-p '*) prompts=$((prompts+1)); printf -v \"${@: -1}\" '%s' " + json.dumps(answer) + ";; *) builtin read \"$@\";; esac; }\n"
            script += """
step_existing
test "$AGENTTEAMS_ADMIN_USER" = operator
test "$AGENTTEAMS_ADMIN_PASSWORD" = 'secret#with=equals'
test "$AGENTTEAMS_DATA_DIR" = original-data
test "$AGENTTEAMS_WORKSPACE_DIR" = '/original/work space'
test "$AGENTTEAMS_DOCKER_PROXY" = 0
test "$AGENTTEAMS_DASHBOARD" = 0
test "$AGENTTEAMS_UPGRADE_KEEP_ALL" = 1
echo "PROMPTS=$prompts"
"""
            return run(script)

    def test_upgrade_asks_once_and_keeps_configuration_verbatim(self):
        result = self.existing()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PROMPTS=1", result.stdout)

    def test_unattended_upgrade_does_not_prompt_or_delete_data(self):
        result = self.existing(unattended=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("PROMPTS=0", result.stdout)

    def test_cancel_does_not_modify_containers(self):
        result = self.existing(answer="n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("PROMPTS=", result.stdout)
        self.assertNotIn("MUTATION", result.stderr)

    def test_conflicting_data_volume_fails_before_confirmation(self):
        result = self.existing(mismatch=True)
        self.assertEqual(result.returncode, 7)
        self.assertNotIn("MUTATION", result.stderr)

    def test_incomplete_credentials_stop_instead_of_generating_new_identity(self):
        result = self.existing(incomplete=True)
        self.assertEqual(result.returncode, 7)
        self.assertNotIn('PROMPTS=', result.stdout)
        self.assertNotIn('MUTATION', result.stderr)

    def test_unrecoverable_orphan_data_is_never_cleaned(self):
        result = run(functions("recover_existing_install", "detect_installed_workspace") + """
set -e
log() { :; }
msg() { printf '%s' "$1"; }
die() { echo "$1" >&2; exit 7; }
DOCKER_CMD=docker
AGENTTEAMS_NON_INTERACTIVE=1
docker() {
    case "$1 $2" in
        'ps -a') return 0 ;;
        'volume inspect') return 0 ;;
        *) echo "UNEXPECTED MUTATION $*" >&2; exit 99 ;;
    esac
}
recover_existing_install
""")
        self.assertEqual(result.returncode, 7)
        self.assertNotIn("MUTATION", result.stderr)

    def test_recovery_uses_original_mounts_and_credentials_without_writes(self):
        result = run(functions("recover_existing_install", "detect_installed_workspace") + """
set -e
log() { :; }
msg() { :; }
die() { echo failed >&2; exit 7; }
DOCKER_CMD=docker
detect_installed_data_volume() { echo custom-data; }
docker() {
    case "$1" in
        ps) echo agentteams-controller ;;
        inspect)
            case "$*" in
                *Config.Env*) printf '%s\\n' AGENTTEAMS_ADMIN_USER=operator 'AGENTTEAMS_ADMIN_PASSWORD=secret#saved' AGENTTEAMS_MANAGER_PASSWORD=manager-secret AGENTTEAMS_REGISTRATION_TOKEN=registration-secret AGENTTEAMS_MINIO_USER=operator AGENTTEAMS_MINIO_PASSWORD=storage-secret AGENTTEAMS_MANAGER_GATEWAY_KEY=gateway-secret AGENTTEAMS_MATRIX_APPSERVICE_ENABLED=false ;;
                *Mounts*) echo '/original/work space' ;;
                *) exit 99 ;;
            esac ;;
        *) echo "UNEXPECTED MUTATION $*" >&2; exit 99 ;;
    esac
}
recover_existing_install
test -n "$AGENTTEAMS_RECOVERED_CONFIG"
printf '%s' "$AGENTTEAMS_RECOVERED_CONFIG" | grep -q 'AGENTTEAMS_DATA_DIR=custom-data'
printf '%s' "$AGENTTEAMS_RECOVERED_CONFIG" | grep -q 'AGENTTEAMS_WORKSPACE_DIR=/original/work space'
""")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_harness_names_preserve_old_releases_and_explicit_images(self):
        result = run(functions("_normalize_version", "_ver_lt", "_supports_deepseek_harness", "resolve_image_tags") + """
set -e
AGENTTEAMS_REGISTRY=registry.example
AGENTTEAMS_CODEX_WORKER_VERSION=v0.1.0
AGENTTEAMS_DEEPSEEK_HARNESS_MIN_VERSION=v1.2.4
AGENTTEAMS_VERSION=v1.2.6
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.1
resolve_image_tags
test "$WORKER_IMAGE" = registry.example/agentteams/agentteams-worker:v1.2.6
test "$QWENPAW_WORKER_IMAGE" = registry.example/agentteams/agentteams-qwenpaw-worker:v1.2.6
test "$DEEPSEEK_HARNESS_WORKER_IMAGE" = registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.2
resolve_image_tags
test "$DEEPSEEK_HARNESS_WORKER_IMAGE" = registry.example/agentteams/agentteams-deepseek-harness-worker:v0.1.1
_INSTALL_ENV_KEYS=' AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION '
resolve_image_tags
test "$DEEPSEEK_HARNESS_WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-deepseek:v0.1.2
_INSTALL_ENV_KEYS=' '
AGENTTEAMS_VERSION=v1.2.7
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.2
resolve_image_tags
test "$WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-openclaw:v1.2.7
test "$QWENPAW_WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-qwenpaw:v1.2.7
test "$HERMES_WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-hermes:v1.2.7
test "$OPENHUMAN_WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-openhuman:v1.2.7
test "$DEEPSEEK_HARNESS_WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-deepseek:v0.1.2
AGENTTEAMS_VERSION=local-pre127
resolve_image_tags
test "$WORKER_IMAGE" = registry.example/agentteams/agentteams-harness-openclaw:local-pre127
AGENTTEAMS_VERSION=v1.2.6
AGENTTEAMS_INSTALL_WORKER_IMAGE=private.example/agentteams-harness-openclaw:custom
AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE=private.example/agentteams-harness-qwenpaw:custom
AGENTTEAMS_INSTALL_HERMES_WORKER_IMAGE=private.example/agentteams-harness-hermes:custom
AGENTTEAMS_INSTALL_OPENHUMAN_WORKER_IMAGE=private.example/agentteams-harness-openhuman:custom
AGENTTEAMS_INSTALL_DEEPSEEK_HARNESS_WORKER_IMAGE=private.example/agentteams-harness-deepseek:custom
AGENTTEAMS_DEEPSEEK_HARNESS_WORKER_VERSION=v0.1.1
resolve_image_tags
test "$WORKER_IMAGE" = "$AGENTTEAMS_INSTALL_WORKER_IMAGE"
test "$QWENPAW_WORKER_IMAGE" = "$AGENTTEAMS_INSTALL_QWENPAW_WORKER_IMAGE"
test "$HERMES_WORKER_IMAGE" = "$AGENTTEAMS_INSTALL_HERMES_WORKER_IMAGE"
test "$OPENHUMAN_WORKER_IMAGE" = "$AGENTTEAMS_INSTALL_OPENHUMAN_WORKER_IMAGE"
test "$DEEPSEEK_HARNESS_WORKER_IMAGE" = "$AGENTTEAMS_INSTALL_DEEPSEEK_HARNESS_WORKER_IMAGE"
""")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_dashboard_defaults_advance_but_custom_choices_are_kept(self):
        result = run(functions("prepare_upgrade_dashboard") + """
set -e
_INSTALL_ENV_KEYS=' '
AGENTTEAMS_REGISTRY=registry.example
AGENTTEAMS_DASHBOARD_VERSION=v1.3.1
AGENTTEAMS_DASHBOARD_IMAGE=registry.example/agentteams/agentteams-dashboard:v1.3.1
prepare_upgrade_dashboard
test "$AGENTTEAMS_DASHBOARD_VERSION" = v1.4.0
test "$AGENTTEAMS_DASHBOARD_IMAGE" = registry.example/agentteams/agentteams-dashboard:v1.4.0
AGENTTEAMS_DASHBOARD_IMAGE=private.example/dashboard:canary
prepare_upgrade_dashboard
test "$AGENTTEAMS_DASHBOARD_IMAGE" = private.example/dashboard:canary
_INSTALL_ENV_KEYS=' AGENTTEAMS_DASHBOARD_VERSION '
AGENTTEAMS_DASHBOARD_VERSION=v1.3.1
AGENTTEAMS_DASHBOARD_IMAGE=registry.example/agentteams/agentteams-dashboard:v1.3.1
prepare_upgrade_dashboard
test "$AGENTTEAMS_DASHBOARD_VERSION" = v1.3.1
""")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
