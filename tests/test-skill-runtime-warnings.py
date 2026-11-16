import importlib.util
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("skill_warnings", ROOT / "shared/lib/agentteams_skill_warnings.py")
skills = importlib.util.module_from_spec(spec)
spec.loader.exec_module(skills)


def write_skill(root, name, content):
    target = root / name / "SKILL.md"
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content)


def test_invalid_skill_is_excluded_without_losing_healthy_content(tmp_path):
    root = tmp_path / "skills"
    write_skill(root, "healthy", "---\nname: healthy\ndescription: Works\n---\nYou can use this skill.\n")
    write_skill(root, "broken", "---\nname: [broken\n---\n")
    with patch.object(skills, "_request", return_value=None) as report:
        warnings = skills.inspect_skills(root, ["healthy", "broken", "missing"])
    assert len(warnings) == 2
    assert (root / "healthy/SKILL.md").is_file()
    assert not (root / "broken").exists()
    assert (tmp_path / ".agentteams-invalid-skills/broken/SKILL.md").is_file()
    assert report.call_args.args == ("POST", "/skill-warnings", {"skillWarnings": warnings})
    write_skill(root, "broken", "# Repaired\n")
    write_skill(root, "missing", "# Available now\n")
    with patch.object(skills, "_request", return_value=None) as report:
        assert skills.inspect_skills(root, ["healthy", "broken", "missing"]) == []
    assert report.call_args.args[2] == {"skillWarnings": []}


def test_invalid_names_and_external_links_never_modify_external_files(tmp_path):
    root = tmp_path / "skills"
    outside = tmp_path / "outside"
    write_skill(outside, "keep", "# External\n")
    root.mkdir()
    (root / "external").symlink_to(outside / "keep", target_is_directory=True)
    with patch.object(skills, "_request", return_value=None):
        warnings = skills.inspect_skills(root, ["..", "../outside", "external"])
    assert len(warnings) == 3
    assert (outside / "keep/SKILL.md").read_text() == "# External\n"


def test_warning_report_failure_does_not_fail_validation(tmp_path):
    write_skill(tmp_path / "skills", "healthy", "# Ready\n")
    with patch.object(skills, "_request", side_effect=OSError("offline")):
        assert skills.inspect_skills(tmp_path / "skills", ["healthy"]) == []


def test_skill_file_symlink_cannot_read_external_prompt_content(tmp_path):
    root = tmp_path / "skills"
    write_skill(root, "linked", "# Placeholder\n")
    outside = tmp_path / "private.txt"
    outside.write_text("private content")
    prompt = root / "linked/SKILL.md"
    prompt.unlink()
    prompt.symlink_to(outside)
    with patch.object(skills, "_request", return_value=None):
        assert len(skills.inspect_skills(root, ["linked"])) == 1
    assert outside.read_text() == "private content"
    assert not (root / "linked").exists()
