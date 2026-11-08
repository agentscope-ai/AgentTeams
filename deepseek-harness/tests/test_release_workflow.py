from pathlib import Path
import unittest


WORKFLOW = Path(__file__).resolve().parents[2] / ".github" / "workflows" / "build-deepseek-harness.yml"


class DeepSeekHarnessReleaseWorkflowTest(unittest.TestCase):
    def test_untrusted_version_and_ref_are_passed_through_environment(self) -> None:
        text = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn("INPUT_VERSION: ${{ inputs.version }}", text)
        self.assertIn("REF_NAME: ${{ github.ref_name }}", text)
        self.assertIn('VERSION="${INPUT_VERSION}"', text)
        self.assertIn('VERSION="${REF_NAME}"', text)
        self.assertIn('if [[ ! "${VERSION}" =~ ^v[0-9]+\\.[0-9]+\\.[0-9]+', text)
        self.assertNotIn('VERSION="${{ inputs.version }}"', text)
        self.assertNotIn('VERSION="${{ github.ref_name }}"', text)
        self.assertNotIn('echo "${VERSION}" | grep', text)

    def test_validated_version_reaches_make_through_environment(self) -> None:
        text = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn("RUNTIME_VERSION: ${{ steps.meta.outputs.version }}", text)
        self.assertIn('DEEPSEEK_HARNESS_WORKER_VERSION="${RUNTIME_VERSION}"', text)
        self.assertNotIn("DEEPSEEK_HARNESS_WORKER_VERSION=${{ steps.meta.outputs.version }}", text)

    def test_published_manifest_must_contain_both_supported_platforms(self) -> None:
        text = WORKFLOW.read_text(encoding="utf-8")

        self.assertIn("docker buildx imagetools inspect --raw", text)
        self.assertIn("for PLATFORM in linux/amd64 linux/arm64", text)
        self.assertIn("jq -e --arg platform", text)


if __name__ == "__main__":
    unittest.main()
