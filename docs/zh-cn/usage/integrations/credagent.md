# CredAgent 凭证保护配置

## 概述

CredAgent 通过 CoPaw 安全框架在应用层保护容器内的凭证，提供三层防护：

1. **文件防护**：Agent 通过 `read_file`、`write_file`、`execute_shell_command` 等工具访问受保护文件时，自动拒绝（auto-denied）
2. **输出脱敏**：工具输出中的 AK/SK/Token 等敏感内容在到达 Agent 之前被正则替换
3. **Prompt 加固**：Agent 系统提示词中内置不可覆盖的凭证访问禁令

用户通过 `config/credagent.json` 统一配置文件防护和输出脱敏规则，通过 MinIO/OSS 下发到 Worker。

## 文件位置

MinIO: `agents/{worker_name}/config/credagent.json`

通过 MinIO sync 自动下发到 Worker 本地 `{install_dir}/{worker_name}/config/credagent.json`。

## 格式

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

配置文件包含两部分：
- `credentials` — 声明需要保护的凭证文件（文件防护）
- `output_sanitize` — 声明工具输出脱敏规则（输出脱敏），可选

## credentials 字段说明

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|------|------|------|--------|------|
| `path` | string | 是 | — | 要保护的文件路径，支持 `~` 展开 |
| `programPermit` | string \| string[] | 是 | `[]` | 允许读取该文件的程序绝对路径（应用层模式下保留，供未来 FUSE 方案使用） |
| `writable` | bool | 否 | `false` | 授权程序是否可写（应用层模式下保留，供未来 FUSE 方案使用） |

## 配置示例

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

## 输出脱敏（output_sanitize）

即使文件防护阻止了 Agent 直接读取凭证文件，Agent 仍可能通过 CLI 命令（如 `aliyun configure get`）获取明文凭证。`output_sanitize` 与 `credentials` 在同一个 `credagent.json` 中配置，在工具输出到达 Agent 之前对匹配的敏感内容进行正则替换。

内置规则始终生效，覆盖以下场景：
- AccessKey ID 前缀：阿里云 `LTAI`、AWS `AKIA`、腾讯云 `AKID`
- Secret 关键字后的值：`access_key_secret`、`SecretAccessKey` 等
- Token 关键字后的长字符串：`security_token`、`session_token` 等

用户可通过 `output_sanitize` 字段添加自定义规则，支持三种类型：

| 类型 | 字段 | 说明 |
|------|------|------|
| `prefix` | `prefix`, `min_length`(默认16) | 匹配以指定前缀开头的 Key ID，保留前缀，后面替换为 `****` |
| `keyword` | `keywords` | 匹配关键字后面的值（支持 `=`、`:`、`"` 分隔），值替换为 `********` |
| `regex` | `pattern`, `replacement` | 原始正则表达式，支持反向引用 `\1` |

### 自定义规则示例

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

## 工作原理

### 文件防护

1. Worker 启动时，`bridge_standard_to_runtime()` 读取 `config/credagent.json`，将所有 `credentials[].path` 注入到 CoPaw 的 `config.json` → `security.file_guard.sensitive_files`
2. CoPaw 的 `FilePathToolGuardian` 在每次工具调用前检查路径是否匹配 `sensitive_files`
3. AgentTeams 的 credential guard hook 将 `SENSITIVE_FILE_ACCESS` finding 强制设为 `auto_denied`，Agent 无法绕过

### 输出脱敏

1. Worker 启动时，从 `credagent.json` 的 `output_sanitize` 加载用户自定义规则，与内置规则合并
2. AgentTeams 通过 CoPaw Toolkit middleware 机制注册脱敏中间件
3. 每次工具执行完成后，中间件在输出到达 Agent 内存和用户展示之前，对 TextBlock 内容执行正则替换

### 热重载

运行时修改 MinIO 中的 `credagent.json` 会通过 sync_loop 自动拉取并热重载（约 60 秒生效），文件防护和输出脱敏规则同时更新。

## 保护范围

### 文件防护（File Guard）

拦截以下工具对受保护路径的访问：
- `read_file` / `write_file` — 直接文件操作
- `execute_shell_command` — 从 shell 命令中提取路径参数（如 `cat`、`head`、`vim` 等）

### 输出脱敏（Output Sanitizer）

对所有工具的输出进行正则脱敏，防止 CLI 命令泄露凭证明文：
- `execute_shell_command` — 拦截 `aliyun configure get`、`env | grep KEY` 等命令的输出
- `read_file` — 即使绕过 File Guard 读取到凭证内容，输出中的敏感值也会被替换

### Prompt 加固（AGENTS.md）

Agent 系统提示词中声明了不可覆盖的凭证访问禁令，防止通过社会工程（"安全测试"、"调试"等话术）诱导 Agent 尝试获取凭证。
