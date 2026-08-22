#!/usr/bin/env powershell
# hiclaw-install.ps1 - One-click installation for HiClaw Manager and Worker on Windows
# Compatible with PowerShell 5.1+ and PowerShell 7.0+
#
# Usage:
#   .\hiclaw-install.ps1                  # Interactive installation (choose Quick Start or Manual)
#   .\hiclaw-install.ps1 manager          # Same as above (explicit)
#   .\hiclaw-install.ps1 worker --name <name> ...  # Worker installation
#
# Onboarding Modes:
#   Quick Start  - Fast installation with all default values (recommended)
#   Manual       - Customize each option step by step
#
# Environment variables (for automation):
#   HICLAW_NON_INTERACTIVE    Skip all prompts, use defaults  (default: 0)
#   HICLAW_LLM_PROVIDER       LLM provider       (default: qwen)
#   HICLAW_DEFAULT_MODEL      Default model      (default: qwen3.5-plus)
#   HICLAW_LLM_API_KEY        LLM API key        (required)
#   HICLAW_ADMIN_USER         Admin username     (default: admin)
#   HICLAW_ADMIN_PASSWORD     Admin password     (auto-generated if not set, min 8 chars)
#   HICLAW_MATRIX_DOMAIN      Matrix domain      (default: matrix-local.hiclaw.io:18080)
#   HICLAW_MOUNT_SOCKET       Mount container runtime socket (default: 1)
#   HICLAW_MATRIX_E2EE        Matrix E2EE        (default: 0, disabled)
#   HICLAW_DATA_DIR           Docker volume name for persistent data (default: hiclaw-data)
#   HICLAW_WORKSPACE_DIR      Host directory for manager workspace (default: ~/hiclaw-manager)
#   HICLAW_VERSION            Image tag          (default: latest)
#   HICLAW_REGISTRY           Image registry     (default: auto-detected by timezone)
#   HICLAW_INSTALL_MANAGER_IMAGE       Override manager image (e.g., local build)
#   HICLAW_INSTALL_WORKER_IMAGE        Override worker image  (e.g., local build)
#   HICLAW_INSTALL_COPAW_WORKER_IMAGE  Override copaw worker image (e.g., local build)
#   HICLAW_PORT_GATEWAY       Host port for Higress gateway (default: 18080)
#   HICLAW_PORT_CONSOLE       Host port for Higress console (default: 18001)
#   HICLAW_PORT_ELEMENT_WEB   Host port for Element Web direct access (default: 18088)

#Requires -Version 5.1

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet("manager", "worker", "uninstall")]
    [string]$Command = "manager",

    # Worker options
    [string]$Name,
    [string]$Fs,
    [string]$FsKey,
    [string]$FsSecret,
    [switch]$Reset,
    [switch]$FindSkills,
    [string]$SkillsApiUrl,

    # General options
    [switch]$NonInteractive,
    [string]$EnvFile
)

# ============================================================
# Configuration
# ============================================================

$script:HICLAW_VERSION = if ($env:HICLAW_VERSION) { $env:HICLAW_VERSION } else { "latest" }
$script:HICLAW_NON_INTERACTIVE = if ($env:HICLAW_NON_INTERACTIVE -eq "1" -or $NonInteractive) { $true } else { $false }
$script:HICLAW_MOUNT_SOCKET = if ($env:HICLAW_MOUNT_SOCKET -eq "0") { $false } else { $true }
$script:HICLAW_ENV_FILE = if ($EnvFile) { $EnvFile } elseif ($env:HICLAW_ENV_FILE) { $env:HICLAW_ENV_FILE } else { "$env:USERPROFILE\hiclaw-manager.env" }
$script:StepResult = ""  # Used by state machine to signal "back" navigation
$script:config = @{}     # Shared config hashtable for step functions

# ANSI escape character for PowerShell 5.1+ compatibility
$script:ESC = [char]0x1B

# ============================================================
# Log all output to file
# ============================================================

$script:HICLAW_LOG_FILE = "$env:USERPROFILE\hiclaw-install.log"

# Start transcript for logging (PowerShell's built-in logging mechanism)
try {
    Start-Transcript -Path $script:HICLAW_LOG_FILE -Append -ErrorAction SilentlyContinue | Out-Null
} catch {
    # If transcript fails, continue without logging
}

Write-Host ""
Write-Host "========================================"
Write-Host "HiClaw Installation Log"
Write-Host "Started: $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
Write-Host "User: $env:USERNAME"
Write-Host "System: $(hostname)"
Write-Host "Log file: $($script:HICLAW_LOG_FILE)"
Write-Host "========================================"
Write-Host ""

# ============================================================
# Utility Functions
# ============================================================

function Write-Log {
    param([string]$Message)
    Write-Host "$($script:ESC)[36m[HiClaw]$($script:ESC)[0m $Message"
}

function Write-Error {
    param([string]$Message)
    Write-Host "$($script:ESC)[31m[HiClaw ERROR]$($script:ESC)[0m $Message" -ForegroundColor Red
    throw $Message
}

function Write-Warning {
    param([string]$Message)
    Write-Host "$($script:ESC)[33m[HiClaw WARNING]$($script:ESC)[0m $Message"
}

# Pause before exit on error so user can read the message when running via double-click
function Exit-Script {
    param([int]$ExitCode = 0)
    if ($ExitCode -ne 0 -and -not $script:HICLAW_NON_INTERACTIVE) {
        Write-Host ""
        Write-Host "Press Enter to exit." -NoNewline
        $Host.UI.ReadLine()
    }
    exit $ExitCode
}

function Test-DockerRunning {
    try {
        $null = docker info 2>&1
        if ($LASTEXITCODE -ne 0) {
            return $false
        }
        return $true
    }
    catch {
        return $false
    }
}

function Get-HiClawTimeZone {
    try {
        if ($env:HICLAW_TIMEZONE) {
            return $env:HICLAW_TIMEZONE
        }

        $tz = (Get-TimeZone).Id
        # Convert Windows timezone to IANA format
        $tzMap = @{
            "China Standard Time" = "Asia/Shanghai"
            "Pacific Standard Time" = "America/Los_Angeles"
            "Mountain Standard Time" = "America/Denver"
            "Central Standard Time" = "America/Chicago"
            "Eastern Standard Time" = "America/New_York"
            "GMT Standard Time" = "Europe/London"
            "Central European Standard Time" = "Europe/Berlin"
            "Tokyo Standard Time" = "Asia/Tokyo"
            "Singapore Standard Time" = "Asia/Singapore"
            "Korea Standard Time" = "Asia/Seoul"
            "India Standard Time" = "Asia/Kolkata"
        }

        if ($tzMap.ContainsKey($tz)) {
            return $tzMap[$tz]
        }
        return $tz
    }
    catch {
        return "Asia/Shanghai"
    }
}

function Get-Registry {
    param([string]$Timezone)

    if ($env:HICLAW_REGISTRY) {
        return $env:HICLAW_REGISTRY
    }

    # Americas
    if ($Timezone -match "^America/") {
        return "higress-registry.us-west-1.cr.aliyuncs.com"
    }

    # Southeast Asia
    if ($Timezone -match "^(Asia/Singapore|Asia/Bangkok|Asia/Jakarta|Asia/Kuala_Lumpur|Asia/Ho_Chi_Minh|Asia/Manila|Asia/Yangon)") {
        return "higress-registry.ap-southeast-7.cr.aliyuncs.com"
    }

    # Default: China
    return "higress-registry.cn-hangzhou.cr.aliyuncs.com"
}

function Get-HiClawLanguage {
    param([string]$Timezone)
    $chineseZones = @(
        "Asia/Shanghai", "Asia/Chongqing", "Asia/Harbin", "Asia/Urumqi",
        "Asia/Taipei", "Asia/Hong_Kong", "Asia/Macau"
    )
    if ($chineseZones -contains $Timezone) { return "zh" }
    return "en"
}

# ============================================================
# Centralized message dictionary and Get-Msg function
# ============================================================

