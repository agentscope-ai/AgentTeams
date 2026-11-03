# CredAgent Credential Protection Configuration

## Overview

CredAgent protects credentials inside containers at the application layer through the CoPaw security framework. It provides three layers of protection:

1. **File protection**: access is automatically denied when an Agent uses tools such as `read_file`, `write_file`, or `execute_shell_command` on a protected file.
2. **Output sanitization**: AK, SK, token, and other sensitive values in tool output are replaced by regular-expression rules before reaching the Agent.
3. **Prompt hardening**: the Agent system prompt contains non-overridable prohibitions against credential access.

Use `config/credagent.json` to configure file protection and output-sanitization rules together. The file is distributed to Workers through MinIO or OSS.

## File location

MinIO: `agents/{worker_name}/config/credagent.json`

MinIO synchronization automatically distributes it to `{install_dir}/{worker_name}/config/credagent.json` in the Worker environment.

## Format

```json
{
  "credentials": [
    {
      "path": "~/.aliyun/config.json",
      "programPermit": ["/usr/local/bin/aliyun"]
    }
  ],
  "output_sanitize": [
    {
      "type": "prefix",
      "prefix": "LTAI",
      "min_length": 16
    }
  ]
}
```

The configuration has two sections:

- `credentials` declares credential files that require file protection.
- `output_sanitize` optionally declares tool-output sanitization rules.

## `credentials` fields

| Field | Type | Required | Default | Description |
|------|------|------|--------|------|
| `path` | string | Yes | — | File path to protect; supports `~` expansion |
| `programPermit` | string \| string[] | Yes | `[]` | Absolute paths of programs permitted to read the file; retained in application-layer mode for a future FUSE implementation |
| `writable` | bool | No | `false` | Whether authorized programs may write the file; retained in application-layer mode for a future FUSE implementation |

## Configuration example

```json
{
  "credentials": [
    {
      "path": "~/.aliyun/config.json",
      "programPermit": ["/usr/local/bin/aliyun"]
    },
    {
      "path": "~/.ssh/id_rsa",
      "programPermit": ["/usr/bin/ssh", "/usr/bin/git"]
    },
    {
      "path": "~/.docker/config.json",
      "programPermit": ["/usr/bin/docker"],
      "writable": true
    }
  ],
  "output_sanitize": [
    {
      "type": "prefix",
      "prefix": "LTAI",
      "min_length": 16
    },
    {
      "type": "keyword",
      "keywords": ["access_key_secret", "accessKeySecret", "AccessKeySecret"]
    }
  ]
}
```

## Output sanitization (`output_sanitize`)

Even when file protection prevents an Agent from reading a credential file directly, the Agent may still obtain plaintext credentials through a CLI command such as `aliyun configure get`. Configure `output_sanitize` alongside `credentials` in `credagent.json` to replace matching sensitive values before tool output reaches the Agent.

Built-in rules are always active and cover:

- AccessKey ID prefixes: Alibaba Cloud `LTAI`, AWS `AKIA`, and Tencent Cloud `AKID`.
- Values following secret-related keywords such as `access_key_secret` and `SecretAccessKey`.
- Long strings following token-related keywords such as `security_token` and `session_token`.

Add custom rules through `output_sanitize`. Three rule types are supported:

| Type | Fields | Description |
|------|------|------|
| `prefix` | `prefix`, `min_length` (default: 16) | Matches a Key ID beginning with the specified prefix, preserves the prefix, and replaces the remaining value with `****` |
| `keyword` | `keywords` | Matches values following a keyword, with `=`, `:`, or `"` as the separator, and replaces the value with `********` |
| `regex` | `pattern`, `replacement` | Raw regular expression with backreferences such as `\1` |

### Custom-rule example

```json
{
  "output_sanitize": [
    {
      "type": "prefix",
      "prefix": "MYKEY_",
      "min_length": 20
    },
    {
      "type": "keyword",
      "keywords": ["my_api_secret", "custom_token"]
    },
    {
      "type": "regex",
      "pattern": "\\b(SK-)[A-Za-z0-9]{32,}",
      "replacement": "\\1****"
    }
  ]
}
```

## How it works

### File protection

1. When a Worker starts, `bridge_standard_to_runtime()` reads `config/credagent.json` and injects every `credentials[].path` into CoPaw `config.json` under `security.file_guard.sensitive_files`.
2. CoPaw's `FilePathToolGuardian` checks whether a path matches `sensitive_files` before every tool call.
3. The AgentTeams credential-guard hook forces a `SENSITIVE_FILE_ACCESS` finding to `auto_denied`, so the Agent cannot bypass the decision.

### Output sanitization

1. When a Worker starts, custom `output_sanitize` rules are loaded from `credagent.json` and merged with the built-in rules.
2. AgentTeams registers the sanitization middleware through the CoPaw Toolkit middleware mechanism.
3. After each tool finishes, the middleware applies regular-expression replacements to TextBlock content before the output reaches Agent memory or is displayed to the user.

### Hot reload

Changes to `credagent.json` in MinIO are automatically pulled by `sync_loop` and hot-reloaded at runtime. File-protection and output-sanitization rules are updated together and normally take effect in about 60 seconds.

## Protection scope

### File Guard

The following tools are prevented from accessing protected paths:

- `read_file` / `write_file`: direct file operations.
- `execute_shell_command`: extracts path arguments from shell commands such as `cat`, `head`, and `vim`.

### Output Sanitizer

Regular-expression sanitization is applied to all tool output to prevent CLI commands from exposing plaintext credentials:

- `execute_shell_command`: intercepts output from commands such as `aliyun configure get` and `env | grep KEY`.
- `read_file`: replaces sensitive values in output even if file content is read after bypassing File Guard.

### Prompt hardening (`AGENTS.md`)

The Agent system prompt declares a non-overridable prohibition against credential access, preventing social-engineering phrases such as “security test” or “debugging” from inducing the Agent to retrieve credentials.
