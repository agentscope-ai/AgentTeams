package skillscan

// probeScript is the Python probe executed inside the qwenpaw container.
//
// It runs as a direct argv (no shell): the controller passes the skill dir,
// the skill name, and the verdict output path as argv[1..3]. The probe
// writes exactly one JSON line to the output path:
//
//	{"status":"pass","findings":[...]}      no findings
//	{"status":"warn","findings":[...]}      MEDIUM/LOW/INFO findings
//	{"status":"block","findings":[...]}     CRITICAL/HIGH findings
//	{"status":"unavailable","detail":".."}  scanner not importable or the
//	                                       skill dir is missing (an
//	                                       infrastructure failure — NOT a
//	                                       content verdict)
//
// Contract (the gate is the gate — runtime off/warn config is bypassed):
//   - prefers SkillScanner.scan_skill (no runtime-config gating);
//   - on older runtimes without that class, falls back to
//     scan_skill_directory(block=True) — a raised SkillScanError is a
//     block finding (block mode only raises on CRITICAL/HIGH), and a
//     None return (scanner disabled / skill whitelisted by runtime
//     config) is a block finding, not a pass (fail closed).
//
// Findings carry metadata only — never file contents (secret hygiene).
// If the probe dies before writing the verdict (a hard crash), the
// controller sees a missing verdict file and treats the scan as
// unavailable — never as a pass.
const probeScript = `
import json, sys, traceback
from pathlib import Path
src = Path(sys.argv[1]); name = sys.argv[2]; out = Path(sys.argv[3])

def emit(status, findings=None, detail=""):
    payload = {"status": status, "findings": findings or []}
    if detail:
        payload["detail"] = detail
    out.write_text(json.dumps(payload))

def report(r):
    d = r.to_dict()
    if d["max_severity"] in ("CRITICAL", "HIGH"):
        status = "block"
    elif d["findings_count"] > 0:
        status = "warn"
    else:
        status = "pass"
    emit(status, [{
        "rule_id": f.get("rule_id", ""),
        "category": f.get("category", ""),
        "severity": f.get("severity", ""),
        "file": f.get("file_path", ""),
        "line": f.get("line_number", 0),
        "title": f.get("title", ""),
    } for f in d["findings"]])

if not src.is_dir():
    emit("unavailable", detail="skill directory missing: %s" % src)
    sys.exit(0)

try:
    from qwenpaw.security.skill_scanner.scanner import SkillScanner
except ImportError:
    try:
        from qwenpaw.security.skill_scanner import scan_skill_directory
    except ImportError:
        emit("unavailable", detail="qwenpaw skill_scanner not importable")
        sys.exit(0)
    # Older runtimes expose only the mode-gated wrapper: force block mode.
    try:
        r = scan_skill_directory(src, skill_name=name, block=True)
    except Exception as e:
        # block=True raises only on CRITICAL/HIGH findings.
        emit("block", [{
            "rule_id": "scan.raised", "severity": "CRITICAL",
            "file": "", "line": 0, "category": "",
            "title": "scanner raised: %s" % e,
        }])
    else:
        if r is None:
            emit("block", [{
                "rule_id": "scan.disabled", "severity": "CRITICAL",
                "file": "", "line": 0, "category": "",
                "title": "scanner disabled or skill whitelisted by runtime config",
            }])
        else:
            report(r)
    sys.exit(0)

try:
    report(SkillScanner().scan_skill(src, skill_name=name))
except Exception:
    # A scanner-internal failure is an infrastructure problem (a
    # half-working scanner must not be read as "clean").
    emit("unavailable", detail="scanner failed: %s" % traceback.format_exc(limit=3))
`