$script:Messages = @{
    # --- Timezone detection messages ---
    "tz.warning.title" = @{ zh = "无法自动检测时区。"; en = "Could not detect timezone automatically." }
    "tz.warning.prompt" = @{ zh = "请输入您的时区（例如 Asia/Shanghai、America/New_York）。"; en = "Please enter your timezone (e.g., Asia/Shanghai, America/New_York)." }
    "tz.default" = @{ zh = "使用默认时区: {0}"; en = "Using default timezone: {0}" }
    "tz.input_prompt" = @{ zh = "时区"; en = "Timezone" }

    # --- Installation title and info ---
    "install.title" = @{ zh = "=== HiClaw Manager 安装 ==="; en = "=== HiClaw Manager Installation ===" }
    "install.registry" = @{ zh = "镜像仓库: {0}"; en = "Registry: {0}" }
    "install.dir" = @{ zh = "安装目录: {0}"; en = "Installation directory: {0}" }
    "install.dir_hint" = @{ zh = "  （env 文件 'hiclaw-manager.env' 将保存到 HOME 目录。）"; en = "  (The env file 'hiclaw-manager.env' will be saved to your HOME directory.)" }
    "install.dir_hint2" = @{ zh = "  （请从您希望管理此安装的目录运行此脚本。）"; en = "  (Run this script from the directory where you want to manage this installation.)" }

    # --- Onboarding mode ---
    "install.mode.title" = @{ zh = "--- Onboarding 模式 ---"; en = "--- Onboarding Mode ---" }
    "install.mode.choose" = @{ zh = "选择安装模式:"; en = "Choose your installation mode:" }
    "install.mode.quickstart" = @{ zh = "  1) 快速开始  - 使用阿里云百炼快速安装（推荐）"; en = "  1) Quick Start  - Fast installation with Alibaba Cloud CodingPlan (recommended)" }
    "install.mode.manual" = @{ zh = "  2) 手动配置  - 选择 LLM 提供商并自定义选项"; en = "  2) Manual       - Choose LLM provider and customize options" }
    "install.mode.prompt" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "install.mode.quickstart_selected" = @{ zh = "已选择快速开始模式 - 使用阿里云百炼"; en = "Quick Start mode selected - using Alibaba Cloud CodingPlan" }
    "install.mode.manual_selected" = @{ zh = "已选择手动配置模式 - 您将选择 LLM 提供商并自定义选项"; en = "Manual mode selected - you will choose LLM provider and customize options" }
    "install.mode.invalid" = @{ zh = "无效选择，默认使用快速开始模式"; en = "Invalid choice, defaulting to Quick Start mode" }

    # --- Existing installation detected ---
    "install.existing.detected" = @{ zh = "检测到已有 Manager 安装（env 文件: {0}）"; en = "Existing Manager installation detected (env file: {0})" }
    "install.existing.choose" = @{ zh = "选择操作:"; en = "Choose an action:" }
    "install.existing.upgrade" = @{ zh = "  1) 就地升级（保留数据、工作空间、env 文件）"; en = "  1) In-place upgrade (keep data, workspace, env file)" }
    "install.existing.reinstall" = @{ zh = "  2) 全新重装（删除所有数据，重新开始）"; en = "  2) Clean reinstall (remove all data, start fresh)" }
    "install.existing.cancel" = @{ zh = "  3) 取消"; en = "  3) Cancel" }
    "install.existing.prompt" = @{ zh = "请选择 [1/2/3]"; en = "Enter choice [1/2/3]" }
    "install.existing.upgrade_noninteractive" = @{ zh = "非交互模式: 执行就地升级..."; en = "Non-interactive mode: performing in-place upgrade..." }
    "install.existing.upgrading" = @{ zh = "执行就地升级..."; en = "Performing in-place upgrade..." }
    "install.existing.warn_manager_stop" = @{ zh = "警告: Manager 容器将被停止并重新创建。"; en = "WARNING: Manager container will be stopped and recreated." }
    "install.existing.warn_worker_recreate" = @{ zh = "警告: Worker 容器也将被重新创建（以更新 Manager IP）。"; en = "WARNING: Worker containers will also be recreated (to update Manager IP in hosts)." }
    "install.existing.continue_prompt" = @{ zh = "继续？[y/N]"; en = "Continue? [y/N]" }
    "install.existing.cancelled" = @{ zh = "安装已取消。"; en = "Installation cancelled." }
    "install.existing.stopping_manager" = @{ zh = "停止并移除现有 manager 容器..."; en = "Stopping and removing existing manager container..." }
    "install.existing.stopping_workers" = @{ zh = "停止并移除现有 worker 容器..."; en = "Stopping and removing existing worker containers..." }
    "install.existing.removed" = @{ zh = "  已移除: {0}"; en = "  Removed: {0}" }

    # --- Clean reinstall messages ---
    "install.reinstall.performing" = @{ zh = "执行全新重装..."; en = "Performing clean reinstall..." }
    "install.reinstall.warn_stop" = @{ zh = "警告: 以下运行中的容器将被停止:"; en = "WARNING: The following running containers will be stopped:" }
    "install.reinstall.warn_delete" = @{ zh = "警告: 以下内容将被删除:"; en = "WARNING: This will DELETE the following:" }
    "install.reinstall.warn_volume" = @{ zh = "   - Docker 卷: hiclaw-data"; en = "   - Docker volume: hiclaw-data" }
    "install.reinstall.warn_env" = @{ zh = "   - Env 文件: {0}"; en = "   - Env file: {0}" }
    "install.reinstall.warn_workspace" = @{ zh = "   - Manager 工作空间: {0}"; en = "   - Manager workspace: {0}" }
    "install.reinstall.warn_workers" = @{ zh = "   - 所有 worker 容器"; en = "   - All worker containers" }
    "install.reinstall.confirm_type" = @{ zh = "请输入工作空间路径以确认删除（或按 Ctrl+C 取消）:"; en = "To confirm deletion, please type the workspace path:" }
    "install.reinstall.confirm_path" = @{ zh = "输入路径以确认（或按 Ctrl+C 取消）"; en = "Type the path to confirm (or press Ctrl+C to cancel)" }
    "install.reinstall.path_mismatch" = @{ zh = "路径不匹配。中止重装。输入: '{0}'，期望: '{1}'"; en = "Path mismatch. Aborting reinstall. Input: '{0}', Expected: '{1}'" }
    "install.reinstall.confirmed" = @{ zh = "已确认。正在清理..."; en = "Confirmed. Cleaning up..." }
    "install.reinstall.removed_worker" = @{ zh = "  已移除 worker: {0}"; en = "  Removed worker: {0}" }
    "install.reinstall.removing_volume" = @{ zh = "正在移除 Docker 卷: hiclaw-data"; en = "Removing Docker volume: hiclaw-data" }
    "install.reinstall.warn_volume_fail" = @{ zh = "  警告: 无法移除卷（可能有引用）"; en = "  Warning: Could not remove volume (may have references)" }
    "install.reinstall.removing_workspace" = @{ zh = "正在移除工作空间目录: {0}"; en = "Removing workspace directory: {0}" }
    "install.reinstall.removing_env" = @{ zh = "正在移除 env 文件: {0}"; en = "Removing env file: {0}" }
    "install.reinstall.cleanup_done" = @{ zh = "清理完成。开始全新安装..."; en = "Cleanup complete. Starting fresh installation..." }
    "install.reinstall.failed_rm_workspace" = @{ zh = "无法移除工作空间目录"; en = "Failed to remove workspace directory" }

    # --- Orphan volume detection ---
    "install.orphan_volume.detected" = @{ zh = "⚠️  检测到残留数据卷 '{0}'，但未找到对应的 env 配置文件。"; en = "⚠️  Found leftover data volume '{0}' but no matching env config file." }
    "install.orphan_volume.warn" = @{ zh = "这可能是之前安装的残留数据，会导致新安装出现异常（如密码冲突、服务启动失败）。"; en = "This is likely leftover data from a previous installation and may cause issues (e.g., credential conflicts, service startup failures)." }
    "install.orphan_volume.choose" = @{ zh = "选择操作:"; en = "Choose an action:" }
    "install.orphan_volume.clean" = @{ zh = "  1) 清理残留数据卷后继续安装（推荐）"; en = "  1) Remove leftover volume and continue installation (recommended)" }
    "install.orphan_volume.keep" = @{ zh = "  2) 保留数据卷继续安装（可能出现异常）"; en = "  2) Keep the volume and continue installation (may cause issues)" }
    "install.orphan_volume.prompt" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "install.orphan_volume.cleaning" = @{ zh = "正在清理残留数据卷..."; en = "Removing leftover data volume..." }
    "install.orphan_volume.cleaned" = @{ zh = "残留数据卷已清理。继续全新安装..."; en = "Leftover volume removed. Continuing with fresh installation..." }
    "install.orphan_volume.keeping" = @{ zh = "保留数据卷，继续安装。如遇异常请选择全新重装。"; en = "Keeping existing volume. If you encounter issues, consider a clean reinstall." }
    "install.orphan_volume.clean_noninteractive" = @{ zh = "非交互模式: 自动清理残留数据卷..."; en = "Non-interactive mode: automatically removing leftover volume..." }

    # --- Loading existing config ---
    "install.loading_config" = @{ zh = "从 {0} 加载已有配置（shell 环境变量优先）..."; en = "Loading existing config from {0} (shell env vars take priority)..." }

    # --- LLM Configuration ---
    "llm.title" = @{ zh = "--- LLM 配置 ---"; en = "--- LLM Configuration ---" }
    "llm.provider.label" = @{ zh = "  提供商: {0}"; en = "  Provider: {0}" }
    "llm.model.label" = @{ zh = "  模型: {0}"; en = "  Model: {0}" }
    "llm.provider.qwen" = @{ zh = "  提供商: qwen（阿里云百炼）"; en = "  Provider: qwen (Alibaba Cloud Bailian)" }
    "llm.provider.qwen_default" = @{ zh = "  提供商: {0}（默认）"; en = "  Provider: {0} (default)" }
    "llm.model.default" = @{ zh = "  模型: {0}（默认）"; en = "  Model: {0} (default)" }
    "llm.apikey_hint" = @{ zh = "  提示: 获取阿里云百炼 API Key:"; en = "  Hint: Get your Alibaba Cloud CodingPlan API Key from:" }
    "llm.apikey_url" = @{ zh = "     https://www.aliyun.com/product/bailian"; en = "     https://www.alibabacloud.com/en/campaign/ai-scene-coding" }
    "llm.apikey_prompt" = @{ zh = "LLM API Key"; en = "LLM API Key" }
    "llm.providers_title" = @{ zh = "可用 LLM 提供商:"; en = "Available LLM Providers:" }
    "llm.provider.alibaba" = @{ zh = "  1) 阿里云百炼  - 推荐中国用户使用"; en = "  1) Alibaba Cloud CodingPlan  - Optimized for coding tasks (recommended)" }
    "llm.provider.openai_compat" = @{ zh = "  2) OpenAI 兼容 API  - 自定义 Base URL（OpenAI、DeepSeek 等）"; en = "  2) OpenAI-compatible API  - Custom Base URL (OpenAI, DeepSeek, etc.)" }
    "llm.provider.select" = @{ zh = "选择提供商 [1/2]"; en = "Select provider [1/2]" }
    "llm.alibaba.models_title" = @{ zh = "选择百炼模型系列:"; en = "Select Bailian model series:" }
    "llm.alibaba.model.codingplan" = @{ zh = "  1) CodingPlan  - 专为编程任务优化（推荐）"; en = "  1) CodingPlan  - Optimized for coding tasks (recommended)" }
    "llm.alibaba.model.qwen" = @{ zh = "  2) 百炼通用接口"; en = "  2) qwen general  - General purpose LLM" }
    "llm.alibaba.model.select" = @{ zh = "选择模型系列 [1/2]"; en = "Select model series [1/2]" }
    "llm.codingplan.models_title" = @{ zh = "选择 CodingPlan 默认模型:"; en = "Select CodingPlan default model:" }
    "llm.codingplan.model.qwen35plus" = @{ zh = "  1) qwen3.5-plus  - 千问 3.5（速度最快）"; en = "  1) qwen3.5-plus  - Qwen 3.5 (fastest)" }
    "llm.codingplan.model.glm5" = @{ zh = "  2) glm-5  - 智谱 GLM-5（编程推荐）"; en = "  2) glm-5  - Zhipu GLM-5 (recommended for coding)" }
    "llm.codingplan.model.kimi" = @{ zh = "  3) kimi-k2.5  - Moonshot Kimi K2.5"; en = "  3) kimi-k2.5  - Moonshot Kimi K2.5" }
    "llm.codingplan.model.minimax" = @{ zh = "  4) MiniMax-M2.5  - MiniMax M2.5"; en = "  4) MiniMax-M2.5  - MiniMax M2.5" }
    "llm.codingplan.model.select" = @{ zh = "选择模型 [1/2/3/4]"; en = "Select model [1/2/3/4]" }
    "llm.provider.selected_codingplan" = @{ zh = "  提供商: 阿里云百炼 CodingPlan"; en = "  Provider: Alibaba Cloud CodingPlan" }
    "llm.provider.selected_qwen" = @{ zh = "  提供商: 阿里云百炼"; en = "  Provider: Alibaba Cloud Bailian" }
    "llm.provider.selected_openai" = @{ zh = "  提供商: {0}（OpenAI 兼容）"; en = "  Provider: {0} (OpenAI-compatible)" }
    "llm.provider.invalid" = @{ zh = "无效选择: {0}（请输入 1 或 2）"; en = "Invalid choice: {0} (please enter 1 or 2)" }
    "llm.qwen.model_prompt" = @{ zh = "默认模型 ID [qwen3.5-plus]"; en = "Default Model ID [qwen3.5-plus]" }
    "llm.openai.base_url_prompt" = @{ zh = "Base URL（例如 https://api.openai.com/v1）"; en = "Base URL (e.g., https://api.openai.com/v1)" }
    "llm.openai.model_prompt" = @{ zh = "默认模型 ID [gpt-5.4]"; en = "Default Model ID [gpt-5.4]" }
    "llm.openai.base_url_label" = @{ zh = "  Base URL: {0}"; en = "  Base URL: {0}" }

    # --- Custom model parameters ---
    "llm.custom_model.detected" = @{ zh = "  ⚠️  模型 '{0}' 不在内置模型列表中，请配置模型参数:"; en = "  ⚠️  Model '{0}' is not in the built-in model list. Please configure model parameters:" }
    "llm.custom_model.context_prompt" = @{ zh = "最大上下文长度（token 数）[150000]"; en = "Max context window (tokens) [150000]" }
    "llm.custom_model.max_tokens_prompt" = @{ zh = "最大输出长度（token 数）[128000]"; en = "Max output tokens [128000]" }
    "llm.custom_model.reasoning_prompt" = @{ zh = "是否支持推理/思考模式？[Y/n]"; en = "Does it support reasoning/thinking mode? [Y/n]" }
    "llm.custom_model.vision_prompt" = @{ zh = "是否支持图片输入？[y/N]"; en = "Does it support image input? [y/N]" }
    "llm.custom_model.summary" = @{ zh = "  自定义模型参数: 上下文={0}, 最大输出={1}, 推理={2}, 图片={3}"; en = "  Custom model params: context={0}, maxTokens={1}, reasoning={2}, vision={3}" }

    # --- Admin Credentials ---
    "admin.title" = @{ zh = "--- 管理员凭据 ---"; en = "--- Admin Credentials ---" }
    "admin.username_prompt" = @{ zh = "管理员用户名"; en = "Admin Username" }
    "admin.password_prompt" = @{ zh = "管理员密码（留空自动生成，最少 8 位）"; en = "Admin Password (leave empty to auto-generate, min 8 chars)" }
    "admin.password_generated" = @{ zh = "  已自动生成管理员密码"; en = "  Auto-generated admin password" }
    "admin.password_too_short" = @{ zh = "管理员密码至少需要 8 个字符（MinIO 要求）。当前长度: {0}"; en = "Admin password must be at least 8 characters (MinIO requirement). Current length: {0}" }

    # --- Port Configuration ---
    "port.title" = @{ zh = "--- 端口配置（按回车使用默认值）---"; en = "--- Port Configuration (press Enter for defaults) ---" }
    "port.gateway_prompt" = @{ zh = "网关主机端口（容器内 8080）"; en = "Host port for gateway (8080 inside container)" }
    "port.console_prompt" = @{ zh = "Higress 控制台主机端口（容器内 8001）"; en = "Host port for Higress console (8001 inside container)" }
    "port.element_prompt" = @{ zh = "Element Web 直接访问主机端口（容器内 8088）"; en = "Host port for Element Web direct access (8088 inside container)" }
    "port.openclaw_console_prompt" = @{ zh = "OpenClaw 控制台主机端口（容器内 18888）"; en = "Host port for OpenClaw console (18888 inside container)" }
    "port.local_only.title" = @{ zh = "--- 网络访问模式 ---"; en = "--- Network Access Mode ---" }
    "port.local_only.hint_yes" = @{ zh = "  仅本机使用，无需开放外部端口（推荐）"; en = "  Local use only, no external port exposure (recommended)" }
    "port.local_only.hint_no" = @{ zh = "  允许外部访问（局域网 / 公网）"; en = "  Allow external access (LAN / public network)" }
    "port.local_only.choice" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "port.local_only.selected_local" = @{ zh = "端口已绑定到 127.0.0.1（仅本机访问）"; en = "Ports bound to 127.0.0.1 (localhost only)" }
    "port.local_only.selected_external" = @{ zh = "端口已绑定到所有网络接口（0.0.0.0）"; en = "Ports bound to all interfaces (0.0.0.0)" }
    "port.local_only.https_hint" = @{ zh = "警告: 建议在 Higress 控制台配置 TLS 证书并启用 HTTPS，避免明文传输。"; en = "WARNING: It is recommended to configure TLS certificates and enable HTTPS in the Higress Console to avoid plaintext transmission." }
    "port.local_only.https_docs" = @{ zh = ""; en = "" }

    # --- Domain Configuration ---
    "domain.title" = @{ zh = "--- 域名配置（按回车使用默认值）---"; en = "--- Domain Configuration (press Enter for defaults) ---" }
    "domain.hint" = @{ zh = "提示: 自定义域名前必须事先做好 DNS 解析。单机 ECS 部署时无需修改 aigw、fs 等域名；Element Web 和 Matrix Server 也可通过 IP 直接访问。"; en = "Hint: Configure DNS resolution before customizing domains. For single ECS deployment, no need to change aigw, fs, etc.; Element Web and Matrix Server can also be accessed directly via IP." }
    "domain.matrix_prompt" = @{ zh = "Matrix 域名"; en = "Matrix Domain" }
    "domain.element_prompt" = @{ zh = "Element Web 域名"; en = "Element Web Domain" }
    "domain.gateway_prompt" = @{ zh = "AI 网关域名"; en = "AI Gateway Domain" }
    "domain.fs_prompt" = @{ zh = "文件系统域名"; en = "File System Domain" }
    "domain.console_prompt" = @{ zh = "OpenClaw 控制台域名"; en = "OpenClaw Console Domain" }

    # --- GitHub Integration ---
    "github.title" = @{ zh = "--- GitHub 集成（可选，按回车跳过）---"; en = "--- GitHub Integration (optional, press Enter to skip) ---" }
    "github.token_prompt" = @{ zh = "GitHub 个人访问令牌（可选）"; en = "GitHub Personal Access Token (optional)" }

    # --- Skills Registry ---
    "skills.title" = @{ zh = "--- Skills 注册中心（可选，按回车使用默认 https://skills.sh）---"; en = "--- Skills Registry (optional, press Enter for default https://skills.sh) ---" }
    "skills.url_prompt" = @{ zh = "Skills 注册中心 URL（留空使用默认 https://skills.sh）"; en = "Skills Registry URL (leave empty for default https://skills.sh)" }

    # --- Data Persistence ---
    "data.title" = @{ zh = "--- 数据持久化 ---"; en = "--- Data Persistence ---" }
    "data.volume_prompt" = @{ zh = "Docker 卷名称 [hiclaw-data]"; en = "Docker volume name for persistent data [hiclaw-data]" }
    "data.volume_using" = @{ zh = "  使用 Docker 卷: {0}"; en = "  Using Docker volume: {0}" }

    # --- Manager Workspace ---
    "workspace.title" = @{ zh = "--- Manager 工作空间 ---"; en = "--- Manager Workspace ---" }
    "workspace.dir_prompt" = @{ zh = "Manager 工作空间目录 [{0}]"; en = "Manager workspace directory [{0}]" }
    "workspace.dir_label" = @{ zh = "  Manager 工作空间: {0}"; en = "  Manager workspace: {0}" }

    # --- Host directory sharing ---
    "host_share.prompt" = @{ zh = "与 Agent 共享的主机目录（默认: {0}）"; en = "Host directory to share with agents (default: {0})" }
    "host_share.sharing" = @{ zh = "共享主机目录: {0} -> 容器内 /host-share"; en = "Sharing host directory: {0} -> /host-share in container" }
    "host_share.not_exist" = @{ zh = "警告: 主机目录 {0} 不存在，跳过验证继续使用"; en = "WARNING: Host directory {0} does not exist, using without validation" }

    # --- Default worker runtime ---
    "worker_runtime.title" = @{ zh = "--- 默认 Worker 运行时 ---"; en = "--- Default Worker Runtime ---" }
    "worker_runtime.openclaw" = @{ zh = "OpenClaw（Node.js 容器，~500MB 内存）"; en = "OpenClaw (Node.js container, ~500MB RAM)" }
    "worker_runtime.copaw" = @{ zh = "CoPaw（Python 容器，~150MB 内存，默认关闭控制台，可跟 Manager 对话按需开启）"; en = "CoPaw (Python container, ~150MB RAM, console off by default, enable on demand via Manager)" }
    "worker_runtime.choice" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "worker_runtime.selected" = @{ zh = "默认 Worker 运行时: {0}"; en = "Default Worker runtime: {0}" }
    "worker_runtime.title_short" = @{ zh = "默认 Worker 运行时"; en = "Default Worker Runtime" }

    # --- Matrix E2EE ---
    "matrix_e2ee.title" = @{ zh = "--- Matrix 端到端加密（E2EE）---"; en = "--- Matrix End-to-End Encryption (E2EE) ---" }
    "matrix_e2ee.desc" = @{
        zh = "E2EE 会对 Manager 与 Worker 之间的 Matrix 消息进行端到端加密。`n  启用后，即使 Matrix 服务器被入侵，消息内容也无法被窃取。`n  但 E2EE 会增加首次握手耗时，且要求所有 Agent 都支持 matrix-sdk-crypto。`n  如果不确定，建议保持禁用。`n  ⚠ 注意：禁用 E2EE 后，请勿在 Element 上创建默认启用加密的 Private 房间，`n  否则 Agent 将无法读取该房间中的加密消息。请改用 Public 房间或关闭房间加密。"
        en = "E2EE encrypts Matrix messages between Manager and Workers end-to-end.`n  When enabled, message content stays private even if the Matrix server is compromised.`n  However, E2EE adds overhead to the initial handshake and requires all Agents`n  to support matrix-sdk-crypto. If unsure, keep it disabled.`n  ⚠ Note: When E2EE is disabled, do NOT create Private rooms in Element (which`n  enable encryption by default) — Agents cannot read encrypted messages without`n  E2EE support. Use Public rooms or turn off room encryption instead."
    }
    "matrix_e2ee.enable" = @{ zh = "启用 E2EE"; en = "Enable E2EE" }
    "matrix_e2ee.disable" = @{ zh = "禁用 E2EE（推荐）"; en = "Disable E2EE (recommended)" }
    "matrix_e2ee.choice" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "matrix_e2ee.selected_enabled" = @{ zh = "Matrix E2EE: 已启用"; en = "Matrix E2EE: enabled" }
    "matrix_e2ee.selected_disabled" = @{ zh = "Matrix E2EE: 已禁用（默认）"; en = "Matrix E2EE: disabled (default)" }
    "matrix_e2ee.title_short" = @{ zh = "Matrix E2EE"; en = "Matrix E2EE" }
    "matrix_e2ee.val_enabled" = @{ zh = "已启用"; en = "enabled" }
    "matrix_e2ee.val_disabled" = @{ zh = "已禁用"; en = "disabled" }

    # --- Docker API proxy ---
    "docker_proxy.title" = @{ zh = "--- Docker API 安全代理 ---"; en = "--- Docker API Security Proxy ---" }
    "docker_proxy.desc" = @{ zh = "Docker API 代理可防止 AI Agent 通过 Docker API 越狱访问宿主机。`n  启用后，Manager 不再直接持有 Docker socket，所有容器操作经过安全校验。"; en = "Docker API proxy prevents AI Agents from escaping via Docker API to access the host.`n  When enabled, Manager no longer has direct Docker socket access; all container operations go through security validation." }
    "docker_proxy.enable" = @{ zh = "启用（推荐）"; en = "Enable (recommended)" }
    "docker_proxy.disable" = @{ zh = "禁用（直接挂载 Docker socket）"; en = "Disable (mount Docker socket directly)" }
    "docker_proxy.choice" = @{ zh = "请选择 [1/2]"; en = "Enter choice [1/2]" }
    "docker_proxy.selected_enabled" = @{ zh = "Docker API 代理: 已启用"; en = "Docker API proxy: enabled" }
    "docker_proxy.selected_disabled" = @{ zh = "Docker API 代理: 已禁用"; en = "Docker API proxy: disabled" }
    "docker_proxy.title_short" = @{ zh = "Docker API 代理"; en = "Docker API Proxy" }
    "docker_proxy.val_enabled" = @{ zh = "已启用"; en = "enabled" }
    "docker_proxy.val_disabled" = @{ zh = "已禁用"; en = "disabled" }
    "docker_proxy.registries_desc" = @{ zh = "默认放行的镜像来源：本地镜像、localhost、Higress 仓库（所有 region）。`n  如需放行其他镜像仓库，请输入逗号分隔的地址前缀。`n  示例: ghcr.io/myorg,registry.example.com/team"; en = "Default allowed image sources: local images, localhost, Higress registries (all regions).`n  To allow additional image sources, enter comma-separated address prefixes.`n  Example: ghcr.io/myorg,registry.example.com/team" }
    "docker_proxy.registries_prompt" = @{ zh = "额外放行的镜像来源（按回车跳过）"; en = "Additional allowed image sources (press Enter to skip)" }
    "docker_proxy.registries_label" = @{ zh = "额外放行的镜像来源"; en = "Additional allowed image sources" }

    # --- Worker idle timeout ---
    "idle_timeout.prompt" = @{ zh = "Worker 空闲自动停止超时（分钟）[720]"; en = "Worker idle auto-stop timeout in minutes [720]" }
    "idle_timeout.selected" = @{ zh = "Worker 空闲超时: {0} 分钟"; en = "Worker idle timeout: {0} minutes" }
    "idle_timeout.label" = @{ zh = "Worker 空闲超时（分钟）"; en = "Worker idle timeout (min)" }

    # --- Secrets and config ---
    "install.generating_secrets" = @{ zh = "正在生成密钥..."; en = "Generating secrets..." }
    "install.config_saved" = @{ zh = "配置已保存到 {0}"; en = "Configuration saved to {0}" }

    # --- Container runtime socket ---
    "install.socket_detected" = @{ zh = "容器运行时 socket: {0}（已启用直接创建 Worker）"; en = "Container runtime socket: {0} (direct Worker creation enabled)" }
    "install.socket_not_found" = @{ zh = "未找到容器运行时 socket（Manager 无法直接创建 Worker 容器，需要你手动执行 docker 命令创建）"; en = "No container runtime socket found (Manager cannot create Worker containers directly, you will need to create them manually using docker commands)" }
    "install.socket_confirm.title" = @{ zh = "警告: 未检测到容器运行时 Socket"; en = "WARNING: Container Runtime Socket Not Detected" }
    "install.socket_confirm.message" = @{ zh = "未找到 Docker/Podman socket，Manager 将无法自动创建 Worker 容器。`n你需要手动执行 docker run 命令来创建 Worker。`n`n是否继续安装？"; en = "Docker/Podman socket not found. Manager will not be able to create Worker containers automatically.`nYou will need to manually run docker commands to create Workers.`n`nContinue installation?" }
    "install.socket_confirm.prompt" = @{ zh = "继续安装? [y/N]: "; en = "Continue? [y/N]: " }
    "install.socket_confirm.cancelled" = @{ zh = "安装已取消。如需启用 Worker 自动创建，请确保 Docker/Podman 正在运行，然后重新运行安装脚本。"; en = "Installation cancelled. To enable automatic Worker creation, ensure Docker/Podman is running and re-run the installer." }

    # --- Container management ---
    "install.removing_existing" = @{ zh = "正在移除现有 hiclaw-manager 容器..."; en = "Removing existing hiclaw-manager container..." }

    # --- YOLO mode ---
    "install.yolo" = @{ zh = "YOLO 模式已启用（自主决策，无交互提示）"; en = "YOLO mode enabled (autonomous decisions, no interactive prompts)" }

    # --- Image pulling ---
    "install.image.exists" = @{ zh = "Manager 镜像已存在: {0}"; en = "Manager image already exists locally: {0}" }
    "install.image.pulling_manager" = @{ zh = "正在拉取 Manager 镜像: {0}"; en = "Pulling Manager image: {0}" }
    "install.image.worker_exists" = @{ zh = "Worker 镜像已存在: {0}"; en = "Worker image already exists locally: {0}" }
    "install.image.pulling_worker" = @{ zh = "正在拉取 Worker 镜像: {0}"; en = "Pulling Worker image: {0}" }

    # --- Starting container ---
    "install.starting_manager" = @{ zh = "正在启动 Manager 容器..."; en = "Starting Manager container..." }

    # --- Wait for Manager ready ---
    "install.wait_ready" = @{ zh = "等待 Manager Agent 就绪（超时: {0}s）..."; en = "Waiting for Manager agent to be ready (timeout: {0}s)..." }
    "install.wait_ready.ok" = @{ zh = "Manager Agent 已就绪！"; en = "Manager agent is ready!" }
    "install.wait_ready.waiting" = @{ zh = "等待中... ({0}s/{1}s)"; en = "Waiting... ({0}s/{1}s)" }
    "install.wait_ready.timeout" = @{ zh = "Manager Agent 在 {0}s 内未就绪。请检查: docker logs {1}"; en = "Manager agent did not become ready within {0}s. Check: docker logs {1}" }

    # --- Wait for Matrix ready ---
    "install.wait_matrix" = @{ zh = "等待 Matrix 服务就绪（超时: {0}s）..."; en = "Waiting for Matrix server to be ready (timeout: {0}s)..." }
    "install.wait_matrix.ok" = @{ zh = "Matrix 服务已就绪！"; en = "Matrix server is ready!" }
    "install.wait_matrix.waiting" = @{ zh = "等待 Matrix 中... ({0}s/{1}s)"; en = "Waiting for Matrix... ({0}s/{1}s)" }
    "install.wait_matrix.timeout" = @{ zh = "Matrix 服务在 {0}s 内未就绪。请检查: docker logs {1}"; en = "Matrix server did not become ready within {0}s. Check: docker logs {1}" }

    # --- OpenAI-compatible connectivity test ---
    "llm.openai.test.testing" = @{ zh = "正在测试 API 联通性..."; en = "Testing API connectivity..." }
    "llm.openai.test.ok" = @{ zh = "API 联通性测试通过"; en = "API connectivity test passed" }
    "llm.openai.test.fail" = @{ zh = "API 联通性测试失败（HTTP {0}）。响应内容:`n{1}`n请根据以上错误信息联系您的模型服务商解决。"; en = "API connectivity test failed (HTTP {0}). Response body:`n{1}`nPlease contact your model provider to resolve the issue." }
    "llm.openai.test.fail.codingplan" = @{ zh = "提示: 请确认您的 API Key 已开通阿里云百炼 CodingPlan 服务。开通地址: https://www.aliyun.com/benefit/scene/codingplan"; en = "Hint: Please verify that your API Key has CodingPlan service enabled. Enable at: https://www.alibabacloud.com/en/campaign/ai-scene-coding" }
    "llm.openai.test.confirm" = @{ zh = "是否仍要继续安装？[y/N/b]"; en = "Continue with installation anyway? [y/N/b]" }
    "llm.openai.test.aborted" = @{ zh = "安装已中止。"; en = "Installation aborted." }
    "nav.back_hint" = @{ zh = "（输入 b 返回上一步）"; en = "(enter b to go back)" }
    # --- OpenAI-compatible provider creation ---
    "install.openai_compat.missing" = @{ zh = "警告: OpenAI Base URL 或 API Key 未设置，跳过提供商创建"; en = "WARNING: OpenAI Base URL or API Key not set, skipping provider creation" }
    "install.openai_compat.creating" = @{ zh = "正在创建 OpenAI 兼容提供商..."; en = "Creating OpenAI-compatible provider..." }
    "install.openai_compat.domain" = @{ zh = "  域名: {0}"; en = "  Domain: {0}" }
    "install.openai_compat.port" = @{ zh = "  端口: {0}"; en = "  Port: {0}" }
    "install.openai_compat.protocol" = @{ zh = "  协议: {0}"; en = "  Protocol: {0}" }
    "install.openai_compat.service_fail" = @{ zh = "警告: 创建 DNS 服务源失败（可能已存在）"; en = "WARNING: Failed to create DNS service source (may already exist)" }
    "install.openai_compat.provider_fail" = @{ zh = "警告: 创建 AI 提供商失败（可能已存在）"; en = "WARNING: Failed to create AI provider (may already exist)" }
    "install.openai_compat.success" = @{ zh = "OpenAI 兼容提供商创建成功"; en = "OpenAI-compatible provider created successfully" }

    # --- Welcome message ---
    "install.welcome_msg.soul_configured" = @{ zh = "Soul 已配置（找到 soul-configured 标记），跳过 onboarding 消息"; en = "Soul already configured (soul-configured marker found), skipping onboarding message" }
    "install.welcome_msg.logging_in" = @{ zh = "正在以 {0} 身份登录以发送欢迎消息..."; en = "Logging in as {0} to send welcome message..." }
    "install.welcome_msg.login_failed" = @{ zh = "警告: 以 {0} 身份登录失败，跳过欢迎消息"; en = "WARNING: Failed to login as {0}, skipping welcome message" }
    "install.welcome_msg.finding_room" = @{ zh = "正在查找与 Manager 的 DM 房间..."; en = "Finding DM room with Manager..." }
    "install.welcome_msg.creating_room" = @{ zh = "正在创建与 Manager 的 DM 房间..."; en = "Creating DM room with Manager..." }
    "install.welcome_msg.no_room" = @{ zh = "警告: 无法找到或创建与 Manager 的 DM 房间"; en = "WARNING: Could not find or create DM room with Manager" }
    "install.welcome_msg.waiting_join" = @{ zh = "等待 Manager 加入房间..."; en = "Waiting for Manager to join the room..." }
    "install.welcome_msg.sending" = @{ zh = "正在向 Manager 发送欢迎消息..."; en = "Sending welcome message to Manager..." }
    "install.welcome_msg.send_failed" = @{ zh = "警告: 发送欢迎消息失败"; en = "WARNING: Failed to send welcome message" }
    "install.welcome_msg.sent" = @{ zh = "欢迎消息已发送给 Manager"; en = "Welcome message sent to Manager" }

    # --- Final output panel ---
    "success.title" = @{ zh = "=== HiClaw Manager 已启动！==="; en = "=== HiClaw Manager Started! ===" }
    "success.domains_configured" = @{ zh = "以下域名已配置解析到 127.0.0.1:"; en = "The following domains are configured to resolve to 127.0.0.1:" }
    "success.open_url" = @{ zh = "  在浏览器中打开以下 URL 开始使用:                           "; en = "  Open the following URL in your browser to start:                           " }
    "success.login_with" = @{ zh = "  登录信息:"; en = "  Login with:" }
    "success.username" = @{ zh = "    用户名: {0}"; en = "    Username: {0}" }
    "success.password" = @{ zh = "    密码: {0}"; en = "    Password: {0}" }
    "success.after_login" = @{ zh = "  登录后，开始与 Manager 聊天！"; en = "  After login, start chatting with the Manager!" }
    "success.tell_it" = @{ zh = "    告诉它: `"创建一个名为 alice 的前端开发 Worker`""; en = "    Tell it: `"Create a Worker named alice for frontend dev`"" }
    "success.manager_auto" = @{ zh = "    Manager 会自动处理一切。"; en = "    The Manager will handle everything automatically." }
    "success.mobile_title" = @{ zh = "  移动端访问（FluffyChat / Element Mobile）:"; en = "  Mobile access (FluffyChat / Element Mobile):" }
    "success.mobile_step1" = @{ zh = "    1. 在手机上下载 FluffyChat 或 Element"; en = "    1. Download FluffyChat or Element on your phone" }
    "success.mobile_step2" = @{ zh = "    2. 设置 homeserver 为: {0}"; en = "    2. Set homeserver to: {0}" }
    "success.mobile_step2_noip" = @{ zh = "    2. 设置 homeserver 为: http://<本机局域网IP>:{0}"; en = "    2. Set homeserver to: http://<this-machine-LAN-IP>:{0}" }
    "success.mobile_noip_hint" = @{ zh = "       （无法自动检测局域网 IP — 请使用 ifconfig / ip addr 查看）"; en = "       (Could not detect LAN IP automatically — check with: ifconfig / ip addr)" }
    "success.mobile_step3" = @{ zh = "    3. 登录信息:"; en = "    3. Login with:" }
    "success.mobile_username" = @{ zh = "         用户名: {0}"; en = "         Username: {0}" }
    "success.mobile_password" = @{ zh = "         密码: {0}"; en = "         Password: {0}" }

    # --- Other consoles and tips ---
    "success.other_consoles" = @{ zh = "--- 其他控制台 ---"; en = "--- Other Consoles ---" }
    "success.higress_console" = @{ zh = "  Higress 控制台: http://localhost:{0}（用户名: {1} / 密码: {2}）"; en = "  Higress Console: http://localhost:{0} (Username: {1} / Password: {2})" }
    "success.openclaw_console" = @{ zh = "  OpenClaw 控制台（本地）: http://localhost:{0}（无需登录）"; en = "  OpenClaw Console (local): http://localhost:{0} (no login required)" }
    "success.openclaw_console_gateway" = @{ zh = "  OpenClaw 控制台（网关）: http://console-local.hiclaw.io（用户名: {0} / 密码: {1}）"; en = "  OpenClaw Console (gateway): http://console-local.hiclaw.io (Username: {0} / Password: {1})" }
    "success.switch_llm.title" = @{ zh = "--- 切换 LLM 提供商 ---"; en = "--- Switch LLM Providers ---" }
    "success.switch_llm.hint" = @{ zh = "  您可以通过 Higress 控制台切换到其他 LLM 提供商（OpenAI、Anthropic 等）。"; en = "  You can switch to other LLM providers (OpenAI, Anthropic, etc.) via Higress Console." }
    "success.switch_llm.docs" = @{ zh = "  详细说明请参阅:"; en = "  For detailed instructions, see:" }
    "success.switch_llm.url" = @{ zh = "  https://higress.ai/en/docs/ai/scene-guide/multi-proxy#console-configuration"; en = "  https://higress.ai/en/docs/ai/scene-guide/multi-proxy#console-configuration" }
    "success.tip" = @{ zh = "提示: 您也可以在聊天中让 Manager 为您配置 LLM 提供商。"; en = "Tip: You can also ask the Manager to configure LLM providers for you in the chat." }
    "success.config_file" = @{ zh = "配置文件: {0}"; en = "Configuration file: {0}" }
    "success.data_volume" = @{ zh = "数据卷:        {0}"; en = "Data volume:        {0}" }
    "success.workspace" = @{ zh = "Manager 工作空间:  {0}"; en = "Manager workspace:  {0}" }

    # --- Worker installation ---
    "worker.resetting" = @{ zh = "正在重置 Worker: {0}..."; en = "Resetting Worker: {0}..." }
    "worker.exists" = @{ zh = "容器 '{0}' 已存在。使用 --reset 重新创建。"; en = "Container '{0}' already exists. Use --reset to recreate." }
    "worker.starting" = @{ zh = "正在启动 Worker: {0}..."; en = "Starting Worker: {0}..." }
    "worker.skills_url" = @{ zh = "  Skills API URL: {0}"; en = "  Skills API URL: {0}" }
    "worker.started" = @{ zh = "=== Worker {0} 已启动！==="; en = "=== Worker {0} Started! ===" }
    "worker.container" = @{ zh = "容器: {0}"; en = "Container: {0}" }
    "worker.view_logs" = @{ zh = "查看日志: docker logs -f {0}"; en = "View logs: docker logs -f {0}" }

    # --- Prompt function messages ---
    "prompt.preset" = @{ zh = "  {0} = （已通过环境变量预设）"; en = "  {0} = (pre-set via env)" }
    "prompt.upgrade_keep" = @{ zh = "  {0} = {1}（当前值，回车保留 / 输入新值覆盖）"; en = "  {0} = {1} (current value, press Enter to keep / type new value to change)" }
    "prompt.upgrade_empty" = @{ zh = "  {0} = （未设置，回车跳过 / 输入新值设置）"; en = "  {0} = (not set, press Enter to skip / type new value to set)" }
    "prompt.default" = @{ zh = "  {0} = {1}（默认）"; en = "  {0} = {1} (default)" }
    "prompt.required" = @{ zh = "{0} 是必需的（在非交互模式下通过环境变量设置）"; en = "{0} is required (set via environment variable in non-interactive mode)" }
    "prompt.required_empty" = @{ zh = "{0} 是必需的"; en = "{0} is required" }

    # --- Language switch prompt (bilingual by design) ---
    "lang.detected.zh" = @{ zh = "检测到语言 / Detected language: 中文"; en = "检测到语言 / Detected language: 中文" }
    "lang.detected.en" = @{ zh = "检测到语言 / Detected language: English"; en = "检测到语言 / Detected language: English" }
    "lang.switch_title" = @{ zh = "切换语言 / Switch language:"; en = "切换语言 / Switch language:" }
    "lang.option_zh" = @{ zh = "  1) 中文"; en = "  1) 中文" }
    "lang.option_en" = @{ zh = "  2) English"; en = "  2) English" }
    "lang.prompt" = @{ zh = "请选择 / Enter choice"; en = "请选择 / Enter choice" }

    # --- Error messages ---
    "error.name_required" = @{ zh = "--name 是必需的"; en = "--name is required" }
    "error.fs_required" = @{ zh = "--fs 是必需的"; en = "--fs is required" }
    "error.fs_key_required" = @{ zh = "--fs-key 是必需的"; en = "--fs-key is required" }
    "error.fs_secret_required" = @{ zh = "--fs-secret 是必需的"; en = "--fs-secret is required" }
    "error.unknown_option" = @{ zh = "未知选项: {0}"; en = "Unknown option: {0}" }
    "error.docker_not_running" = @{ zh = "Docker 未运行。请先启动 Docker Desktop 或 Podman Desktop。"; en = "Docker is not running. Please start Docker Desktop or Podman Desktop first." }
    "error.docker_not_found" = @{ zh = "未找到 docker 或 podman 命令。请先安装 Docker Desktop 或 Podman Desktop：`n  Docker Desktop: https://www.docker.com/products/docker-desktop/`n  Podman Desktop: https://podman-desktop.io/"; en = "docker or podman command not found. Please install Docker Desktop or Podman Desktop first:`n  Docker Desktop: https://www.docker.com/products/docker-desktop/`n  Podman Desktop: https://podman-desktop.io/" }

    # --- Uninstall messages ---
    "uninstall.title" = @{ zh = "正在卸载 HiClaw..."; en = "Uninstalling HiClaw..." }
    "uninstall.stopping_manager" = @{ zh = "正在停止并移除 hiclaw-manager..."; en = "Stopping and removing hiclaw-manager..." }
    "uninstall.stopping_workers" = @{ zh = "正在停止并移除 worker 容器..."; en = "Stopping and removing worker containers..." }
    "uninstall.removed" = @{ zh = "  已移除: {0}"; en = "  Removed: {0}" }
    "uninstall.removing_volume" = @{ zh = "正在移除 Docker 卷: hiclaw-data"; en = "Removing Docker volume: hiclaw-data" }
    "uninstall.removing_env" = @{ zh = "正在移除 env 文件: {0}"; en = "Removing env file: {0}" }
    "uninstall.done" = @{ zh = "HiClaw 已卸载。"; en = "HiClaw has been uninstalled." }
    "uninstall.workspace_note" = @{ zh = "注意: Manager 工作空间目录已保留。如需删除请手动操作。"; en = "Note: Manager workspace directory was preserved. Remove manually if desired." }
}

# Get-Msg: look up message by key, with -f style argument substitution.
# Falls back to English if the current language translation is missing.
function Get-Msg {
    param(
        [Parameter(Mandatory)][string]$Key,
        [object[]]$f
    )
    $lang = $script:HICLAW_LANGUAGE
    if (-not $lang) { $lang = "en" }
    $entry = $script:Messages[$Key]
    if (-not $entry) { return $Key }
    $text = $entry[$lang]
    if (-not $text) { $text = $entry["en"] }
    if (-not $text) { return $Key }
    if ($f) { return ($text -f $f) }
    return $text
}

function Get-LanIP {
    # Detect local LAN IP address on Windows
    try {
        # Get network adapters with IPv4 addresses, prefer connected/active interfaces
        $adapters = Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
            Where-Object {
                $_.IPAddress -notlike "127.*" -and
                $_.PrefixOrigin -ne "WellKnown" -and
                $_.InterfaceAlias -notlike "*Loopback*"
            } |
            Sort-Object {
                # PowerShell 5.1 compatible: use if-else instead of ternary operator
                if ($_.InterfaceAlias -like "*Wi-Fi*" -or $_.InterfaceAlias -like "*Ethernet*") { 0 } else { 1 }
            }

        if ($adapters) {
            return $adapters[0].IPAddress
        }

        # Fallback: use ipconfig
        $ipconfig = ipconfig 2>$null
        $ip = ($ipconfig | Select-String "IPv4 Address.*?: (\d+\.\d+\.\d+\.\d+)" | Select-Object -First 1)
        if ($ip -match "(\d+\.\d+\.\d+\.\d+)") {
            return $Matches[1]
        }
    }
    catch {
        # Ignore errors
    }

    return ""
}

# Known models list — used to detect custom models during install
$script:KnownModels = @(
    "gpt-5.4", "gpt-5.3-codex", "gpt-5-mini", "gpt-5-nano",
    "claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5",
    "qwen3.5-plus", "deepseek-chat", "deepseek-reasoner",
    "kimi-k2.5", "glm-5", "MiniMax-M2.7", "MiniMax-M2.7-highspeed", "MiniMax-M2.5"
)

function Test-KnownModel {
    param([string]$Model)
    return $script:KnownModels -contains $Model
}

function Request-CustomModelParams {
    param([string]$Model)
    if (Test-KnownModel $Model) {
        $script:config.MODEL_CONTEXT_WINDOW = ""
        $script:config.MODEL_MAX_TOKENS = ""
        $script:config.MODEL_REASONING = ""
        $script:config.MODEL_VISION = ""
        return
    }
    Write-Host ""
    Write-Log (Get-Msg "llm.custom_model.detected" -f $Model)
    Write-Host ""
    $ctxInput = Read-Host "  $(Get-Msg 'llm.custom_model.context_prompt')"
    if ($ctxInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.MODEL_CONTEXT_WINDOW = if ($ctxInput) { $ctxInput } else { "150000" }
    $maxInput = Read-Host "  $(Get-Msg 'llm.custom_model.max_tokens_prompt')"
    if ($maxInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.MODEL_MAX_TOKENS = if ($maxInput) { $maxInput } else { "128000" }
    $reasoningInput = Read-Host "  $(Get-Msg 'llm.custom_model.reasoning_prompt')"
    if ($reasoningInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.MODEL_REASONING = if ($reasoningInput -match "^[nN]") { "false" } else { "true" }
    $visionInput = Read-Host "  $(Get-Msg 'llm.custom_model.vision_prompt')"
    if ($visionInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.MODEL_VISION = if ($visionInput -match "^[yY]") { "true" } else { "false" }
    Write-Log (Get-Msg "llm.custom_model.summary" -f $script:config.MODEL_CONTEXT_WINDOW, $script:config.MODEL_MAX_TOKENS, $script:config.MODEL_REASONING, $script:config.MODEL_VISION)
}

function New-RandomKey {
    # Generate 64 character hex string (32 bytes)
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $rng.GetBytes($bytes)
    return [BitConverter]::ToString($bytes).Replace("-", "").ToLower()
}

function ConvertTo-DockerPath {
    param([string]$Path)

    # Convert Windows path to Docker mount format
    $fullPath = (Resolve-Path $Path -ErrorAction SilentlyContinue).Path
    if (-not $fullPath) {
        $fullPath = $Path
    }

    # Convert C:\path to /c/path format for Docker
    if ($fullPath -match "^([A-Za-z]):") {
        $drive = $Matches[1].ToLower()
        $rest = $fullPath.Substring(2).Replace("\", "/")
        return "/$drive$rest"
    }
    return $fullPath.Replace("\", "/")
}

function Wait-ManagerReady {
    param(
        [string]$Container = "hiclaw-manager",
        [int]$Timeout = 300
    )

    $elapsed = 0
    Write-Log (Get-Msg "install.wait_ready" -f $Timeout)

    while ($elapsed -lt $Timeout) {
        try {
            $result = docker exec $Container openclaw gateway health --json 2>$null
            if ($result -match '"ok"') {
                Write-Log (Get-Msg "install.wait_ready.ok")
                return $true
            }
        } catch {
            # Ignore errors during polling
        }

        Start-Sleep -Seconds 5
        $elapsed += 5
        Write-Host "`r$($script:ESC)[36m[HiClaw]$($script:ESC)[0m $(Get-Msg 'install.wait_ready.waiting' -f $elapsed, $Timeout)" -NoNewline
    }

    Write-Host ""
    Write-Error (Get-Msg "install.wait_ready.timeout" -f $Timeout, $Container)
}

function Wait-MatrixReady {
    param(
        [string]$Container = "hiclaw-manager",
        [int]$Timeout = 300
    )

    $elapsed = 0
    Write-Log (Get-Msg "install.wait_matrix" -f $Timeout)

    while ($elapsed -lt $Timeout) {
        try {
            $result = docker exec $Container curl -sf http://127.0.0.1:6167/_tuwunel/server_version 2>$null
            if ($result) {
                Write-Log (Get-Msg "install.wait_matrix.ok")
                return $true
            }
        } catch {
            # Ignore errors during polling
        }

        Start-Sleep -Seconds 5
        $elapsed += 5
        Write-Host "`r$($script:ESC)[36m[HiClaw]$($script:ESC)[0m $(Get-Msg 'install.wait_matrix.waiting' -f $elapsed, $Timeout)" -NoNewline
    }

    Write-Host ""
    Write-Error (Get-Msg "install.wait_matrix.timeout" -f $Timeout, $Container)
}

function New-EnvFile {
    param([hashtable]$Config, [string]$Path)

    $content = @"
# HiClaw Manager Configuration
# Generated by hiclaw-install.ps1 on $(Get-Date -Format "yyyy-MM-dd HH:mm:ss")

# Language
HICLAW_LANGUAGE=$($Config.LANGUAGE)

# LLM
HICLAW_LLM_PROVIDER=$($Config.LLM_PROVIDER)
HICLAW_DEFAULT_MODEL=$($Config.DEFAULT_MODEL)
HICLAW_LLM_API_KEY=$($Config.LLM_API_KEY)
HICLAW_OPENAI_BASE_URL=$($Config.OPENAI_BASE_URL)
HICLAW_MODEL_CONTEXT_WINDOW=$($Config.MODEL_CONTEXT_WINDOW)
HICLAW_MODEL_MAX_TOKENS=$($Config.MODEL_MAX_TOKENS)
HICLAW_MODEL_REASONING=$($Config.MODEL_REASONING)
HICLAW_MODEL_VISION=$($Config.MODEL_VISION)

# Admin
HICLAW_ADMIN_USER=$($Config.ADMIN_USER)
HICLAW_ADMIN_PASSWORD=$($Config.ADMIN_PASSWORD)

# Ports
HICLAW_LOCAL_ONLY=$($Config.LOCAL_ONLY)
HICLAW_PORT_GATEWAY=$($Config.PORT_GATEWAY)
HICLAW_PORT_CONSOLE=$($Config.PORT_CONSOLE)
HICLAW_PORT_ELEMENT_WEB=$($Config.PORT_ELEMENT_WEB)
HICLAW_PORT_OPENCLAW_CONSOLE=$($Config.PORT_OPENCLAW_CONSOLE)

# Matrix
HICLAW_MATRIX_DOMAIN=$($Config.MATRIX_DOMAIN)
HICLAW_MATRIX_CLIENT_DOMAIN=$($Config.MATRIX_CLIENT_DOMAIN)

# Gateway
HICLAW_AI_GATEWAY_DOMAIN=$($Config.AI_GATEWAY_DOMAIN)
HICLAW_MANAGER_GATEWAY_KEY=$($Config.MANAGER_GATEWAY_KEY)

# File System
HICLAW_FS_DOMAIN=$($Config.FS_DOMAIN)
HICLAW_CONSOLE_DOMAIN=$($Config.CONSOLE_DOMAIN)
HICLAW_MINIO_USER=$($Config.MINIO_USER)
HICLAW_MINIO_PASSWORD=$($Config.MINIO_PASSWORD)

# Internal
HICLAW_MANAGER_PASSWORD=$($Config.MANAGER_PASSWORD)
HICLAW_REGISTRATION_TOKEN=$($Config.REGISTRATION_TOKEN)

# GitHub (optional)
HICLAW_GITHUB_TOKEN=$($Config.GITHUB_TOKEN)

# Skills Registry (optional, default: https://skills.sh)
HICLAW_SKILLS_API_URL=$($Config.SKILLS_API_URL)

# Worker images (for direct container creation)
HICLAW_WORKER_IMAGE=$($Config.WORKER_IMAGE)
HICLAW_COPAW_WORKER_IMAGE=$($Config.COPAW_WORKER_IMAGE)

# Default Worker runtime (openclaw | copaw)
HICLAW_DEFAULT_WORKER_RUNTIME=$($Config.DEFAULT_WORKER_RUNTIME)

# Matrix E2EE (0=disabled, 1=enabled; default: 0)
HICLAW_MATRIX_E2EE=$($Config.MATRIX_E2EE)

# Docker API proxy (0=disabled, 1=enabled; default: 1)
HICLAW_DOCKER_PROXY=$($Config.DOCKER_PROXY)

# Docker API proxy: additional allowed image sources (comma-separated)
HICLAW_PROXY_ALLOWED_REGISTRIES=$($Config.PROXY_ALLOWED_REGISTRIES)

# Worker idle timeout in minutes (default: 720 = 12 hours)
HICLAW_WORKER_IDLE_TIMEOUT=$($Config.WORKER_IDLE_TIMEOUT)

# Higress WASM plugin image registry (auto-selected by timezone)
HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=$($Config.REGISTRY)

# Data persistence
HICLAW_DATA_DIR=$($Config.DATA_DIR)
# Manager workspace (skills, memory, state - host-editable)
HICLAW_WORKSPACE_DIR=$($Config.WORKSPACE_DIR)
# Host directory sharing
HICLAW_HOST_SHARE_DIR=$($Config.HOST_SHARE_DIR)
"@

    Set-Content -Path $Path -Value $content -Encoding UTF8
    Write-Log (Get-Msg "install.config_saved" -f $Path)
}

# ============================================================
# Prompt Functions
# ============================================================

function Read-Prompt {
    param(
        [string]$VarName,
        [string]$PromptText,
        [string]$Default = "",
        [switch]$Secret,
        [switch]$Optional
    )

    # Check if already set in environment
    $envValue = [Environment]::GetEnvironmentVariable($VarName)
    if ($envValue) {
        if ($script:HICLAW_UPGRADE -and -not $script:HICLAW_NON_INTERACTIVE) {
            # Show current value (masked for secrets) and let user change it
            $displayValue = $envValue
            if ($Secret) {
                if ($envValue.Length -le 8) {
                    $displayValue = "****"
                } else {
                    $displayValue = $envValue.Substring(0, 4) + "****" + $envValue.Substring($envValue.Length - 4)
                }
            }
            Write-Log (Get-Msg "prompt.upgrade_keep" -f $PromptText, $displayValue)
            $prompt = $PromptText
            if ($Secret) {
                $newValue = Read-Host -Prompt $prompt -AsSecureString
                $newValue = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto(
                    [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($newValue)
                )
            } else {
                $newValue = Read-Host -Prompt $prompt
                if ($newValue -eq "b") { $script:StepResult = "back"; return $null }
            }
            if ($newValue) {
                return $newValue
            }
            return $envValue
        }
        Write-Log (Get-Msg "prompt.preset" -f $PromptText)
        return $envValue
    }
    # Upgrade mode: optional fields with empty value — let user set a new value
    elseif ($Optional -and $script:HICLAW_UPGRADE -and -not $script:HICLAW_NON_INTERACTIVE) {
        Write-Log (Get-Msg "prompt.upgrade_empty" -f $PromptText)
        $prompt = $PromptText
        if ($Secret) {
            $newValue = Read-Host -Prompt $prompt -AsSecureString
            $newValue = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto(
                [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($newValue)
            )
        } else {
            $newValue = Read-Host -Prompt $prompt
            if ($newValue -eq "b") { $script:StepResult = "back"; return $null }
        }
        if ($newValue) {
            return $newValue
        }
        return ""
    }

    # Non-interactive or quickstart mode
    if ($script:HICLAW_NON_INTERACTIVE -or $script:HICLAW_QUICKSTART) {
        if ($Default) {
            Write-Log (Get-Msg "prompt.default" -f $PromptText, $Default)
            return $Default
        }
        elseif ($Optional) {
            return ""
        }
        elseif ($script:HICLAW_NON_INTERACTIVE) {
            # Only hard-error in fully non-interactive mode, not quickstart
            Write-Error (Get-Msg "prompt.required" -f $PromptText)
        }
        # quickstart + no default + not optional: fall through to interactive prompt
    }

    # Interactive prompt
    $prompt = if ($Default) { "$PromptText [$Default]" } else { $PromptText }

    if ($Secret) {
        $value = Read-Host -Prompt $prompt -AsSecureString
        $value = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto(
            [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($value)
        )
    }
    else {
        $value = Read-Host -Prompt $prompt
        if ($value -eq "b") { $script:StepResult = "back"; return $null }
    }

    if (-not $value -and $Default) {
        $value = $Default
    }

    if (-not $value -and -not $Optional) {
        Write-Error (Get-Msg "prompt.required_empty" -f $PromptText)
    }

    return $value
}

# ============================================================
# OpenAI-Compatible Provider
# ============================================================

function Test-LlmConnectivity {
    param(
        [string]$BaseUrl,
        [string]$ApiKey,
        [string]$Model,
        [string]$Hint = ""
    )
    Write-Log (Get-Msg "llm.openai.test.testing")
    $uri = ($BaseUrl.TrimEnd('/')) + "/chat/completions"
    $bodyHash = @{
        model    = $Model
        messages = @(@{ role = "user"; content = "hi" })
    }
    $body = $bodyHash | ConvertTo-Json -Compress
    try {
        $response = Invoke-WebRequest -Uri $uri -Method POST `
            -Headers @{ "Authorization" = "Bearer $ApiKey"; "Content-Type" = "application/json"; "User-Agent" = "HiClaw/$($script:HICLAW_VERSION)" } `
            -Body $body -TimeoutSec 30 -ErrorAction Stop -UseBasicParsing
        Write-Log (Get-Msg "llm.openai.test.ok")
    } catch {
        $statusCode = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
        $responseBody = ""
        if ($_.Exception.Response) {
            try {
                $stream = $_.Exception.Response.GetResponseStream()
                $reader = New-Object System.IO.StreamReader($stream)
                $responseBody = $reader.ReadToEnd()
            } catch {}
        }
        Write-Host (Get-Msg "llm.openai.test.fail" -f $statusCode, $responseBody) -ForegroundColor Yellow
        if ($Hint) {
            Write-Host $Hint -ForegroundColor Yellow
        }
        if (-not $script:HICLAW_NON_INTERACTIVE) {
            $confirm = Read-Host (Get-Msg "llm.openai.test.confirm")
            if ($confirm -eq "b" -or $confirm -eq "B") {
                $script:StepResult = "back"
                return
            }
            if ($confirm -ne "y" -and $confirm -ne "Y") {
                Write-Log (Get-Msg "llm.openai.test.aborted")
                Exit-Script 1
            }
        }
    }
}

function New-OpenAICompatProvider {
    param(
        [string]$BaseUrl,
        [string]$ApiKey,
        [int]$ConsolePort = 18001
    )

    if (-not $BaseUrl -or -not $ApiKey) {
        Write-Log (Get-Msg "install.openai_compat.missing")
        return $false
    }

    $consoleUrl = "http://localhost:$ConsolePort"

    # Parse base URL
    $protocol = "https"
    $port = 443
    $urlWithoutProto = $BaseUrl -replace "^https?://", ""

    if ($BaseUrl -match "^http://") {
        $protocol = "http"
        $port = 80
    }

    $domain = $urlWithoutProto.Split("/")[0]

    if ($domain -match ":(\d+)$") {
        $port = [int]$Matches[1]
        $domain = $domain -replace ":\d+$", ""
    }

    Write-Log (Get-Msg "install.openai_compat.creating")
    Write-Log (Get-Msg "install.openai_compat.domain" -f $domain)
    Write-Log (Get-Msg "install.openai_compat.port" -f $port)
    Write-Log (Get-Msg "install.openai_compat.protocol" -f $protocol)

    $serviceName = "openai-compat"

    # Create DNS service source
    $serviceBody = @{
        type = "dns"
        name = $serviceName
        port = $port.ToString()
        protocol = $protocol
        proxyName = ""
        domain = $domain
    } | ConvertTo-Json -Compress

    try {
        Invoke-RestMethod -Uri "$consoleUrl/v1/service-sources" -Method POST -ContentType "application/json" -Body $serviceBody -ErrorAction SilentlyContinue | Out-Null
    }
    catch {
        Write-Log (Get-Msg "install.openai_compat.service_fail")
    }

    Start-Sleep -Seconds 2

    # Create AI provider
    $providerBody = @{
        type = "openai"
        name = "openai-compat"
        tokens = @($ApiKey)
        version = 0
        protocol = "openai/v1"
        tokenFailoverConfig = @{ enabled = $false }
        rawConfigs = @{
            openaiCustomUrl = $BaseUrl
            openaiCustomServiceName = "$serviceName.dns"
            openaiCustomServicePort = $port
        }
    } | ConvertTo-Json -Compress -Depth 3

    try {
        Invoke-RestMethod -Uri "$consoleUrl/v1/ai/providers" -Method POST -ContentType "application/json" -Body $providerBody -ErrorAction SilentlyContinue | Out-Null
        Write-Log (Get-Msg "install.openai_compat.success")
        return $true
    }
    catch {
        Write-Log (Get-Msg "install.openai_compat.provider_fail")
        return $false
    }
}

# ============================================================
# State Machine Helpers
# ============================================================

function Test-ShouldSkipStep {
    param([string]$StepFn)
    switch ($StepFn) {
        { $_ -in @("Step-Lang", "Step-Mode") } {
            return $script:HICLAW_NON_INTERACTIVE
        }
        "Step-Existing" {
            return (-not (Test-Path $script:HICLAW_ENV_FILE))
        }
        { $_ -in @("Step-Volume", "Step-Workspace") } {
            if ($script:HICLAW_NON_INTERACTIVE) { return $true }
            if ($script:HICLAW_QUICKSTART) { return $true }
            return $false
        }
        { $_ -in @("Step-E2ee", "Step-Idle", "Step-DockerProxy") } {
            if ($script:HICLAW_NON_INTERACTIVE) { return $true }
            if ($script:HICLAW_QUICKSTART -and -not $script:HICLAW_UPGRADE) { return $true }
            return $false
        }
        "Step-Hostshare" {
            if ($script:HICLAW_NON_INTERACTIVE) { return $true }
            if ($script:HICLAW_QUICKSTART) { return $true }
            return $false
        }
        default { return $false }
    }
}

function Clear-StepVars {
    param([string]$StepFn)
    switch ($StepFn) {
        "Step-Mode" { $script:HICLAW_QUICKSTART = $false }
        "Step-Existing" {
            $script:HICLAW_UPGRADE = $false
            $script:UPGRADE_EXISTING_WORKERS = $null
        }
        "Step-Llm" {
            foreach ($k in @("HICLAW_LLM_PROVIDER","HICLAW_DEFAULT_MODEL","HICLAW_OPENAI_BASE_URL",
                             "HICLAW_LLM_API_KEY","HICLAW_MODEL_CONTEXT_WINDOW","HICLAW_MODEL_MAX_TOKENS",
                             "HICLAW_MODEL_REASONING","HICLAW_MODEL_VISION")) {
                [Environment]::SetEnvironmentVariable($k, $null, "Process")
            }
        }
        "Step-Admin" {
            [Environment]::SetEnvironmentVariable("HICLAW_ADMIN_USER", $null, "Process")
            [Environment]::SetEnvironmentVariable("HICLAW_ADMIN_PASSWORD", $null, "Process")
        }
        "Step-Network" {
            [Environment]::SetEnvironmentVariable("HICLAW_LOCAL_ONLY", $null, "Process")
        }
        "Step-Ports" {
            foreach ($k in @("HICLAW_PORT_GATEWAY","HICLAW_PORT_CONSOLE","HICLAW_PORT_ELEMENT_WEB","HICLAW_PORT_OPENCLAW_CONSOLE")) {
                [Environment]::SetEnvironmentVariable($k, $null, "Process")
            }
        }
        "Step-Domains" {
            foreach ($k in @("HICLAW_MATRIX_DOMAIN","HICLAW_MATRIX_CLIENT_DOMAIN","HICLAW_AI_GATEWAY_DOMAIN","HICLAW_FS_DOMAIN","HICLAW_CONSOLE_DOMAIN")) {
                [Environment]::SetEnvironmentVariable($k, $null, "Process")
            }
        }
        "Step-Github" {
            [Environment]::SetEnvironmentVariable("HICLAW_GITHUB_TOKEN", $null, "Process")
        }
        "Step-Skills" {
            [Environment]::SetEnvironmentVariable("HICLAW_SKILLS_API_URL", $null, "Process")
        }
        "Step-Volume" {
            [Environment]::SetEnvironmentVariable("HICLAW_DATA_DIR", $null, "Process")
        }
        "Step-Workspace" {
            [Environment]::SetEnvironmentVariable("HICLAW_WORKSPACE_DIR", $null, "Process")
        }
        "Step-Runtime" {
            [Environment]::SetEnvironmentVariable("HICLAW_DEFAULT_WORKER_RUNTIME", $null, "Process")
        }
        "Step-E2ee" {
            [Environment]::SetEnvironmentVariable("HICLAW_MATRIX_E2EE", $null, "Process")
        }
        "Step-DockerProxy" {
            [Environment]::SetEnvironmentVariable("HICLAW_DOCKER_PROXY", $null, "Process")
            [Environment]::SetEnvironmentVariable("HICLAW_PROXY_ALLOWED_REGISTRIES", $null, "Process")
        }
        "Step-Idle" {
            [Environment]::SetEnvironmentVariable("HICLAW_WORKER_IDLE_TIMEOUT", $null, "Process")
        }
        "Step-Hostshare" {
            [Environment]::SetEnvironmentVariable("HICLAW_HOST_SHARE_DIR", $null, "Process")
        }
    }
}

# ============================================================
# Individual step functions
# ============================================================

function Step-Lang {
    $langDefaultChoice = if ($script:HICLAW_LANGUAGE -eq "zh") { "1" } else { "2" }
    $langDetectedKey = "lang.detected.$($script:HICLAW_LANGUAGE)"
    Write-Log (Get-Msg $langDetectedKey)
    Write-Log (Get-Msg "lang.switch_title")
    Write-Host (Get-Msg "lang.option_zh")
    Write-Host (Get-Msg "lang.option_en")
    Write-Host ""
    $langChoice = Read-Host "$(Get-Msg 'lang.prompt') [$langDefaultChoice]"
    if (-not $langChoice) { $langChoice = $langDefaultChoice }
    if ($langChoice -eq "b") { $script:StepResult = "back"; return }
    switch ($langChoice) {
        "1" { $script:HICLAW_LANGUAGE = "zh" }
        "2" { $script:HICLAW_LANGUAGE = "en" }
    }
    $env:HICLAW_LANGUAGE = $script:HICLAW_LANGUAGE
    Write-Log ""
}

function Step-Mode {
    Write-Log (Get-Msg "install.mode.title")
    Write-Host ""
    Write-Host (Get-Msg "install.mode.choose")
    Write-Host (Get-Msg "install.mode.quickstart")
    Write-Host (Get-Msg "install.mode.manual")
    Write-Host ""
    $choice = Read-Host (Get-Msg "install.mode.prompt")
    $choice = if ($choice) { $choice } else { "1" }
    if ($choice -eq "b") { $script:StepResult = "back"; return }
    switch -Regex ($choice) {
        "^(1|quick|quickstart)$" {
            Write-Log (Get-Msg "install.mode.quickstart_selected")
            $script:HICLAW_QUICKSTART = $true
        }
        "^(2|manual)$" {
            Write-Log (Get-Msg "install.mode.manual_selected")
            $script:HICLAW_QUICKSTART = $false
        }
        default {
            Write-Log (Get-Msg "install.mode.invalid")
            $script:HICLAW_QUICKSTART = $true
        }
    }
    Write-Log ""
}

function Step-Existing {
    # This step is skipped when env file doesn't exist
    Write-Log (Get-Msg "install.existing.detected" -f $script:HICLAW_ENV_FILE)

    $runningManager = docker ps --format "{{.Names}}" 2>$null | Select-String "^hiclaw-manager$"
    $runningWorkers = docker ps --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-"
    $existingWorkers = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-"

    if ($script:HICLAW_NON_INTERACTIVE) {
        Write-Log (Get-Msg "install.existing.upgrade_noninteractive")
        $upgradeChoice = "1"
    } else {
        Write-Host ""
        Write-Host (Get-Msg "install.existing.choose")
        Write-Host (Get-Msg "install.existing.upgrade")
        Write-Host (Get-Msg "install.existing.reinstall")
        Write-Host (Get-Msg "install.existing.cancel")
        Write-Host ""
        $upgradeChoice = Read-Host (Get-Msg "install.existing.prompt")
        $upgradeChoice = if ($upgradeChoice) { $upgradeChoice } else { "1" }
        if ($upgradeChoice -eq "b") { $script:StepResult = "back"; return }
    }

    switch -Regex ($upgradeChoice) {
        "^(1|upgrade)$" {
            $script:HICLAW_UPGRADE = $true
            Write-Log (Get-Msg "install.existing.upgrading")

            if ($runningManager -or $runningWorkers) {
                Write-Host ""
                Write-Host "$($script:ESC)[33m$(Get-Msg 'install.existing.warn_manager_stop')$($script:ESC)[0m"
                if ($existingWorkers) {
                    Write-Host "$($script:ESC)[33m$(Get-Msg 'install.existing.warn_worker_recreate')$($script:ESC)[0m"
                }
                if (-not $script:HICLAW_NON_INTERACTIVE) {
                    $confirm = Read-Host (Get-Msg "install.existing.continue_prompt")
                    if ($confirm -eq "b" -or $confirm -eq "B") { $script:StepResult = "back"; return }
                    if ($confirm -ne "y" -and $confirm -ne "Y") {
                        Write-Log (Get-Msg "install.existing.cancelled")
                        exit 0
                    }
                }
            }
            $script:UPGRADE_EXISTING_WORKERS = $existingWorkers
        }
        "^(2|reinstall)$" {
            Write-Log (Get-Msg "install.reinstall.performing")
            $existingWorkspace = "$env:USERPROFILE\hiclaw-manager"
            if (Test-Path $script:HICLAW_ENV_FILE) {
                $envContent = Get-Content $script:HICLAW_ENV_FILE
                $wsLine = $envContent | Select-String "^HICLAW_WORKSPACE_DIR="
                if ($wsLine) {
                    $existingWorkspace = $wsLine.Line.Substring(21)
                }
            }
            Write-Host ""
            Write-Host "$($script:ESC)[33m$(Get-Msg 'install.reinstall.warn_stop')$($script:ESC)[0m"
            if ($runningManager) { Write-Host "$($script:ESC)[33m   - hiclaw-manager (manager)$($script:ESC)[0m" }
            $runningWorkers | ForEach-Object { Write-Host "$($script:ESC)[33m   - $_ (worker)$($script:ESC)[0m" }
            Write-Host ""
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.warn_delete')$($script:ESC)[0m"
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.warn_volume')$($script:ESC)[0m"
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.warn_env' -f $script:HICLAW_ENV_FILE)$($script:ESC)[0m"
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.warn_workspace' -f $existingWorkspace)$($script:ESC)[0m"
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.warn_workers')$($script:ESC)[0m"
            Write-Host ""
            Write-Host "$($script:ESC)[31m$(Get-Msg 'install.reinstall.confirm_type')$($script:ESC)[0m"
            Write-Host "$($script:ESC)[31m  $existingWorkspace$($script:ESC)[0m"
            Write-Host ""
            $confirmPath = Read-Host (Get-Msg "install.reinstall.confirm_path")
            if ($confirmPath -ne $existingWorkspace) {
                Write-Error (Get-Msg "install.reinstall.path_mismatch" -f $confirmPath, $existingWorkspace)
            }
            Write-Log (Get-Msg "install.reinstall.confirmed")
            docker stop hiclaw-manager *>$null
            docker rm hiclaw-manager *>$null
            docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-" | ForEach-Object {
                docker stop $_ *>$null
                docker rm $_ *>$null
                Write-Log (Get-Msg "install.reinstall.removed_worker" -f $_)
            }
            if (docker volume ls -q 2>$null | Select-String "^hiclaw-data$") {
                Write-Log (Get-Msg "install.reinstall.removing_volume")
                docker volume rm hiclaw-data *>$null
            }
            if (Test-Path $existingWorkspace) {
                Write-Log (Get-Msg "install.reinstall.removing_workspace" -f $existingWorkspace)
                Remove-Item -Recurse -Force $existingWorkspace
            }
            if (Test-Path $script:HICLAW_ENV_FILE) {
                Write-Log (Get-Msg "install.reinstall.removing_env" -f $script:HICLAW_ENV_FILE)
                Remove-Item -Force $script:HICLAW_ENV_FILE
            }
            Write-Log (Get-Msg "install.reinstall.cleanup_done")
        }
        default {
            Write-Log (Get-Msg "install.existing.cancelled")
            exit 0
        }
    }

    # Load existing env file (upgrade path)
    if (Test-Path $script:HICLAW_ENV_FILE) {
        Write-Log (Get-Msg "install.loading_config" -f $script:HICLAW_ENV_FILE)
        Get-Content $script:HICLAW_ENV_FILE | ForEach-Object {
            if ($_ -match "^([^#=][^=]*)=(.*)$") {
                $key = $Matches[1].Trim()
                $value = $Matches[2].Split("#")[0].Trim()
                if (-not [Environment]::GetEnvironmentVariable($key)) {
                    [Environment]::SetEnvironmentVariable($key, $value, "Process")
                }
            }
        }
    }
}

function Step-Llm {
    Write-Log (Get-Msg "llm.title")

    if ($script:HICLAW_NON_INTERACTIVE) {
        $script:config.LLM_PROVIDER = if ($env:HICLAW_LLM_PROVIDER) { $env:HICLAW_LLM_PROVIDER } else { "qwen" }
        $script:config.DEFAULT_MODEL = if ($env:HICLAW_DEFAULT_MODEL) { $env:HICLAW_DEFAULT_MODEL } else { "qwen3.5-plus" }
        $script:config.OPENAI_BASE_URL = if ($env:HICLAW_OPENAI_BASE_URL) { $env:HICLAW_OPENAI_BASE_URL } else { "" }
        Write-Log (Get-Msg "llm.provider.label" -f $script:config.LLM_PROVIDER)
        Write-Log (Get-Msg "llm.model.label" -f $script:config.DEFAULT_MODEL)
        Write-Log ""
        $script:config.LLM_API_KEY = Read-Prompt -VarName "HICLAW_LLM_API_KEY" -PromptText (Get-Msg "llm.apikey_prompt") -Secret
        return
    }

    Write-Host ""
    Write-Host (Get-Msg "llm.providers_title")
    Write-Host (Get-Msg "llm.provider.alibaba")
    Write-Host (Get-Msg "llm.provider.openai_compat")
    Write-Host ""

    if ($script:HICLAW_QUICKSTART) {
        $providerChoice = Read-Host "$(Get-Msg 'llm.provider.select') [1]"
    } else {
        $providerChoice = Read-Host (Get-Msg "llm.provider.select")
    }
    $providerChoice = if ($providerChoice) { $providerChoice } else { "1" }
    if ($providerChoice -eq "b") { $script:StepResult = "back"; return }

    switch -Regex ($providerChoice) {
        "^(1|alibaba-cloud)$" {
            if ($script:HICLAW_LANGUAGE -eq "en") {
                $script:config.LLM_PROVIDER = "openai-compat"
                $script:config.OPENAI_BASE_URL = "https://coding-intl.dashscope.aliyuncs.com/v1"
                $modelChoice = "codingplan"
                Write-Host ""
                Write-Host (Get-Msg "llm.codingplan.models_title")
                Write-Host (Get-Msg "llm.codingplan.model.qwen35plus")
                Write-Host (Get-Msg "llm.codingplan.model.glm5")
                Write-Host (Get-Msg "llm.codingplan.model.kimi")
                Write-Host (Get-Msg "llm.codingplan.model.minimax")
                Write-Host ""
                if ($script:HICLAW_QUICKSTART) {
                    $codingPlanModelChoice = Read-Host "$(Get-Msg 'llm.codingplan.model.select') [1]"
                } else {
                    $codingPlanModelChoice = Read-Host (Get-Msg "llm.codingplan.model.select")
                }
                $codingPlanModelChoice = if ($codingPlanModelChoice) { $codingPlanModelChoice } else { "1" }
                if ($codingPlanModelChoice -eq "b") { $script:StepResult = "back"; return }
                switch -Regex ($codingPlanModelChoice) {
                    "^(1|qwen3\.5-plus)$"  { $script:config.DEFAULT_MODEL = "qwen3.5-plus" }
                    "^(2|glm-5)$"          { $script:config.DEFAULT_MODEL = "glm-5" }
                    "^(3|kimi-k2\.5)$"     { $script:config.DEFAULT_MODEL = "kimi-k2.5" }
                    "^(4|MiniMax-M2\.5)$"  { $script:config.DEFAULT_MODEL = "MiniMax-M2.5" }
                    default                { $script:config.DEFAULT_MODEL = "qwen3.5-plus" }
                }
                Write-Log (Get-Msg "llm.provider.selected_codingplan")
            } else {
                Write-Host ""
                Write-Host (Get-Msg "llm.alibaba.models_title")
                Write-Host (Get-Msg "llm.alibaba.model.codingplan")
                Write-Host (Get-Msg "llm.alibaba.model.qwen")
                Write-Host ""
                if ($script:HICLAW_QUICKSTART) {
                    $modelChoice = Read-Host "$(Get-Msg 'llm.alibaba.model.select') [1]"
                } else {
                    $modelChoice = Read-Host (Get-Msg "llm.alibaba.model.select")
                }
                $modelChoice = if ($modelChoice) { $modelChoice } else { "1" }
                if ($modelChoice -eq "b") { $script:StepResult = "back"; return }

                if ($modelChoice -match "^(2|qwen)$") {
                    $script:config.LLM_PROVIDER = "qwen"
                    Write-Host ""
                    $qwenModelInput = Read-Host (Get-Msg "llm.qwen.model_prompt")
                    if ($qwenModelInput -eq "b") { $script:StepResult = "back"; return }
                    $script:config.DEFAULT_MODEL = if ($qwenModelInput) { $qwenModelInput } elseif ($env:HICLAW_DEFAULT_MODEL) { $env:HICLAW_DEFAULT_MODEL } else { "qwen3.5-plus" }
                    $script:config.OPENAI_BASE_URL = ""
                    Write-Log (Get-Msg "llm.provider.selected_qwen")
                    Request-CustomModelParams $script:config.DEFAULT_MODEL
                    if ($script:StepResult -eq "back") { return }
                } else {
                    $script:config.LLM_PROVIDER = "openai-compat"
                    $script:config.OPENAI_BASE_URL = "https://coding.dashscope.aliyuncs.com/v1"
                    Write-Host ""
                    Write-Host (Get-Msg "llm.codingplan.models_title")
                    Write-Host (Get-Msg "llm.codingplan.model.qwen35plus")
                    Write-Host (Get-Msg "llm.codingplan.model.glm5")
                    Write-Host (Get-Msg "llm.codingplan.model.kimi")
                    Write-Host (Get-Msg "llm.codingplan.model.minimax")
                    Write-Host ""
                    if ($script:HICLAW_QUICKSTART) {
                        $codingPlanModelChoice = Read-Host "$(Get-Msg 'llm.codingplan.model.select') [1]"
                    } else {
                        $codingPlanModelChoice = Read-Host (Get-Msg "llm.codingplan.model.select")
                    }
                    $codingPlanModelChoice = if ($codingPlanModelChoice) { $codingPlanModelChoice } else { "1" }
                    if ($codingPlanModelChoice -eq "b") { $script:StepResult = "back"; return }
                    switch -Regex ($codingPlanModelChoice) {
                        "^(1|qwen3\.5-plus)$"  { $script:config.DEFAULT_MODEL = "qwen3.5-plus" }
                        "^(2|glm-5)$"          { $script:config.DEFAULT_MODEL = "glm-5" }
                        "^(3|kimi-k2\.5)$"     { $script:config.DEFAULT_MODEL = "kimi-k2.5" }
                        "^(4|MiniMax-M2\.5)$"  { $script:config.DEFAULT_MODEL = "MiniMax-M2.5" }
                        default                { $script:config.DEFAULT_MODEL = "qwen3.5-plus" }
                    }
                    Write-Log (Get-Msg "llm.provider.selected_codingplan")
                }
            }

            Write-Log (Get-Msg "llm.model.label" -f $script:config.DEFAULT_MODEL)
            Write-Log ""
            Write-Log (Get-Msg "llm.apikey_hint")
            Write-Log (Get-Msg "llm.apikey_url")
            Write-Log ""
            $script:config.LLM_API_KEY = Read-Prompt -VarName "HICLAW_LLM_API_KEY" -PromptText (Get-Msg "llm.apikey_prompt") -Secret
            if ($script:StepResult -eq "back") { return }
            if ($modelChoice -match "^(2|qwen)$") {
                Test-LlmConnectivity -BaseUrl "https://dashscope.aliyuncs.com/compatible-mode/v1" -ApiKey $script:config.LLM_API_KEY -Model $script:config.DEFAULT_MODEL
            } else {
                Test-LlmConnectivity -BaseUrl $script:config.OPENAI_BASE_URL -ApiKey $script:config.LLM_API_KEY -Model $script:config.DEFAULT_MODEL -Hint (Get-Msg "llm.openai.test.fail.codingplan")
            }
            if ($script:StepResult -eq "back") { return }
        }
        "^(2|openai-compat)$" {
            $script:config.LLM_PROVIDER = "openai-compat"
            Write-Log (Get-Msg "llm.provider.selected_openai" -f $script:config.LLM_PROVIDER)
            Write-Host ""
            $urlInput = Read-Host (Get-Msg "llm.openai.base_url_prompt")
            if ($urlInput -eq "b") { $script:StepResult = "back"; return }
            $script:config.OPENAI_BASE_URL = $urlInput
            $modelInput = Read-Host (Get-Msg "llm.openai.model_prompt")
            if ($modelInput -eq "b") { $script:StepResult = "back"; return }
            $script:config.DEFAULT_MODEL = if ($modelInput) { $modelInput } else { "gpt-5.4" }
            Write-Log (Get-Msg "llm.openai.base_url_label" -f $script:config.OPENAI_BASE_URL)
            Write-Log (Get-Msg "llm.model.label" -f $script:config.DEFAULT_MODEL)
            Request-CustomModelParams $script:config.DEFAULT_MODEL
            if ($script:StepResult -eq "back") { return }
            Write-Log ""
            $script:config.LLM_API_KEY = Read-Prompt -VarName "HICLAW_LLM_API_KEY" -PromptText (Get-Msg "llm.apikey_prompt") -Secret
            if ($script:StepResult -eq "back") { return }
            Test-LlmConnectivity -BaseUrl $script:config.OPENAI_BASE_URL -ApiKey $script:config.LLM_API_KEY -Model $script:config.DEFAULT_MODEL
            if ($script:StepResult -eq "back") { return }
        }
        default {
            Write-Error (Get-Msg "llm.provider.invalid" -f $providerChoice)
        }
    }

    Write-Log ""
}

function Step-Admin {
    Write-Log (Get-Msg "admin.title")
    $script:config.ADMIN_USER = Read-Prompt -VarName "HICLAW_ADMIN_USER" -PromptText (Get-Msg "admin.username_prompt") -Default "admin"
    if ($script:StepResult -eq "back") { return }

    if (-not $env:HICLAW_ADMIN_PASSWORD) {
        $script:config.ADMIN_PASSWORD = Read-Prompt -VarName "HICLAW_ADMIN_PASSWORD" -PromptText (Get-Msg "admin.password_prompt") -Secret -Optional
        if ($script:StepResult -eq "back") { return }
        if (-not $script:config.ADMIN_PASSWORD) {
            $randomSuffix = (New-RandomKey).Substring(0, 12)
            $script:config.ADMIN_PASSWORD = "admin$randomSuffix"
            Write-Log (Get-Msg "admin.password_generated")
        }
    } else {
        $script:config.ADMIN_PASSWORD = $env:HICLAW_ADMIN_PASSWORD
        Write-Log (Get-Msg "prompt.preset" -f (Get-Msg "admin.password_prompt"))
    }

    if ($script:config.ADMIN_PASSWORD.Length -lt 8) {
        Write-Error (Get-Msg "admin.password_too_short" -f $script:config.ADMIN_PASSWORD.Length)
    }

    Write-Log ""
}

function Step-Network {
    Write-Log (Get-Msg "port.local_only.title")
    Write-Host ""
    Write-Host "  1) $(Get-Msg 'port.local_only.hint_yes')"
    Write-Host "  2) $(Get-Msg 'port.local_only.hint_no')"
    Write-Host ""

    if ($script:HICLAW_NON_INTERACTIVE) {
        $localOnly = if ($env:HICLAW_LOCAL_ONLY) { $env:HICLAW_LOCAL_ONLY } else { "1" }
    } elseif ($null -ne $env:HICLAW_LOCAL_ONLY) {
        $localOnly = $env:HICLAW_LOCAL_ONLY
    } else {
        $localChoice = Read-Host "$(Get-Msg 'port.local_only.choice')"
        if ($localChoice -eq "b") { $script:StepResult = "back"; return }
        if (-not $localChoice) { $localChoice = "1" }
        $localOnly = if ($localChoice -match '^(2|n|N|no|NO)$') { "0" } else { "1" }
    }
    $script:config.LOCAL_ONLY = $localOnly

    if ($localOnly -eq "1") {
        Write-Log (Get-Msg "port.local_only.selected_local")
    } else {
        Write-Log (Get-Msg "port.local_only.selected_external")
        Write-Host ""
        Write-Host (Get-Msg "port.local_only.https_hint") -ForegroundColor Yellow
    }
    Write-Log ""
}

function Step-Ports {
    Write-Log (Get-Msg "port.title")
    $script:config.PORT_GATEWAY = Read-Prompt -VarName "HICLAW_PORT_GATEWAY" -PromptText (Get-Msg "port.gateway_prompt") -Default "18080"
    if ($script:StepResult -eq "back") { return }
    $script:config.PORT_CONSOLE = Read-Prompt -VarName "HICLAW_PORT_CONSOLE" -PromptText (Get-Msg "port.console_prompt") -Default "18001"
    if ($script:StepResult -eq "back") { return }
    $script:config.PORT_ELEMENT_WEB = Read-Prompt -VarName "HICLAW_PORT_ELEMENT_WEB" -PromptText (Get-Msg "port.element_prompt") -Default "18088"
    if ($script:StepResult -eq "back") { return }
    $script:config.PORT_OPENCLAW_CONSOLE = Read-Prompt -VarName "HICLAW_PORT_OPENCLAW_CONSOLE" -PromptText (Get-Msg "port.openclaw_console_prompt") -Default "18888"
    if ($script:StepResult -eq "back") { return }
    Write-Log ""
}

function Step-Domains {
    Write-Log (Get-Msg "domain.title")
    Write-Log (Get-Msg "domain.hint")
    $script:config.MATRIX_DOMAIN = Read-Prompt -VarName "HICLAW_MATRIX_DOMAIN" -PromptText (Get-Msg "domain.matrix_prompt") -Default "matrix-local.hiclaw.io:$($script:config.PORT_GATEWAY)"
    if ($script:StepResult -eq "back") { return }
    $script:config.MATRIX_CLIENT_DOMAIN = Read-Prompt -VarName "HICLAW_MATRIX_CLIENT_DOMAIN" -PromptText (Get-Msg "domain.element_prompt") -Default "matrix-client-local.hiclaw.io"
    if ($script:StepResult -eq "back") { return }
    $script:config.AI_GATEWAY_DOMAIN = Read-Prompt -VarName "HICLAW_AI_GATEWAY_DOMAIN" -PromptText (Get-Msg "domain.gateway_prompt") -Default "aigw-local.hiclaw.io"
    if ($script:StepResult -eq "back") { return }
    $script:config.FS_DOMAIN = Read-Prompt -VarName "HICLAW_FS_DOMAIN" -PromptText (Get-Msg "domain.fs_prompt") -Default "fs-local.hiclaw.io"
    if ($script:StepResult -eq "back") { return }
    $script:config.CONSOLE_DOMAIN = Read-Prompt -VarName "HICLAW_CONSOLE_DOMAIN" -PromptText (Get-Msg "domain.console_prompt") -Default "console-local.hiclaw.io"
    if ($script:StepResult -eq "back") { return }
    Write-Log ""
}

function Step-Github {
    Write-Log (Get-Msg "github.title")
    $script:config.GITHUB_TOKEN = Read-Prompt -VarName "HICLAW_GITHUB_TOKEN" -PromptText (Get-Msg "github.token_prompt") -Secret -Optional
}

function Step-Skills {
    Write-Log ""
    Write-Log (Get-Msg "skills.title")
    $script:config.SKILLS_API_URL = Read-Prompt -VarName "HICLAW_SKILLS_API_URL" -PromptText (Get-Msg "skills.url_prompt") -Optional
    if ($script:StepResult -eq "back") { return }
    Write-Log ""
}

function Step-Volume {
    Write-Log (Get-Msg "data.title")
    $dataDirInput = Read-Host (Get-Msg "data.volume_prompt")
    if ($dataDirInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.DATA_DIR = if ($dataDirInput) { $dataDirInput } else { "hiclaw-data" }
    Write-Log (Get-Msg "data.volume_using" -f $script:config.DATA_DIR)
}

function Step-Workspace {
    Write-Log (Get-Msg "workspace.title")
    $defaultWorkspace = "$env:USERPROFILE\hiclaw-manager"
    $wsInput = Read-Host (Get-Msg "workspace.dir_prompt" -f $defaultWorkspace)
    if ($wsInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.WORKSPACE_DIR = if ($wsInput) { $wsInput } else { $defaultWorkspace }
    if (-not (Test-Path $script:config.WORKSPACE_DIR)) {
        New-Item -ItemType Directory -Path $script:config.WORKSPACE_DIR -Force | Out-Null
    }
    Write-Log (Get-Msg "workspace.dir_label" -f $script:config.WORKSPACE_DIR)
}

function Step-Runtime {
    Write-Log (Get-Msg "worker_runtime.title")
    Write-Host ""
    Write-Host "  1) $(Get-Msg 'worker_runtime.openclaw')"
    Write-Host "  2) $(Get-Msg 'worker_runtime.copaw')"
    Write-Host ""

    if ($script:HICLAW_NON_INTERACTIVE) {
        $script:config.DEFAULT_WORKER_RUNTIME = if ($env:HICLAW_DEFAULT_WORKER_RUNTIME) { $env:HICLAW_DEFAULT_WORKER_RUNTIME } else { "openclaw" }
    } elseif ($script:HICLAW_UPGRADE -and $env:HICLAW_DEFAULT_WORKER_RUNTIME) {
        Write-Log (Get-Msg "prompt.upgrade_keep" -f (Get-Msg "worker_runtime.title_short"), $env:HICLAW_DEFAULT_WORKER_RUNTIME)
        $rtChoice = Read-Host (Get-Msg "worker_runtime.choice")
        if ($rtChoice -eq "b") { $script:StepResult = "back"; return }
        if ($rtChoice) {
            $script:config.DEFAULT_WORKER_RUNTIME = if ($rtChoice -eq "2") { "copaw" } else { "openclaw" }
        } else {
            $script:config.DEFAULT_WORKER_RUNTIME = $env:HICLAW_DEFAULT_WORKER_RUNTIME
        }
    } elseif ($env:HICLAW_DEFAULT_WORKER_RUNTIME) {
        $script:config.DEFAULT_WORKER_RUNTIME = $env:HICLAW_DEFAULT_WORKER_RUNTIME
    } else {
        $rtChoice = Read-Host (Get-Msg "worker_runtime.choice")
        if ($rtChoice -eq "b") { $script:StepResult = "back"; return }
        $rtChoice = if ($rtChoice) { $rtChoice } else { "1" }
        $script:config.DEFAULT_WORKER_RUNTIME = if ($rtChoice -eq "2") { "copaw" } else { "openclaw" }
    }
    Write-Log (Get-Msg "worker_runtime.selected" -f $script:config.DEFAULT_WORKER_RUNTIME)
}

function Step-E2ee {
    Write-Host ""
    Write-Log (Get-Msg "matrix_e2ee.title")
    Write-Host ""
    Write-Host "  $(Get-Msg 'matrix_e2ee.desc')"
    Write-Host ""
    Write-Host "  1) $(Get-Msg 'matrix_e2ee.disable')"
    Write-Host "  2) $(Get-Msg 'matrix_e2ee.enable')"
    Write-Host ""

    if ($script:HICLAW_UPGRADE -and $env:HICLAW_MATRIX_E2EE) {
        $e2eeDisplay = if ($env:HICLAW_MATRIX_E2EE -eq "1") { Get-Msg "matrix_e2ee.val_enabled" } else { Get-Msg "matrix_e2ee.val_disabled" }
        Write-Log (Get-Msg "prompt.upgrade_keep" -f (Get-Msg "matrix_e2ee.title_short"), $e2eeDisplay)
        $e2eeChoice = Read-Host (Get-Msg "matrix_e2ee.choice")
        if ($e2eeChoice -eq "b") { $script:StepResult = "back"; return }
        if ($e2eeChoice) {
            $script:config.MATRIX_E2EE = if ($e2eeChoice -eq "2") { "1" } else { "0" }
        } else {
            $script:config.MATRIX_E2EE = $env:HICLAW_MATRIX_E2EE
        }
    } elseif (-not $env:HICLAW_MATRIX_E2EE) {
        $e2eeChoice = Read-Host (Get-Msg "matrix_e2ee.choice")
        if ($e2eeChoice -eq "b") { $script:StepResult = "back"; return }
        $e2eeChoice = if ($e2eeChoice) { $e2eeChoice } else { "1" }
        $script:config.MATRIX_E2EE = if ($e2eeChoice -eq "2") { "1" } else { "0" }
    } else {
        $script:config.MATRIX_E2EE = $env:HICLAW_MATRIX_E2EE
    }

    if ($script:config.MATRIX_E2EE -eq "1") {
        Write-Log (Get-Msg "matrix_e2ee.selected_enabled")
    } else {
        Write-Log (Get-Msg "matrix_e2ee.selected_disabled")
    }
}

function Step-DockerProxy {
    if (-not $script:HICLAW_MOUNT_SOCKET) {
        $script:config.DOCKER_PROXY = "0"
        return
    }

    Write-Host ""
    Write-Log (Get-Msg "docker_proxy.title")
    Write-Host ""
    Write-Host "  $(Get-Msg 'docker_proxy.desc')"
    Write-Host ""
    Write-Host "  1) $(Get-Msg 'docker_proxy.enable')"
    Write-Host "  2) $(Get-Msg 'docker_proxy.disable')"
    Write-Host ""

    if ($script:HICLAW_UPGRADE -and $env:HICLAW_DOCKER_PROXY) {
        $proxyDisplay = if ($env:HICLAW_DOCKER_PROXY -eq "1") { Get-Msg "docker_proxy.val_enabled" } else { Get-Msg "docker_proxy.val_disabled" }
        Write-Log (Get-Msg "prompt.upgrade_keep" -f (Get-Msg "docker_proxy.title_short"), $proxyDisplay)
        $proxyChoice = Read-Host (Get-Msg "docker_proxy.choice")
        if ($proxyChoice -eq "b") { $script:StepResult = "back"; return }
        if ($proxyChoice) {
            $script:config.DOCKER_PROXY = if ($proxyChoice -eq "2") { "0" } else { "1" }
        } else {
            $script:config.DOCKER_PROXY = $env:HICLAW_DOCKER_PROXY
        }
    } elseif (-not $env:HICLAW_DOCKER_PROXY) {
        $proxyChoice = Read-Host (Get-Msg "docker_proxy.choice")
        if ($proxyChoice -eq "b") { $script:StepResult = "back"; return }
        $proxyChoice = if ($proxyChoice) { $proxyChoice } else { "1" }
        $script:config.DOCKER_PROXY = if ($proxyChoice -eq "2") { "0" } else { "1" }
    } else {
        $script:config.DOCKER_PROXY = $env:HICLAW_DOCKER_PROXY
    }

    if ($script:config.DOCKER_PROXY -eq "1") {
        Write-Log (Get-Msg "docker_proxy.selected_enabled")

        # Prompt for additional allowed image sources
        Write-Host ""
        Write-Host "  $(Get-Msg 'docker_proxy.registries_desc')"
        Write-Host ""
        if ($script:HICLAW_UPGRADE -and $env:HICLAW_PROXY_ALLOWED_REGISTRIES) {
            Write-Log (Get-Msg "prompt.upgrade_keep" -f (Get-Msg "docker_proxy.registries_label"), $env:HICLAW_PROXY_ALLOWED_REGISTRIES)
            $regInput = Read-Host (Get-Msg "docker_proxy.registries_prompt")
            if ($regInput -eq "b") { $script:StepResult = "back"; return }
            $script:config.PROXY_ALLOWED_REGISTRIES = if ($regInput) { $regInput } else { $env:HICLAW_PROXY_ALLOWED_REGISTRIES }
        } elseif (-not $env:HICLAW_PROXY_ALLOWED_REGISTRIES) {
            $regInput = Read-Host (Get-Msg "docker_proxy.registries_prompt")
            if ($regInput -eq "b") { $script:StepResult = "back"; return }
            $script:config.PROXY_ALLOWED_REGISTRIES = if ($regInput) { $regInput } else { "" }
        } else {
            $script:config.PROXY_ALLOWED_REGISTRIES = $env:HICLAW_PROXY_ALLOWED_REGISTRIES
        }
    } else {
        Write-Log (Get-Msg "docker_proxy.selected_disabled")
    }
}

function Step-Idle {
    if ($script:HICLAW_UPGRADE -and $env:HICLAW_WORKER_IDLE_TIMEOUT) {
        Write-Log (Get-Msg "prompt.upgrade_keep" -f (Get-Msg "idle_timeout.label"), $env:HICLAW_WORKER_IDLE_TIMEOUT)
        $idleInput = Read-Host (Get-Msg "idle_timeout.prompt")
        if ($idleInput -eq "b") { $script:StepResult = "back"; return }
        $script:config.WORKER_IDLE_TIMEOUT = if ($idleInput) { $idleInput } else { $env:HICLAW_WORKER_IDLE_TIMEOUT }
    } elseif (-not $env:HICLAW_WORKER_IDLE_TIMEOUT) {
        $idleInput = Read-Host (Get-Msg "idle_timeout.prompt")
        if ($idleInput -eq "b") { $script:StepResult = "back"; return }
        $script:config.WORKER_IDLE_TIMEOUT = if ($idleInput) { $idleInput } else { "720" }
    } else {
        $script:config.WORKER_IDLE_TIMEOUT = $env:HICLAW_WORKER_IDLE_TIMEOUT
    }
    Write-Log (Get-Msg "idle_timeout.selected" -f $script:config.WORKER_IDLE_TIMEOUT)
    Write-Log ""
}

function Step-Hostshare {
    $shareInput = Read-Host (Get-Msg "host_share.prompt" -f $env:USERPROFILE)
    if ($shareInput -eq "b") { $script:StepResult = "back"; return }
    $script:config.HOST_SHARE_DIR = if ($shareInput) { $shareInput } else { $env:USERPROFILE }
}

# ============================================================
# Manager Installation
# ============================================================

function Install-Manager {
    Write-Log (Get-Msg "install.title")

    # Detect timezone
    $script:HICLAW_TIMEZONE = Get-HiClawTimeZone

    # Language priority: env var > existing env file > timezone detection
    if ($env:HICLAW_LANGUAGE) {
        $script:HICLAW_LANGUAGE = $env:HICLAW_LANGUAGE
    } else {
        # Check existing env file for saved language preference (upgrade scenario)
        $_envFile = $script:HICLAW_ENV_FILE
        # Migrate from legacy location (current directory) if needed
        if (-not (Test-Path $_envFile) -and (Test-Path ".\hiclaw-manager.env")) {
            Write-Log "Migrating hiclaw-manager.env to $_envFile..."
            Move-Item ".\hiclaw-manager.env" $_envFile -ErrorAction SilentlyContinue
        }
        if (Test-Path $_envFile) {
            $_savedLang = (Get-Content $_envFile | Select-String "^HICLAW_LANGUAGE=" | ForEach-Object {
                $_.Line -replace '^HICLAW_LANGUAGE=', ''
            } | Select-Object -First 1)
            if ($_savedLang) {
                $script:HICLAW_LANGUAGE = $_savedLang
            }
        }
        # Fall back to timezone-based detection
        if (-not $script:HICLAW_LANGUAGE) {
            $script:HICLAW_LANGUAGE = Get-HiClawLanguage -Timezone $script:HICLAW_TIMEZONE
        }
    }
    $env:HICLAW_LANGUAGE = $script:HICLAW_LANGUAGE

    # Detect registry
    $script:HICLAW_REGISTRY = Get-Registry -Timezone $script:HICLAW_TIMEZONE

    # Set image names
    $script:MANAGER_IMAGE = if ($env:HICLAW_INSTALL_MANAGER_IMAGE) {
        $env:HICLAW_INSTALL_MANAGER_IMAGE
    } else {
        "$($script:HICLAW_REGISTRY)/higress/hiclaw-manager:$($script:HICLAW_VERSION)"
    }

    $script:WORKER_IMAGE = if ($env:HICLAW_INSTALL_WORKER_IMAGE) {
        $env:HICLAW_INSTALL_WORKER_IMAGE
    } else {
        "$($script:HICLAW_REGISTRY)/higress/hiclaw-worker:$($script:HICLAW_VERSION)"
    }

    $script:COPAW_WORKER_IMAGE = if ($env:HICLAW_INSTALL_COPAW_WORKER_IMAGE) {
        $env:HICLAW_INSTALL_COPAW_WORKER_IMAGE
    } else {
        "$($script:HICLAW_REGISTRY)/higress/hiclaw-copaw-worker:$($script:HICLAW_VERSION)"
    }

    $script:DOCKER_PROXY_IMAGE = if ($env:HICLAW_INSTALL_DOCKER_PROXY_IMAGE) {
        $env:HICLAW_INSTALL_DOCKER_PROXY_IMAGE
    } else {
        "$($script:HICLAW_REGISTRY)/higress/hiclaw-docker-proxy:$($script:HICLAW_VERSION)"
    }

    Write-Log (Get-Msg "install.registry" -f $script:HICLAW_REGISTRY)
    Write-Log ""
    Write-Log (Get-Msg "install.dir" -f (Get-Location))
    Write-Log (Get-Msg "install.dir_hint")
    Write-Log (Get-Msg "install.dir_hint2")
    Write-Log ""

    # Check container runtime (docker or podman)
    if (-not (Get-Command "docker" -ErrorAction SilentlyContinue) -and
        -not (Get-Command "podman" -ErrorAction SilentlyContinue)) {
        Write-Host "$($script:ESC)[31m[HiClaw ERROR]$($script:ESC)[0m $(Get-Msg 'error.docker_not_found')" -ForegroundColor Red
        Exit-Script 1
    }

    if (-not (Test-DockerRunning)) {
        Write-Host "$($script:ESC)[31m[HiClaw ERROR]$($script:ESC)[0m $(Get-Msg 'error.docker_not_running')" -ForegroundColor Red
        Exit-Script 1
    }

    # Initialize shared config hashtable
    $script:config = @{}
    $config = $script:config  # local alias so post-machine code can use $config

    # Orphan volume detection (env file gone but volume remains)
    if (-not (Test-Path $script:HICLAW_ENV_FILE)) {
        $dataVol = if ($env:HICLAW_DATA_DIR) { $env:HICLAW_DATA_DIR } else { "hiclaw-data" }
        $volumeExists = docker volume ls -q 2>$null | Select-String "^${dataVol}$"
        if ($volumeExists) {
            Write-Host ""
            Write-Log (Get-Msg "install.orphan_volume.detected" -f $dataVol)
            Write-Log (Get-Msg "install.orphan_volume.warn")
            if ($script:HICLAW_NON_INTERACTIVE) {
                Write-Log (Get-Msg "install.orphan_volume.clean_noninteractive")
                docker stop hiclaw-manager *>$null
                docker rm hiclaw-manager *>$null
                docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-" | ForEach-Object {
                    docker stop $_.ToString().Trim() *>$null
                    docker rm $_.ToString().Trim() *>$null
                }
                Write-Log (Get-Msg "install.orphan_volume.cleaning")
                docker volume rm $dataVol *>$null
                Write-Log (Get-Msg "install.orphan_volume.cleaned")
            } else {
                Write-Host ""
                Write-Host (Get-Msg "install.orphan_volume.choose")
                Write-Host (Get-Msg "install.orphan_volume.clean")
                Write-Host (Get-Msg "install.orphan_volume.keep")
                Write-Host ""
                $orphanChoice = Read-Host (Get-Msg "install.orphan_volume.prompt")
                if (-not $orphanChoice) { $orphanChoice = "1" }
                switch -Regex ($orphanChoice) {
                    "^(1|clean)$" {
                        docker stop hiclaw-manager *>$null
                        docker rm hiclaw-manager *>$null
                        docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-" | ForEach-Object {
                            docker stop $_.ToString().Trim() *>$null
                            docker rm $_.ToString().Trim() *>$null
                        }
                        Write-Log (Get-Msg "install.orphan_volume.cleaning")
                        docker volume rm $dataVol *>$null
                        Write-Log (Get-Msg "install.orphan_volume.cleaned")
                    }
                    "^(2|keep)$" {
                        Write-Log (Get-Msg "install.orphan_volume.keeping")
                    }
                }
            }
        }
    }

    # ── State machine ─────────────────────────────────────────────────────────
    $_steps = @("Step-Lang", "Step-Mode", "Step-Existing", "Step-Llm", "Step-Admin",
                "Step-Network", "Step-Ports", "Step-Domains", "Step-Github", "Step-Skills",
                "Step-Volume", "Step-Workspace", "Step-Runtime", "Step-E2ee", "Step-DockerProxy", "Step-Idle",
                "Step-Hostshare")
    $_stepHistory = [System.Collections.Generic.List[int]]::new()
    $_stepIdx = 0
    while ($_stepIdx -lt $_steps.Count) {
        $_stepFn = $_steps[$_stepIdx]
        if (Test-ShouldSkipStep $_stepFn) {
            $_stepIdx++
            continue
        }
        if ($_stepHistory.Count -gt 0) {
            Write-Log (Get-Msg "nav.back_hint")
        }
        $script:StepResult = ""
        & $_stepFn
        if ($script:StepResult -eq "back") {
            if ($_stepHistory.Count -gt 0) {
                $_stepIdx = $_stepHistory[$_stepHistory.Count - 1]
                $_stepHistory.RemoveAt($_stepHistory.Count - 1)
                Clear-StepVars $_steps[$_stepIdx]
            }
            # else: first step, ignore 'b'
        } else {
            $_stepHistory.Add($_stepIdx)
            $_stepIdx++
        }
    }
    # ── End state machine ──────────────────────────────────────────────────────

    # Post-machine defaults for any steps that were skipped
    if (-not $script:config.DATA_DIR) {
        $script:config.DATA_DIR = if ($env:HICLAW_DATA_DIR) { $env:HICLAW_DATA_DIR } else { "hiclaw-data" }
    }
    Write-Log (Get-Msg "data.volume_using" -f $script:config.DATA_DIR)
    if (-not $script:config.WORKSPACE_DIR) {
        $_defaultWs = "$env:USERPROFILE\hiclaw-manager"
        $script:config.WORKSPACE_DIR = if ($env:HICLAW_WORKSPACE_DIR) { $env:HICLAW_WORKSPACE_DIR } else { $_defaultWs }
        if (-not (Test-Path $script:config.WORKSPACE_DIR)) {
            New-Item -ItemType Directory -Path $script:config.WORKSPACE_DIR -Force | Out-Null
        }
        Write-Log (Get-Msg "workspace.dir_label" -f $script:config.WORKSPACE_DIR)
    }
    if (-not $script:config.DEFAULT_WORKER_RUNTIME) {
        $script:config.DEFAULT_WORKER_RUNTIME = if ($env:HICLAW_DEFAULT_WORKER_RUNTIME) { $env:HICLAW_DEFAULT_WORKER_RUNTIME } else { "openclaw" }
        Write-Log (Get-Msg "worker_runtime.selected" -f $script:config.DEFAULT_WORKER_RUNTIME)
    }
    if (-not $script:config.MATRIX_E2EE) {
        $script:config.MATRIX_E2EE = if ($env:HICLAW_MATRIX_E2EE) { $env:HICLAW_MATRIX_E2EE } else { "0" }
    }
    if (-not $script:config.WORKER_IDLE_TIMEOUT) {
        $script:config.WORKER_IDLE_TIMEOUT = if ($env:HICLAW_WORKER_IDLE_TIMEOUT) { $env:HICLAW_WORKER_IDLE_TIMEOUT } else { "720" }
    }
    if (-not $script:config.HOST_SHARE_DIR) {
        $script:config.HOST_SHARE_DIR = if ($env:HICLAW_HOST_SHARE_DIR) { $env:HICLAW_HOST_SHARE_DIR } else { $env:USERPROFILE }
    }
    if (-not $script:config.LOCAL_ONLY) {
        $script:config.LOCAL_ONLY = if ($env:HICLAW_LOCAL_ONLY) { $env:HICLAW_LOCAL_ONLY } else { "1" }
    }
    if (-not $script:config.PORT_GATEWAY) {
        $script:config.PORT_GATEWAY = if ($env:HICLAW_PORT_GATEWAY) { $env:HICLAW_PORT_GATEWAY } else { "18080" }
        $script:config.PORT_CONSOLE = if ($env:HICLAW_PORT_CONSOLE) { $env:HICLAW_PORT_CONSOLE } else { "18001" }
        $script:config.PORT_ELEMENT_WEB = if ($env:HICLAW_PORT_ELEMENT_WEB) { $env:HICLAW_PORT_ELEMENT_WEB } else { "18088" }
        $script:config.PORT_OPENCLAW_CONSOLE = if ($env:HICLAW_PORT_OPENCLAW_CONSOLE) { $env:HICLAW_PORT_OPENCLAW_CONSOLE } else { "18888" }
    }
    if (-not $script:config.MATRIX_DOMAIN) {
        $script:config.MATRIX_DOMAIN = if ($env:HICLAW_MATRIX_DOMAIN) { $env:HICLAW_MATRIX_DOMAIN } else { "matrix-local.hiclaw.io:$($script:config.PORT_GATEWAY)" }
        $script:config.MATRIX_CLIENT_DOMAIN = if ($env:HICLAW_MATRIX_CLIENT_DOMAIN) { $env:HICLAW_MATRIX_CLIENT_DOMAIN } else { "matrix-client-local.hiclaw.io" }
        $script:config.AI_GATEWAY_DOMAIN = if ($env:HICLAW_AI_GATEWAY_DOMAIN) { $env:HICLAW_AI_GATEWAY_DOMAIN } else { "aigw-local.hiclaw.io" }
        $script:config.FS_DOMAIN = if ($env:HICLAW_FS_DOMAIN) { $env:HICLAW_FS_DOMAIN } else { "fs-local.hiclaw.io" }
        $script:config.CONSOLE_DOMAIN = if ($env:HICLAW_CONSOLE_DOMAIN) { $env:HICLAW_CONSOLE_DOMAIN } else { "console-local.hiclaw.io" }
    }

    Write-Log ""

    # Generate secrets
    Write-Log (Get-Msg "install.generating_secrets")
    $config.MANAGER_PASSWORD = if ($env:HICLAW_MANAGER_PASSWORD) { $env:HICLAW_MANAGER_PASSWORD } else { New-RandomKey }
    $config.REGISTRATION_TOKEN = if ($env:HICLAW_REGISTRATION_TOKEN) { $env:HICLAW_REGISTRATION_TOKEN } else { New-RandomKey }
    $config.MINIO_USER = if ($env:HICLAW_MINIO_USER) { $env:HICLAW_MINIO_USER } else { $config.ADMIN_USER }
    $config.MINIO_PASSWORD = if ($env:HICLAW_MINIO_PASSWORD) { $env:HICLAW_MINIO_PASSWORD } else { $config.ADMIN_PASSWORD }
    $config.MANAGER_GATEWAY_KEY = if ($env:HICLAW_MANAGER_GATEWAY_KEY) { $env:HICLAW_MANAGER_GATEWAY_KEY } else { New-RandomKey }

    # Store additional config
    $config.LANGUAGE = $script:HICLAW_LANGUAGE
    $config.REGISTRY = $script:HICLAW_REGISTRY
    $config.WORKER_IMAGE = $script:WORKER_IMAGE
    $config.COPAW_WORKER_IMAGE = $script:COPAW_WORKER_IMAGE

    # Write env file
    New-EnvFile -Config $config -Path $script:HICLAW_ENV_FILE

    # Build Docker arguments
    $dockerArgs = @(
        "run", "-d",
        "--name", "hiclaw-manager",
        "--env-file", $script:HICLAW_ENV_FILE,
        "-e", "HOME=/root/manager-workspace",
        "-w", "/root/manager-workspace",
        "-e", "HOST_ORIGINAL_HOME=$($config.HOST_SHARE_DIR)"
    )

    # Timezone
    $dockerArgs += @("-e", "TZ=$($script:HICLAW_TIMEZONE)")

    # Docker socket mount (Windows uses named pipe)
    # On Windows, we test socket availability by running docker commands
    $socketMounted = $false
    if ($script:HICLAW_MOUNT_SOCKET) {
        $socketAvailable = Test-DockerRunning
        if ($socketAvailable) {
            # Ensure hiclaw-net Docker network exists; Manager joins it with network aliases
            # so workers can resolve *-local.hiclaw.io via Docker DNS without ExtraHosts.
            docker network inspect hiclaw-net *>$null
            if ($LASTEXITCODE -ne 0) { docker network create hiclaw-net *>$null }
            $dockerArgs += @("--network", "hiclaw-net")
            # Workers hardcode these three internal domains to reach manager services,
            # so they must always be network aliases regardless of user domain config.
            $dockerArgs += @("--network-alias", "matrix-local.hiclaw.io")
            $dockerArgs += @("--network-alias", "aigw-local.hiclaw.io")
            $dockerArgs += @("--network-alias", "fs-local.hiclaw.io")
            # Also alias any *-local.hiclaw.io user-configured domains that differ from the fixed ones above.
            foreach ($domain in @($config.MATRIX_CLIENT_DOMAIN, $config.CONSOLE_DOMAIN)) {
                if ($domain -match '-local\.hiclaw\.io$') {
                    $dockerArgs += @("--network-alias", $domain)
                }
            }

            # Start Docker API proxy if enabled
            if ($config.DOCKER_PROXY -eq "1") {
                $proxyImage = $script:DOCKER_PROXY_IMAGE
                Write-Log "Starting Docker API proxy..."
                docker rm -f hiclaw-docker-proxy *>$null
                docker run -d --name hiclaw-docker-proxy `
                    --network hiclaw-net `
                    -v "//var/run/docker.sock:/var/run/docker.sock" `
                    --security-opt label=disable `
                    $(if ($config.PROXY_ALLOWED_REGISTRIES) { @("-e", "HICLAW_PROXY_ALLOWED_REGISTRIES=$($config.PROXY_ALLOWED_REGISTRIES)") }) `
                    --restart unless-stopped `
                    $proxyImage
                $dockerArgs += @("-e", "HICLAW_CONTAINER_API=http://hiclaw-docker-proxy:2375")
                Write-Log (Get-Msg "docker_proxy.selected_enabled")
            } else {
                $dockerArgs += @("-v", "//var/run/docker.sock:/var/run/docker.sock")
                Write-Log (Get-Msg "install.socket_detected" -f "//var/run/docker.sock")
            }
        } else {
            Write-Log (Get-Msg "install.socket_not_found")
            # Interactive confirmation when socket not found
            if (-not $script:HICLAW_NON_INTERACTIVE) {
                Write-Host ""
                Write-Host "$($script:ESC)[33m$(Get-Msg 'install.socket_confirm.title')$($script:ESC)[0m"
                Write-Host ""
                Write-Host (Get-Msg 'install.socket_confirm.message')
                Write-Host ""
                $confirm = Read-Host (Get-Msg 'install.socket_confirm.prompt')
                if ($confirm -ne 'y' -and $confirm -ne 'Y') {
                    Write-Log (Get-Msg 'install.socket_confirm.cancelled')
                    exit 0
                }
            }
        }
    }

    # Port mappings
    $portPrefix = if ($config.LOCAL_ONLY -eq "1") { "127.0.0.1:" } else { "" }
    $dockerArgs += @("-p", "${portPrefix}$($config.PORT_GATEWAY):8080")
    $dockerArgs += @("-p", "${portPrefix}$($config.PORT_CONSOLE):8001")
    $dockerArgs += @("-p", "${portPrefix}$($config.PORT_ELEMENT_WEB):8088")
    $dockerArgs += @("-p", "127.0.0.1:$($config.PORT_OPENCLAW_CONSOLE):18888")

    # Data mount: Docker volume
    $dockerArgs += @("-v", "$($config.DATA_DIR):/data")

    # Workspace mount
    $wsDockerPath = ConvertTo-DockerPath -Path $config.WORKSPACE_DIR
    $dockerArgs += @("-v", "${wsDockerPath}:/root/manager-workspace")

    # Host share mount
    $shareDockerPath = ConvertTo-DockerPath -Path $config.HOST_SHARE_DIR
    $dockerArgs += @("-v", "${shareDockerPath}:/host-share")
    Write-Log (Get-Msg "host_share.sharing" -f $config.HOST_SHARE_DIR)

    # YOLO mode
    if ($env:HICLAW_YOLO -eq "1") {
        $dockerArgs += @("-e", "HICLAW_YOLO=1")
        Write-Log (Get-Msg "install.yolo")
    }

    # Restart policy
    $dockerArgs += @("--restart", "unless-stopped")

    # Image
    $dockerArgs += $script:MANAGER_IMAGE

    # Check if the Docker volume exists; create if not (reuse on reinstall)
    $volumeExists = docker volume ls -q 2>$null | Select-String "^$($config.DATA_DIR)$"
    if (-not $volumeExists) {
        docker volume create $config.DATA_DIR | Out-Null
    }

    # Pull images (skip if already exists locally)
    # For local images (prefix "hiclaw/"), skip pull if exists
    # For remote images, always pull to get updates
    $LocalImagePrefix = "hiclaw/"
    if ($script:MANAGER_IMAGE.StartsWith($LocalImagePrefix)) {
        $managerImageExists = docker image inspect $script:MANAGER_IMAGE 2>$null
        if ($LASTEXITCODE -eq 0) {
            Write-Log (Get-Msg "install.image.exists" -f $script:MANAGER_IMAGE)
        } else {
            Write-Log (Get-Msg "install.image.pulling_manager" -f $script:MANAGER_IMAGE)
            & docker pull $script:MANAGER_IMAGE
        }
    } else {
        Write-Log (Get-Msg "install.image.pulling_manager" -f $script:MANAGER_IMAGE)
        & docker pull $script:MANAGER_IMAGE
    }

    # Pull the worker image matching the selected runtime (on upgrade, also pull the other if present locally)
    $selectedWorkerImage = if ($config.DEFAULT_WORKER_RUNTIME -eq "copaw") { $script:COPAW_WORKER_IMAGE } else { $script:WORKER_IMAGE }
    if ($selectedWorkerImage.StartsWith($LocalImagePrefix)) {
        $workerImageExists = docker image inspect $selectedWorkerImage 2>$null
        if ($LASTEXITCODE -eq 0) {
            Write-Log (Get-Msg "install.image.worker_exists" -f $selectedWorkerImage)
        } else {
            Write-Log (Get-Msg "install.image.pulling_worker" -f $selectedWorkerImage)
            & docker pull $selectedWorkerImage
        }
    } else {
        Write-Log (Get-Msg "install.image.pulling_worker" -f $selectedWorkerImage)
        & docker pull $selectedWorkerImage
    }

    # During upgrade, also pull the other worker image if it exists locally
    if ($script:HICLAW_UPGRADE) {
        $otherWorkerImage = if ($config.DEFAULT_WORKER_RUNTIME -eq "copaw") { $script:WORKER_IMAGE } else { $script:COPAW_WORKER_IMAGE }
        $otherExists = docker image inspect $otherWorkerImage 2>$null
        if ($LASTEXITCODE -eq 0) {
            if ($otherWorkerImage.StartsWith($LocalImagePrefix)) {
                Write-Log (Get-Msg "install.image.worker_exists" -f $otherWorkerImage)
            } else {
                Write-Log (Get-Msg "install.image.pulling_worker" -f $otherWorkerImage)
                & docker pull $otherWorkerImage
            }
        }
    }

    # Stop and remove existing containers (deferred until after all
    # configuration is collected and images are pulled successfully)
    $existingProxy = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-docker-proxy$"
    if ($existingProxy) {
        docker stop hiclaw-docker-proxy *>$null
        docker rm hiclaw-docker-proxy *>$null
    }
    $existingContainer = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-manager$"
    if ($existingContainer) {
        Write-Log (Get-Msg "install.removing_existing")
        docker stop hiclaw-manager *>$null
        docker rm hiclaw-manager *>$null
    }

    # Stop and remove worker containers saved during upgrade detection
    # (Manager IP changes on restart, so workers must be recreated)
    if ($script:UPGRADE_EXISTING_WORKERS) {
        Write-Log (Get-Msg "install.existing.stopping_workers")
        $script:UPGRADE_EXISTING_WORKERS | ForEach-Object {
            docker stop $_ *>$null
            docker rm $_ *>$null
            Write-Log (Get-Msg "install.existing.removed" -f $_)
        }
    }

    # Run container
    Write-Log (Get-Msg "install.starting_manager")
    & docker $dockerArgs

    # Wait for ready
    Wait-ManagerReady -Container "hiclaw-manager"

    # Wait for Matrix server to be ready
    Wait-MatrixReady -Container "hiclaw-manager"

    # Create OpenAI-compatible provider if needed
    if ($config.LLM_PROVIDER -eq "openai-compat") {
        New-OpenAICompatProvider -BaseUrl $config.OPENAI_BASE_URL -ApiKey $config.LLM_API_KEY -ConsolePort ([int]$config.PORT_CONSOLE)
    }

    # Print success message
    Write-Log ""
    Write-Log (Get-Msg "success.title")
    Write-Log ""
    Write-Log (Get-Msg "success.domains_configured")
    Write-Log "  $($config.MATRIX_DOMAIN.Split(':')[0]) $($config.MATRIX_CLIENT_DOMAIN) $($config.AI_GATEWAY_DOMAIN) $($config.FS_DOMAIN) $($config.CONSOLE_DOMAIN)"
    Write-Log ""

    $lanIP = Get-LanIP

    Write-Host "$($script:ESC)[33m===============================================================$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.open_url')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    Write-Host "$($script:ESC)[1;36m    http://127.0.0.1:$($config.PORT_ELEMENT_WEB)/#/login$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.login_with')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.username' -f $config.ADMIN_USER)$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.password' -f $config.ADMIN_PASSWORD)$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.after_login')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.tell_it')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.manager_auto')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  ---------------------------------------------------------------  $($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_title')$($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    if ($lanIP) {
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step1')$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step2' -f "http://${lanIP}:$($config.PORT_GATEWAY)")$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step3')$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_username' -f $config.ADMIN_USER)$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_password' -f $config.ADMIN_PASSWORD)$($script:ESC)[0m"
    } else {
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step1')$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step2_noip' -f $config.PORT_GATEWAY)$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_noip_hint')$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_step3')$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_username' -f $config.ADMIN_USER)$($script:ESC)[0m"
        Write-Host "$($script:ESC)[33m  $(Get-Msg 'success.mobile_password' -f $config.ADMIN_PASSWORD)$($script:ESC)[0m"
    }
    Write-Host "$($script:ESC)[33m                                                                                 $($script:ESC)[0m"
    Write-Host "$($script:ESC)[33m===============================================================$($script:ESC)[0m"

    Write-Log ""
    Write-Log (Get-Msg "success.other_consoles")
    Write-Log (Get-Msg "success.higress_console" -f $config.PORT_CONSOLE, $config.ADMIN_USER, $config.ADMIN_PASSWORD)
    Write-Log (Get-Msg "success.openclaw_console" -f $config.PORT_OPENCLAW_CONSOLE)
    Write-Log (Get-Msg "success.openclaw_console_gateway" -f $config.ADMIN_USER, $config.ADMIN_PASSWORD)
    Write-Log ""
    Write-Log (Get-Msg "success.switch_llm.title")
    Write-Log (Get-Msg "success.switch_llm.hint")
    Write-Log (Get-Msg "success.switch_llm.docs")
    Write-Log (Get-Msg "success.switch_llm.url")
    Write-Log ""
    Write-Log (Get-Msg "success.tip")
    Write-Log ""
    if ($config.LOCAL_ONLY -ne "1") {
        Write-Host (Get-Msg "port.local_only.https_hint") -ForegroundColor Yellow
        Write-Log ""
    }
    Write-Log (Get-Msg "success.config_file" -f $script:HICLAW_ENV_FILE)

    Write-Log (Get-Msg "success.data_volume" -f $config.DATA_DIR)

    Write-Log (Get-Msg "success.workspace" -f $config.WORKSPACE_DIR)
}

# ============================================================
# Worker Installation
# ============================================================

function Install-Worker {
    param(
        [string]$Name,
        [string]$Fs,
        [string]$FsKey,
        [string]$FsSecret,
        [switch]$Reset,
        [switch]$FindSkills,
        [string]$SkillsApiUrl
    )

    # Validate required parameters
    if (-not $Name) {
        Write-Error (Get-Msg "error.name_required")
    }
    if (-not $Fs) {
        Write-Error (Get-Msg "error.fs_required")
    }
    if (-not $FsKey) {
        Write-Error (Get-Msg "error.fs_key_required")
    }
    if (-not $FsSecret) {
        Write-Error (Get-Msg "error.fs_secret_required")
    }

    $containerName = "hiclaw-worker-$Name"

    # Handle reset
    if ($Reset) {
        Write-Log (Get-Msg "worker.resetting" -f $Name)
        docker stop $containerName *>$null
        docker rm $containerName *>$null
    }

    # Check for existing container
    $existing = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^$containerName$"
    if ($existing) {
        Write-Error (Get-Msg "worker.exists" -f $containerName)
    }

    # Detect timezone and registry
    $timezone = Get-HiClawTimeZone
    $registry = Get-Registry -Timezone $timezone
    $workerImage = if ($env:HICLAW_INSTALL_WORKER_IMAGE) {
        $env:HICLAW_INSTALL_WORKER_IMAGE
    } else {
        "$registry/higress/hiclaw-worker:$($script:HICLAW_VERSION)"
    }

    Write-Log (Get-Msg "worker.starting" -f $Name)

    $dockerArgs = @(
        "run", "-d",
        "--name", $containerName,
        "-e", "HOME=/root/hiclaw-fs/agents/$Name",
        "-w", "/root/hiclaw-fs/agents/$Name",
        "-e", "HICLAW_WORKER_NAME=$Name",
        "-e", "HICLAW_FS_ENDPOINT=$Fs",
        "-e", "HICLAW_FS_ACCESS_KEY=$FsKey",
        "-e", "HICLAW_FS_SECRET_KEY=$FsSecret"
    )

    # Add SKILLS_API_URL if find-skills is enabled and URL is specified
    if ($FindSkills -and $SkillsApiUrl) {
        $dockerArgs += @("-e", "SKILLS_API_URL=$SkillsApiUrl")
        Write-Log (Get-Msg "worker.skills_url" -f $SkillsApiUrl)
    }

    $dockerArgs += @("--restart", "unless-stopped", $workerImage)

    & docker $dockerArgs

    Write-Log ""
    Write-Log (Get-Msg "worker.started" -f $Name)
    Write-Log (Get-Msg "worker.container" -f $containerName)
    Write-Log (Get-Msg "worker.view_logs" -f $containerName)
}

# ============================================================
# Uninstall
# ============================================================

function Uninstall-HiClaw {
    Write-Log (Get-Msg "uninstall.title")

    # Stop and remove manager
    $manager = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-manager$"
    if ($manager) {
        Write-Log (Get-Msg "uninstall.stopping_manager")
        docker stop hiclaw-manager *>$null
        docker rm hiclaw-manager *>$null
    }

    # Stop and remove workers
    $workers = docker ps -a --format "{{.Names}}" 2>$null | Select-String "^hiclaw-worker-"
    if ($workers) {
        Write-Log (Get-Msg "uninstall.stopping_workers")
        $workers | ForEach-Object {
            docker stop $_ *>$null
            docker rm $_ *>$null
            Write-Log (Get-Msg "uninstall.removed" -f $_)
        }
    }

    # Remove Docker volume
    $volume = docker volume ls -q 2>$null | Select-String "^hiclaw-data$"
    if ($volume) {
        Write-Log (Get-Msg "uninstall.removing_volume")
        docker volume rm hiclaw-data *>$null
    }

    # Remove env file
    if (Test-Path $script:HICLAW_ENV_FILE) {
        Write-Log (Get-Msg "uninstall.removing_env" -f $script:HICLAW_ENV_FILE)
        Remove-Item -Force $script:HICLAW_ENV_FILE
    }

    Write-Log ""
    Write-Log (Get-Msg "uninstall.done")
    Write-Log (Get-Msg "uninstall.workspace_note")
}

# ============================================================
# Main Entry Point
# ============================================================

try {
    switch ($Command) {
        "manager" {
            Install-Manager
        }
        "worker" {
            Install-Worker -Name $Name -Fs $Fs -FsKey $FsKey -FsSecret $FsSecret -Reset:$Reset -FindSkills:$FindSkills -SkillsApiUrl $SkillsApiUrl
        }
        "uninstall" {
            Uninstall-HiClaw
        }
    }
} catch {
    Exit-Script 1
}

# Stop transcript logging
try {
    Stop-Transcript -ErrorAction SilentlyContinue | Out-Null
} catch {
    # Ignore errors when stopping transcript
}
