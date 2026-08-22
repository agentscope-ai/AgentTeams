#!/bin/bash
# hiclaw-install.sh - One-click installation for HiClaw Manager and Worker
#
# Usage:
#   ./hiclaw-install.sh                  # Interactive installation (choose Quick Start or Manual)
#   ./hiclaw-install.sh manager          # Same as above (explicit)
#   ./hiclaw-install.sh worker --name <name> ...  # Worker installation
#
# Onboarding Modes:
#   Quick Start  - Fast installation with all default values (recommended)
#   Manual       - Customize each option step by step
#
# Environment variables (for automation):
#   HICLAW_NON_INTERACTIVE    Skip all prompts, use defaults  (default: 0)
#   HICLAW_LLM_PROVIDER      LLM provider       (default: alibaba-cloud)
#   HICLAW_DEFAULT_MODEL      Default model       (default: qwen3.5-plus)
#   HICLAW_LLM_API_KEY        LLM API key         (required)
#   HICLAW_ADMIN_USER         Admin username       (default: admin)
#   HICLAW_ADMIN_PASSWORD     Admin password       (auto-generated if not set, min 8 chars)
#   HICLAW_MATRIX_DOMAIN      Matrix domain        (default: matrix-local.hiclaw.io:18080)
#   HICLAW_MOUNT_SOCKET       Mount container runtime socket (default: 1)
#   HICLAW_DATA_DIR           Docker volume name for persistent data (default: hiclaw-data)
#   HICLAW_WORKSPACE_DIR      Host directory for manager workspace (default: ~/hiclaw-manager)
#   HICLAW_VERSION            Image tag            (default: latest)
#   HICLAW_REGISTRY           Image registry       (default: auto-detected by timezone)
#   HICLAW_INSTALL_MANAGER_IMAGE       Override manager image (e.g., local build)
#   HICLAW_INSTALL_WORKER_IMAGE        Override worker image  (e.g., local build)
#   HICLAW_INSTALL_COPAW_WORKER_IMAGE  Override copaw worker image (e.g., local build)
#   HICLAW_NACOS_USERNAME              Default Nacos username for nacos:// package imports (optional)
#   HICLAW_NACOS_PASSWORD              Default Nacos password for nacos:// package imports (optional)
#   HICLAW_CMS_TRACES_ENABLED          Enable openclaw-cms-plugin traces for Manager AND all Workers (default: false)
#   HICLAW_CMS_ENDPOINT                ARMS OTLP endpoint (required if traces enabled)
#   HICLAW_CMS_LICENSE_KEY             CMS license key (required if traces enabled)
#   HICLAW_CMS_PROJECT                 CMS project name (optional)
#   HICLAW_CMS_WORKSPACE               CMS workspace ID (required if traces enabled)
#   HICLAW_CMS_SERVICE_NAME            Manager service name in ARMS (default: hiclaw-manager)
#                                      Workers always report as hiclaw-worker-<name> automatically
#   HICLAW_CMS_METRICS_ENABLED         Enable diagnostics-otel metrics for Manager AND all Workers (default: false)
#   HICLAW_PORT_GATEWAY       Host port for Higress gateway (default: 18080)
#   HICLAW_PORT_CONSOLE       Host port for Higress console (default: 18001)
#   HICLAW_PORT_ELEMENT_WEB   Host port for Element Web direct access (default: 18088)
#   HICLAW_WORKER_IDLE_TIMEOUT  Worker idle timeout in minutes (default: 720, i.e. 12 hours)

set -e

HICLAW_VERSION="${HICLAW_VERSION:-}"
HICLAW_KNOWN_STABLE_VERSION="v1.0.8"   # fallback if GitHub API is unreachable
HICLAW_NON_INTERACTIVE="${HICLAW_NON_INTERACTIVE:-0}"
HICLAW_MOUNT_SOCKET="${HICLAW_MOUNT_SOCKET:-1}"
HICLAW_DOCKER_PROXY="${HICLAW_DOCKER_PROXY:-1}"
STEP_RESULT=""  # Used by state machine to signal "back" navigation

# ============================================================
# Log all output to file
# ============================================================

HICLAW_LOG_FILE="${HOME}/hiclaw-install.log"

# Redirect all output (stdout and stderr) to both terminal and log file
exec > >(tee -a "${HICLAW_LOG_FILE}") 2>&1

echo ""
echo "========================================"
echo "HiClaw Installation Log"
echo "Started: $(date)"
echo "User: $(whoami)"
echo "System: $(uname -a)"
echo "Log file: ${HICLAW_LOG_FILE}"
echo "========================================"
echo ""

# ============================================================
# Utility functions (needed early for timezone detection)
# ============================================================

log() {
    echo -e "\033[36m[HiClaw]\033[0m $1"
}

error() {
    echo -e "\033[31m[HiClaw ERROR]\033[0m $1" >&2
    exit 1
}

# ============================================================
# Timezone detection (compatible with Linux and macOS)
# ============================================================

detect_timezone() {
    local tz=""

    # Try /etc/timezone (Debian/Ubuntu)
    if [ -f /etc/timezone ]; then
        tz=$(cat /etc/timezone 2>/dev/null | tr -d '[:space:]')
    fi

    # Try /etc/localtime symlink (macOS and some Linux)
    if [ -z "${tz}" ] && [ -L /etc/localtime ]; then
        tz=$(ls -l /etc/localtime 2>/dev/null | sed 's|.*/zoneinfo/||')
    fi

    # Try timedatectl (systemd)
    if [ -z "${tz}" ]; then
        tz=$(timedatectl show --value -p Timezone 2>/dev/null)
    fi

    # If still not detected, warn and prompt user
    if [ -z "${tz}" ]; then
        echo ""
        echo -e "\033[33m[HiClaw WARNING]\033[0m Could not detect timezone automatically."
        echo -e "\033[33m[HiClaw]\033[0m Please enter your timezone (e.g., Asia/Shanghai, America/New_York)."
        echo ""
        if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
            tz="Asia/Shanghai"
            log "Using default timezone: ${tz}"
        else
            read -e -p "Timezone [Asia/Shanghai]: " tz
            tz="${tz:-Asia/Shanghai}"
        fi
    fi

    echo "${tz}"
}

# Detect timezone once at startup (used by registry selection and container TZ)
HICLAW_TIMEZONE="${HICLAW_TIMEZONE:-$(detect_timezone)}"

# ============================================================
# Language detection based on timezone
# ============================================================

detect_language() {
    local tz="${HICLAW_TIMEZONE}"
    case "${tz}" in
        Asia/Shanghai|Asia/Chongqing|Asia/Harbin|Asia/Urumqi|\
        Asia/Taipei|Asia/Hong_Kong|Asia/Macau)
            echo "zh"
            ;;
        *)
            echo "en"
            ;;
    esac
}

# Language priority: env var > existing env file > timezone detection
if [ -z "${HICLAW_LANGUAGE}" ]; then
    # Check existing env file for saved language preference (upgrade scenario)
    _env_file="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
    # Migrate from legacy location (current directory) if needed
    if [ ! -f "${_env_file}" ] && [ -f "./hiclaw-manager.env" ]; then
        mv "./hiclaw-manager.env" "${_env_file}" 2>/dev/null || true
    fi
    if [ -f "${_env_file}" ]; then
        _saved_lang=$(grep '^HICLAW_LANGUAGE=' "${_env_file}" 2>/dev/null | cut -d= -f2-)
        if [ -n "${_saved_lang}" ]; then
            HICLAW_LANGUAGE="${_saved_lang}"
        fi
    fi
    # Fall back to timezone-based detection
    if [ -z "${HICLAW_LANGUAGE}" ]; then
        HICLAW_LANGUAGE="$(detect_language)"
    fi
    unset _env_file _saved_lang
fi
export HICLAW_LANGUAGE

# ============================================================
# Centralized message dictionary and msg() function
# Compatible with bash 3.2+ (macOS default) — uses case instead of declare -A
# ============================================================

# msg() function: look up message by key, with printf-style argument substitution
# Falls back to English if the current language translation is missing.
msg() {
    local key="$1"
    shift
    local lang="${HICLAW_LANGUAGE:-en}"
    local text=""
    case "${key}.${lang}" in
        # --- Timezone detection messages ---
        "tz.warning.title.zh") text="无法自动检测时区。" ;;
        "tz.warning.title.en") text="Could not detect timezone automatically." ;;
        "tz.warning.prompt.zh") text="请输入您的时区（例如 Asia/Shanghai、America/New_York）。" ;;
        "tz.warning.prompt.en") text="Please enter your timezone (e.g., Asia/Shanghai, America/New_York)." ;;
        "tz.default.zh") text="使用默认时区: %s" ;;
        "tz.default.en") text="Using default timezone: %s" ;;
        "tz.input_prompt.zh") text="时区" ;;
        "tz.input_prompt.en") text="Timezone" ;;
        # --- Installation title and info ---
        "install.title.zh") text="=== HiClaw Manager 安装 ===" ;;
        "install.title.en") text="=== HiClaw Manager Installation ===" ;;
        "install.registry.zh") text="镜像仓库: %s" ;;
        "install.registry.en") text="Registry: %s" ;;
        "install.dir.zh") text="安装目录: %s" ;;
        "install.dir.en") text="Installation directory: %s" ;;
        "install.dir_hint.zh") text="  （env 文件 'hiclaw-manager.env' 将保存到 HOME 目录。）" ;;
        "install.dir_hint.en") text="  (The env file 'hiclaw-manager.env' will be saved to your HOME directory.)" ;;
        "install.dir_hint2.zh") text="  （请从您希望管理此安装的目录运行此脚本。）" ;;
        "install.dir_hint2.en") text="  (Run this script from the directory where you want to manage this installation.)" ;;
        # --- Onboarding mode ---
        "install.mode.title.zh") text="--- Onboarding 模式 ---" ;;
        "install.mode.title.en") text="--- Onboarding Mode ---" ;;
        "install.mode.choose.zh") text="选择安装模式:" ;;
        "install.mode.choose.en") text="Choose your installation mode:" ;;
        "install.mode.quickstart.zh") text="  1) 快速开始  - 使用阿里云百炼快速安装（推荐）" ;;
        "install.mode.quickstart.en") text="  1) Quick Start  - Fast installation with Alibaba Cloud CodingPlan (recommended)" ;;
        "install.mode.manual.zh") text="  2) 手动配置  - 选择 LLM 提供商并自定义选项" ;;
        "install.mode.manual.en") text="  2) Manual       - Choose LLM provider and customize options" ;;
        "install.mode.prompt.zh") text="请选择 [1/2]" ;;
        "install.mode.prompt.en") text="Enter choice [1/2]" ;;
        "install.mode.quickstart_selected.zh") text="已选择快速开始模式 - 使用阿里云百炼" ;;
        "install.mode.quickstart_selected.en") text="Quick Start mode selected - using Alibaba Cloud CodingPlan" ;;
        "install.mode.manual_selected.zh") text="已选择手动配置模式 - 您将选择 LLM 提供商并自定义选项" ;;
        "install.mode.manual_selected.en") text="Manual mode selected - you will choose LLM provider and customize options" ;;
        "install.mode.invalid.zh") text="无效选择，默认使用快速开始模式" ;;
        "install.mode.invalid.en") text="Invalid choice, defaulting to Quick Start mode" ;;
        # --- Version selection ---
        "install.version.title.zh") text="--- 版本选择 ---" ;;
        "install.version.title.en") text="--- Version Selection ---" ;;
        "install.version.choose.zh") text="选择要安装的版本:" ;;
        "install.version.choose.en") text="Choose the version to install:" ;;
        "install.version.option_latest.zh") text="  1) latest  - 最新版（默认）" ;;
        "install.version.option_latest.en") text="  1) latest  - Latest build (default)" ;;
        "install.version.option_stable.zh") text="  2) %s - 最新稳定版" ;;
        "install.version.option_stable.en") text="  2) %s - Latest stable release" ;;
        "install.version.fetching.zh") text="正在查询最新稳定版本..." ;;
        "install.version.fetching.en") text="Fetching latest stable release..." ;;
        "install.version.fetch_failed.zh") text="无法查询 GitHub，使用内置版本 %s" ;;
        "install.version.fetch_failed.en") text="Could not reach GitHub, using built-in version %s" ;;
        "install.version.option_custom.zh") text="  3) 自定义 - 手动输入版本号（如 v1.0.5）" ;;
        "install.version.option_custom.en") text="  3) Custom  - Enter a specific version (e.g. v1.0.5)" ;;
        "install.version.prompt.zh") text="请选择 [1/2/3]" ;;
        "install.version.prompt.en") text="Enter choice [1/2/3]" ;;
        "install.version.custom_prompt.zh") text="请输入版本号" ;;
        "install.version.custom_prompt.en") text="Enter version tag" ;;
        "install.version.selected_latest.zh") text="已选择最新版 (latest)" ;;
        "install.version.selected_latest.en") text="Selected latest version" ;;
        "install.version.selected_stable.zh") text="已选择最新稳定版 (%s)" ;;
        "install.version.selected_stable.en") text="Selected latest stable version (%s)" ;;
        "install.version.selected_custom.zh") text="已选择自定义版本 (%s)" ;;
        "install.version.selected_custom.en") text="Selected custom version (%s)" ;;
        "install.version.invalid.zh") text="无效选择，使用最新稳定版 (%s)" ;;
        "install.version.invalid.en") text="Invalid choice, defaulting to latest stable version (%s)" ;;
        # --- Existing installation detected ---
        "install.existing.detected.zh") text="检测到已有 Manager 安装（env 文件: %s）" ;;
        "install.existing.detected.en") text="Existing Manager installation detected (env file: %s)" ;;
        "install.existing.choose.zh") text="选择操作:" ;;
        "install.existing.choose.en") text="Choose an action:" ;;
        "install.existing.upgrade.zh") text="  1) 就地升级（保留数据、工作空间、env 文件）" ;;
        "install.existing.upgrade.en") text="  1) In-place upgrade (keep data, workspace, env file)" ;;
        "install.existing.reinstall.zh") text="  2) 全新重装（删除所有数据，重新开始）" ;;
        "install.existing.reinstall.en") text="  2) Clean reinstall (remove all data, start fresh)" ;;
        "install.existing.cancel.zh") text="  3) 取消" ;;
        "install.existing.cancel.en") text="  3) Cancel" ;;
        "install.existing.prompt.zh") text="请选择 [1/2/3]" ;;
        "install.existing.prompt.en") text="Enter choice [1/2/3]" ;;
        "install.existing.upgrade_noninteractive.zh") text="非交互模式: 执行就地升级..." ;;
        "install.existing.upgrade_noninteractive.en") text="Non-interactive mode: performing in-place upgrade..." ;;
        "install.existing.upgrading.zh") text="执行就地升级..." ;;
        "install.existing.upgrading.en") text="Performing in-place upgrade..." ;;
        "install.existing.warn_manager_stop.zh") text="⚠️  Manager 容器将被停止并重新创建。" ;;
        "install.existing.warn_manager_stop.en") text="⚠️  Manager container will be stopped and recreated." ;;
        "install.existing.warn_worker_recreate.zh") text="⚠️  Worker 容器也将被重新创建（以更新 Manager IP）。" ;;
        "install.existing.warn_worker_recreate.en") text="⚠️  Worker containers will also be recreated (to update Manager IP in hosts)." ;;
        "install.existing.continue_prompt.zh") text="继续？[y/N]" ;;
        "install.existing.continue_prompt.en") text="Continue? [y/N]" ;;
        "install.existing.cancelled.zh") text="安装已取消。" ;;
        "install.existing.cancelled.en") text="Installation cancelled." ;;
        "install.existing.stopping_manager.zh") text="停止并移除现有 manager 容器..." ;;
        "install.existing.stopping_manager.en") text="Stopping and removing existing manager container..." ;;
        "install.existing.stopping_workers.zh") text="停止并移除现有 worker 容器..." ;;
        "install.existing.stopping_workers.en") text="Stopping and removing existing worker containers..." ;;
        "install.existing.removed.zh") text="  已移除: %s" ;;
        "install.existing.removed.en") text="  Removed: %s" ;;
        # --- Clean reinstall messages ---
        "install.reinstall.performing.zh") text="执行全新重装..." ;;
        "install.reinstall.performing.en") text="Performing clean reinstall..." ;;
        "install.reinstall.warn_stop.zh") text="⚠️  以下运行中的容器将被停止:" ;;
        "install.reinstall.warn_stop.en") text="⚠️  The following running containers will be stopped:" ;;
        "install.reinstall.warn_delete.zh") text="⚠️  警告: 以下内容将被删除:" ;;
        "install.reinstall.warn_delete.en") text="⚠️  WARNING: This will DELETE the following:" ;;
        "install.reinstall.warn_volume.zh") text="   - Docker 卷: hiclaw-data" ;;
        "install.reinstall.warn_volume.en") text="   - Docker volume: hiclaw-data" ;;
        "install.reinstall.warn_env.zh") text="   - Env 文件: %s" ;;
        "install.reinstall.warn_env.en") text="   - Env file: %s" ;;
        "install.reinstall.warn_workspace.zh") text="   - Manager 工作空间: %s" ;;
        "install.reinstall.warn_workspace.en") text="   - Manager workspace: %s" ;;
        "install.reinstall.warn_workers.zh") text="   - 所有 worker 容器" ;;
        "install.reinstall.warn_workers.en") text="   - All worker containers" ;;
        "install.reinstall.warn_proxy.zh") text="   - Docker API 代理容器: hiclaw-docker-proxy" ;;
        "install.reinstall.warn_proxy.en") text="   - Docker API proxy container: hiclaw-docker-proxy" ;;
        "install.reinstall.warn_network.zh") text="   - Docker 网络: hiclaw-net" ;;
        "install.reinstall.warn_network.en") text="   - Docker network: hiclaw-net" ;;
        "install.reinstall.confirm_type.zh") text="请输入工作空间路径以确认删除（或按 Ctrl+C 取消）:" ;;
        "install.reinstall.confirm_type.en") text="To confirm deletion, please type the workspace path:" ;;
        "install.reinstall.confirm_path.zh") text="输入路径以确认（或按 Ctrl+C 取消）" ;;
        "install.reinstall.confirm_path.en") text="Type the path to confirm (or press Ctrl+C to cancel)" ;;
        "install.reinstall.path_mismatch.zh") text="路径不匹配。中止重装。输入: '%s'，期望: '%s'" ;;
        "install.reinstall.path_mismatch.en") text="Path mismatch. Aborting reinstall. Input: '%s', Expected: '%s'" ;;
        "install.reinstall.confirmed.zh") text="已确认。正在清理..." ;;
        "install.reinstall.confirmed.en") text="Confirmed. Cleaning up..." ;;
        "install.reinstall.removed_worker.zh") text="  已移除 worker: %s" ;;
        "install.reinstall.removed_worker.en") text="  Removed worker: %s" ;;
        "install.reinstall.removing_volume.zh") text="正在移除 Docker 卷: hiclaw-data" ;;
        "install.reinstall.removing_volume.en") text="Removing Docker volume: hiclaw-data" ;;
        "install.reinstall.warn_volume_fail.zh") text="  警告: 无法移除卷（可能有引用）" ;;
        "install.reinstall.warn_volume_fail.en") text="  Warning: Could not remove volume (may have references)" ;;
        "install.reinstall.removing_proxy.zh") text="正在移除 Docker API 代理容器: hiclaw-docker-proxy" ;;
        "install.reinstall.removing_proxy.en") text="Removing Docker API proxy container: hiclaw-docker-proxy" ;;
        "install.reinstall.removing_network.zh") text="正在移除 Docker 网络: hiclaw-net" ;;
        "install.reinstall.removing_network.en") text="Removing Docker network: hiclaw-net" ;;
        "install.reinstall.removing_workspace.zh") text="正在移除工作空间目录: %s" ;;
        "install.reinstall.removing_workspace.en") text="Removing workspace directory: %s" ;;
        "install.reinstall.removing_env.zh") text="正在移除 env 文件: %s" ;;
        "install.reinstall.removing_env.en") text="Removing env file: %s" ;;
        "install.reinstall.cleanup_done.zh") text="清理完成。开始全新安装..." ;;
        "install.reinstall.cleanup_done.en") text="Cleanup complete. Starting fresh installation..." ;;
        "install.reinstall.failed_rm_workspace.zh") text="无法移除工作空间目录" ;;
        "install.reinstall.failed_rm_workspace.en") text="Failed to remove workspace directory" ;;
        # --- Orphan volume detection ---
        "install.orphan_volume.detected.zh") text="⚠️  检测到残留数据卷 '%s'，但未找到对应的 env 配置文件。" ;;
        "install.orphan_volume.detected.en") text="⚠️  Found leftover data volume '%s' but no matching env config file." ;;
        "install.orphan_volume.warn.zh") text="这可能是之前安装的残留数据，会导致新安装出现异常（如密码冲突、服务启动失败）。" ;;
        "install.orphan_volume.warn.en") text="This is likely leftover data from a previous installation and may cause issues (e.g., credential conflicts, service startup failures)." ;;
        "install.orphan_volume.choose.zh") text="选择操作:" ;;
        "install.orphan_volume.choose.en") text="Choose an action:" ;;
        "install.orphan_volume.clean.zh") text="  1) 清理残留数据卷后继续安装（推荐）" ;;
        "install.orphan_volume.clean.en") text="  1) Remove leftover volume and continue installation (recommended)" ;;
        "install.orphan_volume.keep.zh") text="  2) 保留数据卷继续安装（可能出现异常）" ;;
        "install.orphan_volume.keep.en") text="  2) Keep the volume and continue installation (may cause issues)" ;;
        "install.orphan_volume.prompt.zh") text="请选择 [1/2]" ;;
        "install.orphan_volume.prompt.en") text="Enter choice [1/2]" ;;
        "install.orphan_volume.cleaning.zh") text="正在清理残留数据卷..." ;;
        "install.orphan_volume.cleaning.en") text="Removing leftover data volume..." ;;
        "install.orphan_volume.cleaned.zh") text="残留数据卷已清理。继续全新安装..." ;;
        "install.orphan_volume.cleaned.en") text="Leftover volume removed. Continuing with fresh installation..." ;;
        "install.orphan_volume.keeping.zh") text="保留数据卷，继续安装。如遇异常请选择全新重装。" ;;
        "install.orphan_volume.keeping.en") text="Keeping existing volume. If you encounter issues, consider a clean reinstall." ;;
        "install.orphan_volume.clean_noninteractive.zh") text="非交互模式: 自动清理残留数据卷..." ;;
        "install.orphan_volume.clean_noninteractive.en") text="Non-interactive mode: automatically removing leftover volume..." ;;
        # --- Loading existing config ---
        "install.loading_config.zh") text="从 %s 加载已有配置（shell 环境变量优先）..." ;;
        "install.loading_config.en") text="Loading existing config from %s (shell env vars take priority)..." ;;
        # --- LLM Configuration ---
        "llm.title.zh") text="--- LLM 配置 ---" ;;
        "llm.title.en") text="--- LLM Configuration ---" ;;
        "llm.provider.label.zh") text="  提供商: %s" ;;
        "llm.provider.label.en") text="  Provider: %s" ;;
        "llm.model.label.zh") text="  模型: %s" ;;
        "llm.model.label.en") text="  Model: %s" ;;
        "llm.provider.qwen.zh") text="  提供商: qwen（阿里云百炼）" ;;
        "llm.provider.qwen.en") text="  Provider: qwen (Alibaba Cloud Bailian)" ;;
        "llm.provider.qwen_default.zh") text="  提供商: %s（默认）" ;;
        "llm.provider.qwen_default.en") text="  Provider: %s (default)" ;;
        "llm.model.default.zh") text="  模型: %s（默认）" ;;
        "llm.model.default.en") text="  Model: %s (default)" ;;
        "llm.apikey_hint.zh") text="  💡 获取阿里云百炼 API Key:" ;;
        "llm.apikey_hint.en") text="  💡 Get your Alibaba Cloud CodingPlan API Key from:" ;;
        "llm.apikey_url.zh") text="     https://www.aliyun.com/product/bailian" ;;
        "llm.apikey_url.en") text="     https://www.alibabacloud.com/en/campaign/ai-scene-coding" ;;
        "llm.apikey_prompt.zh") text="LLM API Key" ;;
        "llm.apikey_prompt.en") text="LLM API Key" ;;
        "llm.providers_title.zh") text="可用 LLM 提供商:" ;;
        "llm.providers_title.en") text="Available LLM Providers:" ;;
        "llm.provider.alibaba.zh") text="  1) 阿里云百炼  - 推荐中国用户使用" ;;
        "llm.provider.alibaba.en") text="  1) Alibaba Cloud CodingPlan  - Optimized for coding tasks (recommended)" ;;
        "llm.provider.openai_compat.zh") text="  2) OpenAI 兼容 API  - 自定义 Base URL（OpenAI、DeepSeek 等）" ;;
        "llm.provider.openai_compat.en") text="  2) OpenAI-compatible API  - Custom Base URL (OpenAI, DeepSeek, etc.)" ;;
        "llm.provider.select.zh") text="选择提供商 [1/2]" ;;
        "llm.provider.select.en") text="Select provider [1/2]" ;;
        "llm.alibaba.models_title.zh") text="选择百炼模型系列:" ;;
        "llm.alibaba.models_title.en") text="Select Bailian model series:" ;;
        "llm.alibaba.model.codingplan.zh") text="  1) CodingPlan  - 专为编程任务优化（推荐）" ;;
        "llm.alibaba.model.codingplan.en") text="  1) CodingPlan  - Optimized for coding tasks (recommended)" ;;
        "llm.alibaba.model.qwen.zh") text="  2) 百炼通用接口" ;;
        "llm.alibaba.model.qwen.en") text="  2) qwen general  - General purpose LLM" ;;
        "llm.alibaba.model.select.zh") text="选择模型系列 [1/2]" ;;
        "llm.alibaba.model.select.en") text="Select model series [1/2]" ;;
        "llm.codingplan.models_title.zh") text="选择 CodingPlan 默认模型:" ;;
        "llm.codingplan.models_title.en") text="Select CodingPlan default model:" ;;
        "llm.codingplan.model.qwen35plus.zh") text="  1) qwen3.5-plus  - 千问 3.5（速度最快）" ;;
        "llm.codingplan.model.qwen35plus.en") text="  1) qwen3.5-plus  - Qwen 3.5 (fastest)" ;;
        "llm.codingplan.model.glm5.zh") text="  2) glm-5  - 智谱 GLM-5（编程推荐）" ;;
        "llm.codingplan.model.glm5.en") text="  2) glm-5  - Zhipu GLM-5 (recommended for coding)" ;;
        "llm.codingplan.model.kimi.zh") text="  3) kimi-k2.5  - Moonshot Kimi K2.5" ;;
        "llm.codingplan.model.kimi.en") text="  3) kimi-k2.5  - Moonshot Kimi K2.5" ;;
        "llm.codingplan.model.minimax.zh") text="  4) MiniMax-M2.5  - MiniMax M2.5" ;;
        "llm.codingplan.model.minimax.en") text="  4) MiniMax-M2.5  - MiniMax M2.5" ;;
        "llm.codingplan.model.select.zh") text="选择模型 [1/2/3/4]" ;;
        "llm.codingplan.model.select.en") text="Select model [1/2/3/4]" ;;
        "llm.provider.selected_codingplan.zh") text="  提供商: 阿里云百炼 CodingPlan" ;;
        "llm.provider.selected_codingplan.en") text="  Provider: Alibaba Cloud CodingPlan" ;;
        "llm.provider.selected_qwen.zh") text="  提供商: 阿里云百炼" ;;
        "llm.provider.selected_qwen.en") text="  Provider: Alibaba Cloud Bailian" ;;
        "llm.provider.selected_openai.zh") text="  提供商: %s（OpenAI 兼容）" ;;
        "llm.provider.selected_openai.en") text="  Provider: %s (OpenAI-compatible)" ;;
        "llm.provider.invalid.zh") text="无效选择: %s（请输入 1 或 2）" ;;
        "llm.provider.invalid.en") text="Invalid choice: %s (please enter 1 or 2)" ;;
        "llm.qwen.model_prompt.zh") text="默认模型 ID [qwen3.5-plus]" ;;
        "llm.qwen.model_prompt.en") text="Default Model ID [qwen3.5-plus]" ;;
        "llm.openai.base_url_prompt.zh") text="Base URL（例如 https://api.openai.com/v1）" ;;
        "llm.openai.base_url_prompt.en") text="Base URL (e.g., https://api.openai.com/v1)" ;;
        "llm.openai.model_prompt.zh") text="默认模型 ID [gpt-5.4]" ;;
        "llm.openai.model_prompt.en") text="Default Model ID [gpt-5.4]" ;;
        "llm.openai.base_url_label.zh") text="  Base URL: %s" ;;
        "llm.openai.base_url_label.en") text="  Base URL: %s" ;;
        # --- Custom model parameters ---
        "llm.custom_model.detected.zh") text="  ⚠️  模型 '%s' 不在内置模型列表中，请配置模型参数:" ;;
        "llm.custom_model.detected.en") text="  ⚠️  Model '%s' is not in the built-in model list. Please configure model parameters:" ;;
        "llm.custom_model.context_prompt.zh") text="最大上下文长度（token 数）[150000]" ;;
        "llm.custom_model.context_prompt.en") text="Max context window (tokens) [150000]" ;;
        "llm.custom_model.max_tokens_prompt.zh") text="最大输出长度（token 数）[128000]" ;;
        "llm.custom_model.max_tokens_prompt.en") text="Max output tokens [128000]" ;;
        "llm.custom_model.reasoning_prompt.zh") text="是否支持推理/思考模式？[Y/n]" ;;
        "llm.custom_model.reasoning_prompt.en") text="Does it support reasoning/thinking mode? [Y/n]" ;;
        "llm.custom_model.vision_prompt.zh") text="是否支持图片输入？[y/N]" ;;
        "llm.custom_model.vision_prompt.en") text="Does it support image input? [y/N]" ;;
        "llm.custom_model.summary.zh") text="  自定义模型参数: 上下文=%s, 最大输出=%s, 推理=%s, 图片=%s" ;;
        "llm.custom_model.summary.en") text="  Custom model params: context=%s, maxTokens=%s, reasoning=%s, vision=%s" ;;
        # --- Admin Credentials ---
        "admin.title.zh") text="--- 管理员凭据 ---" ;;
        "admin.title.en") text="--- Admin Credentials ---" ;;
        "admin.username_prompt.zh") text="管理员用户名" ;;
        "admin.username_prompt.en") text="Admin Username" ;;
        "admin.password_prompt.zh") text="管理员密码（留空自动生成，最少 8 位）" ;;
        "admin.password_prompt.en") text="Admin Password (leave empty to auto-generate, min 8 chars)" ;;
        "admin.password_generated.zh") text="  已自动生成管理员密码" ;;
        "admin.password_generated.en") text="  Auto-generated admin password" ;;
        "admin.password_too_short.zh") text="管理员密码至少需要 8 个字符（MinIO 要求）。当前长度: %s" ;;
        "admin.password_too_short.en") text="Admin password must be at least 8 characters (MinIO requirement). Current length: %s" ;;
        # --- Port Configuration ---
        "port.title.zh") text="--- 端口配置（按回车使用默认值）---" ;;
        "port.title.en") text="--- Port Configuration (press Enter for defaults) ---" ;;
        "port.gateway_prompt.zh") text="网关主机端口（容器内 8080）" ;;
        "port.gateway_prompt.en") text="Host port for gateway (8080 inside container)" ;;
        "port.console_prompt.zh") text="Higress 控制台主机端口（容器内 8001）" ;;
        "port.console_prompt.en") text="Host port for Higress console (8001 inside container)" ;;
        "port.element_prompt.zh") text="Element Web 直接访问主机端口（容器内 8088）" ;;
        "port.element_prompt.en") text="Host port for Element Web direct access (8088 inside container)" ;;
        "port.openclaw_console_prompt.zh") text="OpenClaw 控制台主机端口（容器内 18888）" ;;
        "port.openclaw_console_prompt.en") text="Host port for OpenClaw console (18888 inside container)" ;;
        # --- Local-only binding ---
        "port.local_only.title.zh") text="--- 网络访问模式 ---" ;;
        "port.local_only.title.en") text="--- Network Access Mode ---" ;;
        "port.local_only.prompt.zh") text="是否仅允许本机访问（端口绑定到 127.0.0.1）？" ;;
        "port.local_only.prompt.en") text="Bind ports to localhost only (127.0.0.1)?" ;;
        "port.local_only.hint_yes.zh") text="  仅本机使用，无需开放外部端口（推荐）" ;;
        "port.local_only.hint_yes.en") text="  Local use only, no external port exposure (recommended)" ;;
        "port.local_only.hint_no.zh") text="  允许外部访问（局域网 / 公网）" ;;
        "port.local_only.hint_no.en") text="  Allow external access (LAN / public network)" ;;
        "port.local_only.choice.zh") text="请选择 [1/2]" ;;
        "port.local_only.choice.en") text="Enter choice [1/2]" ;;
        "port.local_only.selected_local.zh") text="端口已绑定到 127.0.0.1（仅本机访问）" ;;
        "port.local_only.selected_local.en") text="Ports bound to 127.0.0.1 (localhost only)" ;;
        "port.local_only.selected_external.zh") text="端口已绑定到所有网络接口（0.0.0.0）" ;;
        "port.local_only.selected_external.en") text="Ports bound to all interfaces (0.0.0.0)" ;;
        "port.local_only.https_hint.zh") text="⚠️  建议在 Higress 控制台配置 TLS 证书并启用 HTTPS，避免明文传输。" ;;
        "port.local_only.https_hint.en") text="⚠️  It is recommended to configure TLS certificates and enable HTTPS in the Higress Console to avoid plaintext transmission." ;;
        "port.local_only.https_docs.zh") text="" ;;
        "port.local_only.https_docs.en") text="" ;;
        # --- Domain Configuration ---
        "domain.title.zh") text="--- 域名配置（按回车使用默认值）---" ;;
        "domain.title.en") text="--- Domain Configuration (press Enter for defaults) ---" ;;
        "domain.hint.zh") text="提示: 自定义域名前必须事先做好 DNS 解析。单机 ECS 部署时无需修改 aigw、fs 等域名；Element Web 和 Matrix Server 也可通过 IP 直接访问。" ;;
        "domain.hint.en") text="Hint: Configure DNS resolution before customizing domains. For single ECS deployment, no need to change aigw, fs, etc.; Element Web and Matrix Server can also be accessed directly via IP." ;;
        "domain.matrix_prompt.zh") text="Matrix 域名" ;;
        "domain.matrix_prompt.en") text="Matrix Domain" ;;
        "domain.element_prompt.zh") text="Element Web 域名" ;;
        "domain.element_prompt.en") text="Element Web Domain" ;;
        "domain.gateway_prompt.zh") text="AI 网关域名" ;;
        "domain.gateway_prompt.en") text="AI Gateway Domain" ;;
        "domain.fs_prompt.zh") text="文件系统域名" ;;
        "domain.fs_prompt.en") text="File System Domain" ;;
        "domain.console_prompt.zh") text="OpenClaw 控制台域名" ;;
        "domain.console_prompt.en") text="OpenClaw Console Domain" ;;
        # --- GitHub Integration ---
        "github.title.zh") text="--- GitHub 集成（可选，按回车跳过）---" ;;
        "github.title.en") text="--- GitHub Integration (optional, press Enter to skip) ---" ;;
        "github.token_prompt.zh") text="GitHub 个人访问令牌（可选）" ;;
        "github.token_prompt.en") text="GitHub Personal Access Token (optional)" ;;
        # --- Skills Registry ---
        "skills.title.zh") text="--- Skills 注册中心（可选，按回车使用默认 https://skills.sh）---" ;;
        "skills.title.en") text="--- Skills Registry (optional, press Enter for default https://skills.sh) ---" ;;
        "skills.url_prompt.zh") text="Skills 注册中心 URL（留空使用默认 https://skills.sh）" ;;
        "skills.url_prompt.en") text="Skills Registry URL (leave empty for default https://skills.sh)" ;;
        # --- Data Persistence ---
        "data.title.zh") text="--- 数据持久化 ---" ;;
        "data.title.en") text="--- Data Persistence ---" ;;
        "data.volume_prompt.zh") text="Docker 卷名称 [hiclaw-data]" ;;
        "data.volume_prompt.en") text="Docker volume name for persistent data [hiclaw-data]" ;;
        "data.volume_using.zh") text="  使用 Docker 卷: %s" ;;
        "data.volume_using.en") text="  Using Docker volume: %s" ;;
        # --- Manager Workspace ---
        "workspace.title.zh") text="--- Manager 工作空间 ---" ;;
        "workspace.title.en") text="--- Manager Workspace ---" ;;
        "workspace.dir_prompt.zh") text="Manager 工作空间目录 [%s]" ;;
        "workspace.dir_prompt.en") text="Manager workspace directory [%s]" ;;
        "workspace.dir_label.zh") text="  Manager 工作空间: %s" ;;
        "workspace.dir_label.en") text="  Manager workspace: %s" ;;
        # --- Host directory sharing ---
        "host_share.prompt.zh") text="与 Agent 共享的主机目录（默认: %s）" ;;
        "host_share.prompt.en") text="Host directory to share with agents (default: %s)" ;;
        "host_share.sharing.zh") text="共享主机目录: %s -> 容器内 /host-share" ;;
        "host_share.sharing.en") text="Sharing host directory: %s -> /host-share in container" ;;
        "host_share.not_exist.zh") text="警告: 主机目录 %s 不存在，跳过验证继续使用" ;;
        "host_share.not_exist.en") text="WARNING: Host directory %s does not exist, using without validation" ;;
        # --- Default worker runtime ---
        "worker_runtime.title.zh") text="--- 默认 Worker 运行时 ---" ;;
        "worker_runtime.title.en") text="--- Default Worker Runtime ---" ;;
        "worker_runtime.openclaw.zh") text="OpenClaw（Node.js 容器，~500MB 内存）" ;;
        "worker_runtime.openclaw.en") text="OpenClaw (Node.js container, ~500MB RAM)" ;;
        "worker_runtime.copaw.zh") text="CoPaw（Python 容器，~150MB 内存，默认关闭控制台，可跟 Manager 对话按需开启）" ;;
        "worker_runtime.copaw.en") text="CoPaw (Python container, ~150MB RAM, console off by default, enable on demand via Manager)" ;;
        "worker_runtime.choice.zh") text="请选择 [1/2]" ;;
        "worker_runtime.choice.en") text="Enter choice [1/2]" ;;
        "worker_runtime.selected.zh") text="默认 Worker 运行时: %s" ;;
        "worker_runtime.selected.en") text="Default Worker runtime: %s" ;;
        "worker_runtime.title_short.zh") text="默认 Worker 运行时" ;;
        "worker_runtime.title_short.en") text="Default Worker Runtime" ;;
        # --- Secrets and config ---
        "install.generating_secrets.zh") text="正在生成密钥..." ;;
        "install.generating_secrets.en") text="Generating secrets..." ;;
        "install.config_saved.zh") text="配置已保存到 %s" ;;
        "install.config_saved.en") text="Configuration saved to %s" ;;
        # --- Container runtime socket ---
        "install.socket_detected.zh") text="容器运行时 socket: %s（已启用直接创建 Worker）" ;;
        "install.socket_detected.en") text="Container runtime socket: %s (direct Worker creation enabled)" ;;
        "install.socket_not_found.zh") text="未找到容器运行时 socket（Manager 无法直接创建 Worker 容器，需要你手动执行 docker 命令创建）" ;;
        "install.socket_not_found.en") text="No container runtime socket found (Manager cannot create Worker containers directly, you will need to create them manually using docker commands)" ;;
        "install.socket_confirm.title.zh") text="⚠️ 未检测到容器运行时 Socket" ;;
        "install.socket_confirm.title.en") text="⚠️ Container Runtime Socket Not Detected" ;;
        "install.socket_confirm.message.zh") text="未找到 Docker/Podman socket，Manager 将无法自动创建 Worker 容器。\n你需要手动执行 docker run 命令来创建 Worker。\n\n是否继续安装？" ;;
        "install.socket_confirm.message.en") text="Docker/Podman socket not found. Manager will not be able to create Worker containers automatically.\nYou will need to manually run docker commands to create Workers.\n\nContinue installation?" ;;
        "install.socket_confirm.prompt.zh") text="继续安装? [y/N]: " ;;
        "install.socket_confirm.prompt.en") text="Continue? [y/N]: " ;;
        "install.socket_confirm.cancelled.zh") text="安装已取消。如需启用 Worker 自动创建，请确保 Docker/Podman 正在运行，然后重新运行安装脚本。" ;;
        "install.socket_confirm.cancelled.en") text="Installation cancelled. To enable automatic Worker creation, ensure Docker/Podman is running and re-run the installer." ;;
        # --- Container management ---
        "install.removing_existing.zh") text="正在移除现有 hiclaw-manager 容器..." ;;
        "install.removing_existing.en") text="Removing existing hiclaw-manager container..." ;;
        # --- Matrix E2EE ---
        "matrix_e2ee.title.zh") text="--- Matrix 端到端加密（E2EE）---" ;;
        "matrix_e2ee.title.en") text="--- Matrix End-to-End Encryption (E2EE) ---" ;;
        "matrix_e2ee.desc.zh") text="E2EE 会对 Manager 与 Worker 之间的 Matrix 消息进行端到端加密。\n  启用后，即使 Matrix 服务器被入侵，消息内容也无法被窃取。\n  但 E2EE 会增加首次握手耗时，且要求所有 Agent 都支持 matrix-sdk-crypto。\n  如果不确定，建议保持禁用。\n  ⚠ 注意：禁用 E2EE 后，请勿在 Element 上创建默认启用加密的 Private 房间，\n  否则 Agent 将无法读取该房间中的加密消息。请改用 Public 房间或关闭房间加密。" ;;
        "matrix_e2ee.desc.en") text="E2EE encrypts Matrix messages between Manager and Workers end-to-end.\n  When enabled, message content stays private even if the Matrix server is compromised.\n  However, E2EE adds overhead to the initial handshake and requires all Agents\n  to support matrix-sdk-crypto. If unsure, keep it disabled.\n  ⚠ Note: When E2EE is disabled, do NOT create Private rooms in Element (which\n  enable encryption by default) — Agents cannot read encrypted messages without\n  E2EE support. Use Public rooms or turn off room encryption instead." ;;
        "matrix_e2ee.enable.zh") text="启用 E2EE" ;;
        "matrix_e2ee.enable.en") text="Enable E2EE" ;;
        "matrix_e2ee.disable.zh") text="禁用 E2EE（推荐）" ;;
        "matrix_e2ee.disable.en") text="Disable E2EE (recommended)" ;;
        "matrix_e2ee.choice.zh") text="请选择 [1/2]" ;;
        "matrix_e2ee.choice.en") text="Enter choice [1/2]" ;;
        "matrix_e2ee.selected_enabled.zh") text="Matrix E2EE: 已启用" ;;
        "matrix_e2ee.selected_enabled.en") text="Matrix E2EE: enabled" ;;
        "matrix_e2ee.selected_disabled.zh") text="Matrix E2EE: 已禁用（默认）" ;;
        "matrix_e2ee.selected_disabled.en") text="Matrix E2EE: disabled (default)" ;;
        "matrix_e2ee.title_short.zh") text="Matrix E2EE" ;;
        "matrix_e2ee.title_short.en") text="Matrix E2EE" ;;
        "matrix_e2ee.val_enabled.zh") text="已启用" ;;
        "matrix_e2ee.val_enabled.en") text="enabled" ;;
        "matrix_e2ee.val_disabled.zh") text="已禁用" ;;
        "matrix_e2ee.val_disabled.en") text="disabled" ;;
        # --- Docker API proxy ---
        "docker_proxy.title.zh") text="--- Docker API 安全代理 ---" ;;
        "docker_proxy.title.en") text="--- Docker API Security Proxy ---" ;;
        "docker_proxy.desc.zh") text="Docker API 代理可防止 AI Agent 通过 Docker API 越狱访问宿主机。\n  启用后，Manager 不再直接持有 Docker socket，所有容器操作经过安全校验。" ;;
        "docker_proxy.desc.en") text="Docker API proxy prevents AI Agents from escaping via Docker API to access the host.\n  When enabled, Manager no longer has direct Docker socket access; all container operations go through security validation." ;;
        "docker_proxy.enable.zh") text="启用（推荐）" ;;
        "docker_proxy.enable.en") text="Enable (recommended)" ;;
        "docker_proxy.disable.zh") text="禁用（直接挂载 Docker socket）" ;;
        "docker_proxy.disable.en") text="Disable (mount Docker socket directly)" ;;
        "docker_proxy.choice.zh") text="请选择 [1/2]" ;;
        "docker_proxy.choice.en") text="Enter choice [1/2]" ;;
        "docker_proxy.selected_enabled.zh") text="Docker API 代理: 已启用" ;;
        "docker_proxy.selected_enabled.en") text="Docker API proxy: enabled" ;;
        "docker_proxy.selected_disabled.zh") text="Docker API 代理: 已禁用" ;;
        "docker_proxy.selected_disabled.en") text="Docker API proxy: disabled" ;;
        "docker_proxy.title_short.zh") text="Docker API 代理" ;;
        "docker_proxy.title_short.en") text="Docker API Proxy" ;;
        "docker_proxy.val_enabled.zh") text="已启用" ;;
        "docker_proxy.val_enabled.en") text="enabled" ;;
        "docker_proxy.val_disabled.zh") text="已禁用" ;;
        "docker_proxy.val_disabled.en") text="disabled" ;;
        "docker_proxy.registries_desc.zh") text="默认放行的镜像来源：本地镜像、localhost、Higress 仓库（所有 region）。\n  如需放行其他镜像仓库，请输入逗号分隔的地址前缀。\n  示例: ghcr.io/myorg,registry.example.com/team" ;;
        "docker_proxy.registries_desc.en") text="Default allowed image sources: local images, localhost, Higress registries (all regions).\n  To allow additional image sources, enter comma-separated address prefixes.\n  Example: ghcr.io/myorg,registry.example.com/team" ;;
        "docker_proxy.registries_prompt.zh") text="额外放行的镜像来源（按回车跳过）" ;;
        "docker_proxy.registries_prompt.en") text="Additional allowed image sources (press Enter to skip)" ;;
        "docker_proxy.registries_label.zh") text="额外放行的镜像来源" ;;
        "docker_proxy.registries_label.en") text="Additional allowed image sources" ;;
        # --- Worker idle timeout ---
        "idle_timeout.prompt.zh") text="Worker 空闲自动停止超时（分钟）[720]" ;;
        "idle_timeout.prompt.en") text="Worker idle auto-stop timeout in minutes [720]" ;;
        "idle_timeout.selected.zh") text="Worker 空闲超时: %s 分钟" ;;
        "idle_timeout.selected.en") text="Worker idle timeout: %s minutes" ;;
        "idle_timeout.label.zh") text="Worker 空闲超时（分钟）" ;;
        "idle_timeout.label.en") text="Worker idle timeout (min)" ;;
        # --- YOLO mode ---
        "install.yolo.zh") text="YOLO 模式已启用（自主决策，无交互提示）" ;;
        "install.yolo.en") text="YOLO mode enabled (autonomous decisions, no interactive prompts)" ;;
        # --- Image pulling ---
        "install.image.exists.zh") text="Manager 镜像已存在: %s" ;;
        "install.image.exists.en") text="Manager image already exists locally: %s" ;;
        "install.image.pulling_manager.zh") text="正在拉取 Manager 镜像: %s" ;;
        "install.image.pulling_manager.en") text="Pulling Manager image: %s" ;;
        "install.image.worker_exists.zh") text="Worker 镜像已存在: %s" ;;
        "install.image.worker_exists.en") text="Worker image already exists locally: %s" ;;
        "install.image.pulling_worker.zh") text="正在拉取 Worker 镜像: %s" ;;
        "install.image.pulling_worker.en") text="Pulling Worker image: %s" ;;
        # --- Starting container ---
        "install.starting_manager.zh") text="正在启动 Manager 容器..." ;;
        "install.starting_manager.en") text="Starting Manager container..." ;;
        # --- Wait for Manager ready ---
        "install.wait_ready.zh") text="等待 Manager Agent 就绪（超时: %ss）..." ;;
        "install.wait_ready.en") text="Waiting for Manager agent to be ready (timeout: %ss)..." ;;
        "install.wait_ready.ok.zh") text="Manager Agent 已就绪！" ;;
        "install.wait_ready.ok.en") text="Manager agent is ready!" ;;
        "install.wait_ready.waiting.zh") text="等待中... (%ds/%ds)" ;;
        "install.wait_ready.waiting.en") text="Waiting... (%ds/%ds)" ;;
        "install.wait_ready.timeout.zh") text="Manager Agent 在 %ss 内未就绪。请检查: docker logs %s" ;;
        "install.wait_ready.timeout.en") text="Manager agent did not become ready within %ss. Check: docker logs %s" ;;
        # --- Wait for Matrix ready ---
        "install.wait_matrix.zh") text="等待 Matrix 服务就绪（超时: %ss）..." ;;
        "install.wait_matrix.en") text="Waiting for Matrix server to be ready (timeout: %ss)..." ;;
        "install.wait_matrix.ok.zh") text="Matrix 服务已就绪！" ;;
        "install.wait_matrix.ok.en") text="Matrix server is ready!" ;;
        "install.wait_matrix.waiting.zh") text="等待 Matrix 中... (%ds/%ds)" ;;
        "install.wait_matrix.waiting.en") text="Waiting for Matrix... (%ds/%ds)" ;;
        "install.wait_matrix.timeout.zh") text="Matrix 服务在 %ss 内未就绪。请检查: docker logs %s" ;;
        "install.wait_matrix.timeout.en") text="Matrix server did not become ready within %ss. Check: docker logs %s" ;;
        # --- OpenAI-compatible connectivity test ---
        "llm.openai.test.testing.zh") text="正在测试 API 联通性..." ;;
        "llm.openai.test.testing.en") text="Testing API connectivity..." ;;
        "llm.openai.test.ok.zh") text="✅ API 联通性测试通过" ;;
        "llm.openai.test.ok.en") text="✅ API connectivity test passed" ;;
        "llm.openai.test.fail.zh") text="⚠️  API 联通性测试失败（HTTP %s）。响应内容:\n%s\n请根据以上错误信息联系您的模型服务商解决。" ;;
        "llm.openai.test.fail.en") text="⚠️  API connectivity test failed (HTTP %s). Response body:\n%s\nPlease contact your model provider to resolve the issue." ;;
        "llm.openai.test.fail.codingplan.zh") text="⚠️  提示: 请确认您的 API Key 已开通阿里云百炼 CodingPlan 服务。开通地址: https://www.aliyun.com/benefit/scene/codingplan" ;;
        "llm.openai.test.fail.codingplan.en") text="⚠️  Hint: Please verify that your API Key has CodingPlan service enabled. Enable at: https://www.alibabacloud.com/en/campaign/ai-scene-coding" ;;
        "llm.openai.test.no_curl.zh") text="⚠️  未找到 curl，跳过 API 联通性测试" ;;
        "llm.openai.test.no_curl.en") text="⚠️  curl not found, skipping API connectivity test" ;;
        "llm.openai.test.confirm.zh") text="是否仍要继续安装？[y/N/b] " ;;
        "llm.openai.test.confirm.en") text="Continue with installation anyway? [y/N/b] " ;;
        "llm.openai.test.aborted.zh") text="安装已中止。" ;;
        "llm.openai.test.aborted.en") text="Installation aborted." ;;
        "nav.back_hint.zh") text="（输入 b 返回上一步）" ;;
        "nav.back_hint.en") text="(enter b to go back)" ;;
        # --- OpenAI-compatible provider creation ---
        "install.openai_compat.missing.zh") text="警告: OpenAI Base URL 或 API Key 未设置，跳过提供商创建" ;;
        "install.openai_compat.missing.en") text="WARNING: OpenAI Base URL or API Key not set, skipping provider creation" ;;
        "install.openai_compat.creating.zh") text="正在创建 OpenAI 兼容提供商..." ;;
        "install.openai_compat.creating.en") text="Creating OpenAI-compatible provider..." ;;
        "install.openai_compat.domain.zh") text="  域名: %s" ;;
        "install.openai_compat.domain.en") text="  Domain: %s" ;;
        "install.openai_compat.port.zh") text="  端口: %s" ;;
        "install.openai_compat.port.en") text="  Port: %s" ;;
        "install.openai_compat.protocol.zh") text="  协议: %s" ;;
        "install.openai_compat.protocol.en") text="  Protocol: %s" ;;
        "install.openai_compat.service_fail.zh") text="警告: 创建 DNS 服务源失败（可能已存在）" ;;
        "install.openai_compat.service_fail.en") text="WARNING: Failed to create DNS service source (may already exist)" ;;
        "install.openai_compat.provider_fail.zh") text="警告: 创建 AI 提供商失败（可能已存在）" ;;
        "install.openai_compat.provider_fail.en") text="WARNING: Failed to create AI provider (may already exist)" ;;
        "install.openai_compat.success.zh") text="OpenAI 兼容提供商创建成功" ;;
        "install.openai_compat.success.en") text="OpenAI-compatible provider created successfully" ;;
        # --- Welcome message ---
        "install.welcome_msg.soul_configured.zh") text="Soul 已配置（找到 soul-configured 标记），跳过 onboarding 消息" ;;
        "install.welcome_msg.soul_configured.en") text="Soul already configured (soul-configured marker found), skipping onboarding message" ;;
        "install.welcome_msg.logging_in.zh") text="正在以 %s 身份登录以发送欢迎消息..." ;;
        "install.welcome_msg.logging_in.en") text="Logging in as %s to send welcome message..." ;;
        "install.welcome_msg.login_failed.zh") text="警告: 以 %s 身份登录失败，跳过欢迎消息" ;;
        "install.welcome_msg.login_failed.en") text="WARNING: Failed to login as %s, skipping welcome message" ;;
        "install.welcome_msg.finding_room.zh") text="正在查找与 Manager 的 DM 房间..." ;;
        "install.welcome_msg.finding_room.en") text="Finding DM room with Manager..." ;;
        "install.welcome_msg.creating_room.zh") text="正在创建与 Manager 的 DM 房间..." ;;
        "install.welcome_msg.creating_room.en") text="Creating DM room with Manager..." ;;
        "install.welcome_msg.no_room.zh") text="警告: 无法找到或创建与 Manager 的 DM 房间" ;;
        "install.welcome_msg.no_room.en") text="WARNING: Could not find or create DM room with Manager" ;;
        "install.welcome_msg.waiting_join.zh") text="等待 Manager 加入房间..." ;;
        "install.welcome_msg.waiting_join.en") text="Waiting for Manager to join the room..." ;;
        "install.welcome_msg.sending.zh") text="正在向 Manager 发送欢迎消息..." ;;
        "install.welcome_msg.sending.en") text="Sending welcome message to Manager..." ;;
        "install.welcome_msg.send_failed.zh") text="警告: 发送欢迎消息失败" ;;
        "install.welcome_msg.send_failed.en") text="WARNING: Failed to send welcome message" ;;
        "install.welcome_msg.sent.zh") text="欢迎消息已发送给 Manager" ;;
        "install.welcome_msg.sent.en") text="Welcome message sent to Manager" ;;
        # --- Final output panel ---
        "success.title.zh") text="=== HiClaw Manager 已启动！===" ;;
        "success.title.en") text="=== HiClaw Manager Started! ===" ;;
        "success.domains_configured.zh") text="以下域名已配置解析到 127.0.0.1:" ;;
        "success.domains_configured.en") text="The following domains are configured to resolve to 127.0.0.1:" ;;
        "success.open_url.zh") text="  ★ 在浏览器中打开以下 URL 开始使用:                           ★" ;;
        "success.open_url.en") text="  ★ Open the following URL in your browser to start:                           ★" ;;
        "success.login_with.zh") text="  登录信息:" ;;
        "success.login_with.en") text="  Login with:" ;;
        "success.username.zh") text="    用户名: %s" ;;
        "success.username.en") text="    Username: %s" ;;
        "success.password.zh") text="    密码: %s" ;;
        "success.password.en") text="    Password: %s" ;;
        "success.after_login.zh") text="  登录后，开始与 Manager 聊天！" ;;
        "success.after_login.en") text="  After login, start chatting with the Manager!" ;;
        "success.tell_it.zh") text="    告诉它: \"创建一个名为 alice 的前端开发 Worker\"" ;;
        "success.tell_it.en") text="    Tell it: \"Create a Worker named alice for frontend dev\"" ;;
        "success.manager_auto.zh") text="    Manager 会自动处理一切。" ;;
        "success.manager_auto.en") text="    The Manager will handle everything automatically." ;;
        "success.mobile_title.zh") text="  📱 移动端访问（FluffyChat / Element Mobile）:" ;;
        "success.mobile_title.en") text="  📱 Mobile access (FluffyChat / Element Mobile):" ;;
        "success.mobile_step1.zh") text="    1. 在手机上下载 FluffyChat 或 Element" ;;
        "success.mobile_step1.en") text="    1. Download FluffyChat or Element on your phone" ;;
        "success.mobile_step2.zh") text="    2. 设置 homeserver 为: %s" ;;
        "success.mobile_step2.en") text="    2. Set homeserver to: %s" ;;
        "success.mobile_step2_noip.zh") text="    2. 设置 homeserver 为: http://<本机局域网IP>:%s" ;;
        "success.mobile_step2_noip.en") text="    2. Set homeserver to: http://<this-machine-LAN-IP>:%s" ;;
        "success.mobile_noip_hint.zh") text="       （无法自动检测局域网 IP — 请使用 ifconfig / ip addr 查看）" ;;
        "success.mobile_noip_hint.en") text="       (Could not detect LAN IP automatically — check with: ifconfig / ip addr)" ;;
        "success.mobile_step3.zh") text="    3. 登录信息:" ;;
        "success.mobile_step3.en") text="    3. Login with:" ;;
        "success.mobile_username.zh") text="         用户名: %s" ;;
        "success.mobile_username.en") text="         Username: %s" ;;
        "success.mobile_password.zh") text="         密码: %s" ;;
        "success.mobile_password.en") text="         Password: %s" ;;
        # --- Other consoles and tips ---
        "success.other_consoles.zh") text="--- 其他控制台 ---" ;;
        "success.other_consoles.en") text="--- Other Consoles ---" ;;
        "success.higress_console.zh") text="  Higress 控制台: http://localhost:%s（用户名: %s / 密码: %s）" ;;
        "success.higress_console.en") text="  Higress Console: http://localhost:%s (Username: %s / Password: %s)" ;;
        "success.openclaw_console.zh") text="  OpenClaw 控制台（本地）: http://localhost:%s（无需登录）" ;;
        "success.openclaw_console.en") text="  OpenClaw Console (local): http://localhost:%s (no login required)" ;;
        "success.openclaw_console_gateway.zh") text="  OpenClaw 控制台（网关）: http://console-local.hiclaw.io（用户名: %s / 密码: %s）" ;;
        "success.openclaw_console_gateway.en") text="  OpenClaw Console (gateway): http://console-local.hiclaw.io (Username: %s / Password: %s)" ;;
        "success.switch_llm.title.zh") text="--- 切换 LLM 提供商 ---" ;;
        "success.switch_llm.title.en") text="--- Switch LLM Providers ---" ;;
        "success.switch_llm.hint.zh") text="  您可以通过 Higress 控制台切换到其他 LLM 提供商（OpenAI、Anthropic 等）。" ;;
        "success.switch_llm.hint.en") text="  You can switch to other LLM providers (OpenAI, Anthropic, etc.) via Higress Console." ;;
        "success.switch_llm.docs.zh") text="  详细说明请参阅:" ;;
        "success.switch_llm.docs.en") text="  For detailed instructions, see:" ;;
        "success.switch_llm.url.zh") text="  https://higress.ai/en/docs/ai/scene-guide/multi-proxy#console-configuration" ;;
        "success.switch_llm.url.en") text="  https://higress.ai/en/docs/ai/scene-guide/multi-proxy#console-configuration" ;;
        "success.tip.zh") text="提示: 您也可以在聊天中让 Manager 为您配置 LLM 提供商。" ;;
        "success.tip.en") text="Tip: You can also ask the Manager to configure LLM providers for you in the chat." ;;
        "success.config_file.zh") text="配置文件: %s" ;;
        "success.config_file.en") text="Configuration file: %s" ;;
        "success.data_volume.zh") text="数据卷:        %s" ;;
        "success.data_volume.en") text="Data volume:        %s" ;;
        "success.workspace.zh") text="Manager 工作空间:  %s" ;;
        "success.workspace.en") text="Manager workspace:  %s" ;;
        # --- Worker installation ---
        "worker.resetting.zh") text="正在重置 Worker: %s..." ;;
        "worker.resetting.en") text="Resetting Worker: %s..." ;;
        "worker.exists.zh") text="容器 '%s' 已存在。使用 --reset 重新创建。" ;;
        "worker.exists.en") text="Container '%s' already exists. Use --reset to recreate." ;;
        "worker.starting.zh") text="正在启动 Worker: %s..." ;;
        "worker.starting.en") text="Starting Worker: %s..." ;;
        "worker.skills_url.zh") text="  Skills API URL: %s" ;;
        "worker.skills_url.en") text="  Skills API URL: %s" ;;
        "worker.started.zh") text="=== Worker %s 已启动！===" ;;
        "worker.started.en") text="=== Worker %s Started! ===" ;;
        "worker.container.zh") text="容器: %s" ;;
        "worker.container.en") text="Container: %s" ;;
        "worker.view_logs.zh") text="查看日志: docker logs -f %s" ;;
        "worker.view_logs.en") text="View logs: docker logs -f %s" ;;
        # --- Prompt function messages ---
        "prompt.preset.zh") text="  %s = （已通过环境变量预设）" ;;
        "prompt.preset.en") text="  %s = (pre-set via env)" ;;
        "prompt.upgrade_keep.zh") text="  %s = %s（当前值，回车保留 / 输入新值覆盖）" ;;
        "prompt.upgrade_keep.en") text="  %s = %s (current value, press Enter to keep / type new value to change)" ;;
        "prompt.upgrade_keep_secret.zh") text="  %s = %s（当前值，回车保留 / 输入新值覆盖）" ;;
        "prompt.upgrade_keep_secret.en") text="  %s = %s (current value, press Enter to keep / type new value to change)" ;;
        "prompt.upgrade_empty.zh") text="  %s = （未设置，回车跳过 / 输入新值设置）" ;;
        "prompt.upgrade_empty.en") text="  %s = (not set, press Enter to skip / type new value to set)" ;;
        "prompt.default.zh") text="  %s = %s（默认）" ;;
        "prompt.default.en") text="  %s = %s (default)" ;;
        "prompt.required.zh") text="%s 是必需的（在非交互模式下通过环境变量设置）" ;;
        "prompt.required.en") text="%s is required (set via environment variable in non-interactive mode)" ;;
        "prompt.required_empty.zh") text="%s 是必需的" ;;
        "prompt.required_empty.en") text="%s is required" ;;
        # --- Language switch prompt (bilingual by design) ---
        "lang.detected.zh") text="检测到语言 / Detected language: 中文" ;;
        "lang.detected.en") text="检测到语言 / Detected language: English" ;;
        "lang.switch_title.zh") text="切换语言 / Switch language:" ;;
        "lang.switch_title.en") text="切换语言 / Switch language:" ;;
        "lang.option_zh.zh") text="  1) 中文" ;;
        "lang.option_zh.en") text="  1) 中文" ;;
        "lang.option_en.zh") text="  2) English" ;;
        "lang.option_en.en") text="  2) English" ;;
        "lang.prompt.zh") text="请选择 / Enter choice" ;;
        "lang.prompt.en") text="请选择 / Enter choice" ;;
        # --- Error messages ---
        "error.name_required.zh") text="--name 是必需的" ;;
        "error.name_required.en") text="--name is required" ;;
        "error.fs_required.zh") text="--fs 是必需的" ;;
        "error.fs_required.en") text="--fs is required" ;;
        "error.fs_key_required.zh") text="--fs-key 是必需的" ;;
        "error.fs_key_required.en") text="--fs-key is required" ;;
        "error.fs_secret_required.zh") text="--fs-secret 是必需的" ;;
        "error.fs_secret_required.en") text="--fs-secret is required" ;;
        "error.unknown_option.zh") text="未知选项: %s" ;;
        "error.unknown_option.en") text="Unknown option: %s" ;;
        "error.docker_not_found.zh") text="未找到 docker 或 podman 命令。请先安装 Docker Desktop 或 Podman Desktop：\n  Docker Desktop: https://www.docker.com/products/docker-desktop/\n  Podman Desktop: https://podman-desktop.io/" ;;
        "error.docker_not_found.en") text="docker or podman command not found. Please install Docker Desktop or Podman Desktop first:\n  Docker Desktop: https://www.docker.com/products/docker-desktop/\n  Podman Desktop: https://podman-desktop.io/" ;;
        "error.docker_not_running.zh") text="Docker 未运行。请先启动 Docker Desktop 或 Podman Desktop。" ;;
        "error.docker_not_running.en") text="Docker is not running. Please start Docker Desktop or Podman Desktop first." ;;
        # --- Fallback: try English for unknown lang ---
        *)
            case "${key}.en" in
                "tz.warning.title.en") text="Could not detect timezone automatically." ;;
                "install.title.en") text="=== HiClaw Manager Installation ===" ;;
                *) text="${key}" ;;
            esac
            ;;
    esac
    if [ $# -gt 0 ]; then
        # shellcheck disable=SC2059
        printf "${text}\n" "$@"
    else
        echo "${text}"
    fi
}

# ============================================================
# Registry selection based on timezone
# ============================================================

detect_registry() {
    local tz="${HICLAW_TIMEZONE}"

    case "${tz}" in
        America/*)
            echo "higress-registry.us-west-1.cr.aliyuncs.com"
            ;;
        Asia/Singapore|Asia/Bangkok|Asia/Jakarta|Asia/Makassar|Asia/Jayapura|\
        Asia/Kuala_Lumpur|Asia/Ho_Chi_Minh|Asia/Manila|Asia/Yangon|\
        Asia/Vientiane|Asia/Phnom_Penh|Asia/Pontianak|Asia/Ujung_Pandang)
            echo "higress-registry.ap-southeast-7.cr.aliyuncs.com"
            ;;
        *)
            echo "higress-registry.cn-hangzhou.cr.aliyuncs.com"
            ;;
    esac
}

HICLAW_REGISTRY="${HICLAW_REGISTRY:-$(detect_registry)}"
# Image variables are resolved after version selection in step_version().
# These placeholders allow early code paths to reference them without errors.
MANAGER_IMAGE="${HICLAW_INSTALL_MANAGER_IMAGE:-}"
WORKER_IMAGE="${HICLAW_INSTALL_WORKER_IMAGE:-}"
COPAW_WORKER_IMAGE="${HICLAW_INSTALL_COPAW_WORKER_IMAGE:-}"
DOCKER_PROXY_IMAGE="${HICLAW_INSTALL_DOCKER_PROXY_IMAGE:-}"

resolve_image_tags() {
    MANAGER_IMAGE="${HICLAW_INSTALL_MANAGER_IMAGE:-${HICLAW_REGISTRY}/higress/hiclaw-manager:${HICLAW_VERSION}}"
    WORKER_IMAGE="${HICLAW_INSTALL_WORKER_IMAGE:-${HICLAW_REGISTRY}/higress/hiclaw-worker:${HICLAW_VERSION}}"
    COPAW_WORKER_IMAGE="${HICLAW_INSTALL_COPAW_WORKER_IMAGE:-${HICLAW_REGISTRY}/higress/hiclaw-copaw-worker:${HICLAW_VERSION}}"
    # docker-proxy: prefer versioned tag, fall back to :latest at pull time
    # via resolve_docker_proxy_image().
    DOCKER_PROXY_IMAGE="${HICLAW_INSTALL_DOCKER_PROXY_IMAGE:-${HICLAW_REGISTRY}/higress/hiclaw-docker-proxy:${HICLAW_VERSION}}"
}

# Resolve the docker-proxy image: try the versioned tag first; if the registry
# doesn't have it (component didn't exist yet in that release), fall back to :latest.
# Sets DOCKER_PROXY_IMAGE to the tag that will actually be pulled.
resolve_docker_proxy_image() {
    # If the user explicitly overrode the image, respect it as-is.
    [ -n "${HICLAW_INSTALL_DOCKER_PROXY_IMAGE:-}" ] && return 0

    local _versioned="${HICLAW_REGISTRY}/higress/hiclaw-docker-proxy:${HICLAW_VERSION}"
    local _latest="${HICLAW_REGISTRY}/higress/hiclaw-docker-proxy:latest"

    # Skip probe when HICLAW_VERSION is "latest" — no point trying the same tag twice.
    if [ "${HICLAW_VERSION}" = "latest" ]; then
        DOCKER_PROXY_IMAGE="${_latest}"
        return 0
    fi

    if ${DOCKER_CMD} pull "${_versioned}" >/dev/null 2>&1; then
        DOCKER_PROXY_IMAGE="${_versioned}"
    else
        log "docker-proxy ${HICLAW_VERSION} not found, using latest"
        ${DOCKER_CMD} pull "${_latest}" >/dev/null 2>&1 || true
        DOCKER_PROXY_IMAGE="${_latest}"
    fi
}

# ============================================================
# Known models list — used to detect custom models during install
# ============================================================
KNOWN_MODELS="gpt-5.4 gpt-5.3-codex gpt-5-mini gpt-5-nano claude-opus-4-6 claude-sonnet-4-6 claude-haiku-4-5 qwen3.5-plus deepseek-chat deepseek-reasoner kimi-k2.5 glm-5 MiniMax-M2.7 MiniMax-M2.7-highspeed MiniMax-M2.5"

is_known_model() {
    local model="$1"
    for m in ${KNOWN_MODELS}; do
        [ "${m}" = "${model}" ] && return 0
    done
    return 1
}

# Prompt user for custom model parameters when model is not in the known list.
# Sets: HICLAW_MODEL_CONTEXT_WINDOW, HICLAW_MODEL_MAX_TOKENS, HICLAW_MODEL_REASONING, HICLAW_MODEL_VISION
prompt_custom_model_params() {
    local model="$1"
    if is_known_model "${model}"; then
        # Clear any stale custom params for known models
        HICLAW_MODEL_CONTEXT_WINDOW=""
        HICLAW_MODEL_MAX_TOKENS=""
        HICLAW_MODEL_REASONING=""
        HICLAW_MODEL_VISION=""
        return
    fi
    echo ""
    log "$(msg llm.custom_model.detected "${model}")"
    echo ""
    read -e -p "  $(msg llm.custom_model.context_prompt): " HICLAW_MODEL_CONTEXT_WINDOW
    if [ "${HICLAW_MODEL_CONTEXT_WINDOW}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    HICLAW_MODEL_CONTEXT_WINDOW="${HICLAW_MODEL_CONTEXT_WINDOW:-150000}"
    read -e -p "  $(msg llm.custom_model.max_tokens_prompt): " HICLAW_MODEL_MAX_TOKENS
    if [ "${HICLAW_MODEL_MAX_TOKENS}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    HICLAW_MODEL_MAX_TOKENS="${HICLAW_MODEL_MAX_TOKENS:-128000}"
    read -e -p "  $(msg llm.custom_model.reasoning_prompt): " _reasoning
    if [ "${_reasoning}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    case "${_reasoning}" in
        n|N|no|NO) HICLAW_MODEL_REASONING="false" ;;
        *) HICLAW_MODEL_REASONING="true" ;;
    esac
    read -e -p "  $(msg llm.custom_model.vision_prompt): " _vision
    if [ "${_vision}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    case "${_vision}" in
        y|Y|yes|YES) HICLAW_MODEL_VISION="true" ;;
        *) HICLAW_MODEL_VISION="false" ;;
    esac
    log "$(msg llm.custom_model.summary "${HICLAW_MODEL_CONTEXT_WINDOW}" "${HICLAW_MODEL_MAX_TOKENS}" "${HICLAW_MODEL_REASONING}" "${HICLAW_MODEL_VISION}")"
}

# ============================================================
# Wait for Manager agent to be ready
# Uses `openclaw gateway health` inside the container to confirm the gateway is running
# ============================================================

wait_manager_ready() {
    local timeout="${HICLAW_READY_TIMEOUT:-300}"
    local elapsed=0
    local container="${1:-hiclaw-manager}"

    log "$(msg install.wait_ready "${timeout}")"

    # Wait for OpenClaw gateway to be healthy inside the container
    while [ "${elapsed}" -lt "${timeout}" ]; do
        if ${DOCKER_CMD} exec "${container}" openclaw gateway health --json 2>/dev/null | grep -q '"ok"' 2>/dev/null; then
            log "$(msg install.wait_ready.ok)"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
        printf "\r\033[36m[HiClaw]\033[0m $(msg install.wait_ready.waiting "${elapsed}" "${timeout}")"
    done

    echo ""
    error "$(msg install.wait_ready.timeout "${timeout}" "${container}")"
}

wait_matrix_ready() {
    local timeout="${HICLAW_READY_TIMEOUT:-300}"
    local elapsed=0
    local container="${1:-hiclaw-manager}"

    log "$(msg install.wait_matrix "${timeout}")"

    while [ "${elapsed}" -lt "${timeout}" ]; do
        if ${DOCKER_CMD} exec "${container}" curl -sf http://127.0.0.1:6167/_tuwunel/server_version >/dev/null 2>&1; then
            log "$(msg install.wait_matrix.ok)"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
        printf "\r\033[36m[HiClaw]\033[0m $(msg install.wait_matrix.waiting "${elapsed}" "${timeout}")"
    done

    echo ""
    error "$(msg install.wait_matrix.timeout "${timeout}" "${container}")"
}

# In non-interactive mode, uses default or errors if required and no default.
# Usage: prompt VAR_NAME "Prompt text" "default" [true=secret]
prompt() {
    local var_name="$1"
    local prompt_text="$2"
    local default_value="$3"
    local is_secret="${4:-false}"

    # If the variable is already set in the environment, use it silently
    # In upgrade mode, show current value and let user change it
    eval "local current_value=\"\${${var_name}}\""
    if [ -n "${current_value}" ]; then
        if [ "${HICLAW_UPGRADE}" = "1" ] && [ "${HICLAW_NON_INTERACTIVE}" != "1" ]; then
            # Show masked value for secrets, full value otherwise
            local display_value="${current_value}"
            if [ "${is_secret}" = "true" ]; then
                local len=${#current_value}
                if [ "${len}" -le 8 ]; then
                    display_value="****"
                else
                    display_value="${current_value:0:4}****${current_value: -4}"
                fi
            fi
            log "$(msg prompt.upgrade_keep "${prompt_text}" "${display_value}")"
            local new_value=""
            if [ "${is_secret}" = "true" ]; then
                read -s -e -p "${prompt_text}: " new_value
                echo
            else
                read -e -p "${prompt_text}: " new_value
                if [ "${new_value}" = "b" ]; then STEP_RESULT="back"; return 1; fi
            fi
            if [ -n "${new_value}" ]; then
                eval "export ${var_name}='${new_value}'"
            fi
            return
        fi
        log "$(msg prompt.preset "${prompt_text}")"
        return
    fi

    # Non-interactive or quickstart: use default or error
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ] || [ "${HICLAW_QUICKSTART}" = "1" ]; then
        if [ -n "${default_value}" ]; then
            eval "export ${var_name}='${default_value}'"
            log "$(msg prompt.default "${prompt_text}" "${default_value}")"
            return
        elif [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
            # Only hard-error in fully non-interactive mode, not quickstart
            error "$(msg prompt.required "${prompt_text}")"
        fi
        # quickstart + no default: fall through to interactive prompt below
    fi

    if [ -n "${default_value}" ]; then
        prompt_text="${prompt_text} [${default_value}]"
    fi

    local value=""
    if [ "${is_secret}" = "true" ]; then
        read -s -e -p "${prompt_text}: " value
        echo
    else
        read -e -p "${prompt_text}: " value
        if [ "${value}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    fi

    value="${value:-${default_value}}"
    if [ -z "${value}" ]; then
        error "$(msg prompt.required_empty "${prompt_text}")"
    fi

    eval "export ${var_name}='${value}'"
}

# Prompt for an optional value (empty string is acceptable)
# Skips prompt if variable is already defined in environment (even if empty)
# In upgrade mode, shows current value and lets user change it.
# In non-interactive mode, defaults to empty string.
prompt_optional() {
    local var_name="$1"
    local prompt_text="$2"
    local is_secret="${3:-false}"

    # Check if variable is defined (even if set to empty string)
    eval "local _chk=\"\${${var_name}+x}\""
    if [ -n "${_chk}" ]; then
        # In upgrade mode, show current value and let user change it
        if [ "${HICLAW_UPGRADE}" = "1" ] && [ "${HICLAW_NON_INTERACTIVE}" != "1" ]; then
            eval "local current_value=\"\${${var_name}}\""
            local display_value="${current_value}"
            if [ "${is_secret}" = "true" ] && [ -n "${current_value}" ]; then
                local len=${#current_value}
                if [ "${len}" -le 8 ]; then
                    display_value="****"
                else
                    display_value="${current_value:0:4}****${current_value: -4}"
                fi
            fi
            if [ -n "${current_value}" ]; then
                if [ "${is_secret}" = "true" ]; then
                    log "$(msg prompt.upgrade_keep_secret "${prompt_text}" "${display_value}")"
                else
                    log "$(msg prompt.upgrade_keep "${prompt_text}" "${display_value}")"
                fi
            else
                log "$(msg prompt.upgrade_empty "${prompt_text}")"
            fi
            local new_value=""
            if [ "${is_secret}" = "true" ]; then
                read -s -e -p "${prompt_text}: " new_value
                echo
            else
                read -e -p "${prompt_text}: " new_value
                if [ "${new_value}" = "b" ]; then STEP_RESULT="back"; return 1; fi
            fi
            if [ -n "${new_value}" ]; then
                eval "export ${var_name}='${new_value}'"
            fi
            return
        fi
        log "$(msg prompt.preset "${prompt_text}")"
        return
    fi

    # Non-interactive or quickstart: skip, leave unset
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ] || [ "${HICLAW_QUICKSTART}" = "1" ]; then
        eval "export ${var_name}=''"
        return
    fi

    local value=""
    if [ "${is_secret}" = "true" ]; then
        read -s -e -p "${prompt_text}: " value
        echo
    else
        read -e -p "${prompt_text}: " value
        if [ "${value}" = "b" ]; then STEP_RESULT="back"; return 1; fi
    fi

    eval "export ${var_name}='${value}'"
}

generate_key() {
    openssl rand -hex 32
}

# Detect container runtime socket on the host
detect_socket() {
    # Check for Podman
    if [ -S "/run/podman/podman.sock" ]; then
        echo "/run/podman/podman.sock"
    # Check for standard Docker socket
    elif [ -S "/var/run/docker.sock" ]; then
        echo "/var/run/docker.sock"
    else
        # Try to get socket from current active docker context, Example: OrbStack returns "unix:///Users/xxx/.orbstack/run/docker.sock"
        # Note: docker context ls outputs empty lines for non-current contexts,
        # so we use 'grep .' to filter them out before extracting the path
        if command -v docker >/dev/null 2>&1; then
            local socket_path
            socket_path=$(docker context ls --format '{{if .Current}}{{.DockerEndpoint}}{{end}}' 2>/dev/null | grep . | sed 's|^unix://||')
            if [ -n "${socket_path}" ] && [ -S "${socket_path}" ]; then
                echo "${socket_path}"
            fi
        fi
    fi
}

# Detect local LAN IP address (cross-platform: macOS and Linux)
detect_lan_ip() {
    local ip=""

    # macOS: try common Wi-Fi / Ethernet interfaces
    if command -v ipconfig >/dev/null 2>&1; then
        for iface in en0 en1 en2 en3 en4; do
            ip=$(ipconfig getifaddr "${iface}" 2>/dev/null)
            if [ -n "${ip}" ]; then
                echo "${ip}"
                return 0
            fi
        done
    fi

    # Linux: ip route — most reliable
    if command -v ip >/dev/null 2>&1; then
        ip=$(ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')
        if [ -n "${ip}" ]; then
            echo "${ip}"
            return 0
        fi
    fi

    # Linux fallback: hostname -I (space-separated list, take first non-loopback)
    if command -v hostname >/dev/null 2>&1; then
        ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -v '^127\.' | grep -v '^::' | head -1)
        if [ -n "${ip}" ]; then
            echo "${ip}"
            return 0
        fi
    fi

    # Last resort: ifconfig
    if command -v ifconfig >/dev/null 2>&1; then
        ip=$(ifconfig 2>/dev/null | awk '/inet /{if($2!~/^127\./){print $2; exit}}')
        # Strip "addr:" prefix that some ifconfig versions add
        ip="${ip#addr:}"
        if [ -n "${ip}" ]; then
            echo "${ip}"
            return 0
        fi
    fi

    echo ""
}

# ============================================================
# Step-back navigation helpers
# ============================================================

# should_skip_step: returns 0 (skip) when the step is irrelevant in current mode
should_skip_step() {
    local step_fn="$1"
    case "${step_fn}" in
        step_lang|step_mode)
            [ "${HICLAW_NON_INTERACTIVE}" = "1" ] && return 0
            ;;
        step_version)
            [ "${HICLAW_NON_INTERACTIVE}" = "1" ] && return 0
            ;;
        step_existing)
            local _env="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
            [ ! -f "${_env}" ] && return 0
            ;;
        step_volume|step_workspace)
            [ "${HICLAW_NON_INTERACTIVE}" = "1" ] && return 0
            [ "${HICLAW_QUICKSTART}" = "1" ] && return 0
            ;;
        step_e2ee|step_idle|step_docker_proxy)
            [ "${HICLAW_NON_INTERACTIVE}" = "1" ] && return 0
            [ "${HICLAW_QUICKSTART}" = "1" ] && [ "${HICLAW_UPGRADE}" != "1" ] && return 0
            ;;
        step_hostshare)
            [ "${HICLAW_NON_INTERACTIVE}" = "1" ] && return 0
            [ "${HICLAW_QUICKSTART}" = "1" ] && return 0
            ;;
    esac
    return 1
}

# clear_step_vars: unset variables set by a step so it will re-prompt on re-entry
clear_step_vars() {
    local step_fn="$1"
    case "${step_fn}" in
        step_mode)   unset HICLAW_QUICKSTART ;;
        step_version) unset HICLAW_VERSION ;;
        step_existing) unset HICLAW_UPGRADE UPGRADE_EXISTING_WORKERS ;;
        step_llm)
            unset HICLAW_LLM_PROVIDER HICLAW_DEFAULT_MODEL HICLAW_OPENAI_BASE_URL
            unset HICLAW_LLM_API_KEY HICLAW_MODEL_CONTEXT_WINDOW HICLAW_MODEL_MAX_TOKENS
            unset HICLAW_MODEL_REASONING HICLAW_MODEL_VISION
            ;;
        step_admin)   unset HICLAW_ADMIN_USER HICLAW_ADMIN_PASSWORD ;;
        step_network) unset HICLAW_LOCAL_ONLY ;;
        step_ports)
            unset HICLAW_PORT_GATEWAY HICLAW_PORT_CONSOLE
            unset HICLAW_PORT_ELEMENT_WEB HICLAW_PORT_OPENCLAW_CONSOLE
            ;;
        step_domains)
            unset HICLAW_MATRIX_DOMAIN HICLAW_MATRIX_CLIENT_DOMAIN
            unset HICLAW_AI_GATEWAY_DOMAIN HICLAW_FS_DOMAIN HICLAW_CONSOLE_DOMAIN
            ;;
        step_github)    unset HICLAW_GITHUB_TOKEN ;;
        step_skills)    unset HICLAW_SKILLS_API_URL ;;
        step_volume)    unset HICLAW_DATA_DIR ;;
        step_workspace) unset HICLAW_WORKSPACE_DIR ;;
        step_runtime)   unset HICLAW_DEFAULT_WORKER_RUNTIME ;;
        step_e2ee)      unset HICLAW_MATRIX_E2EE ;;
        step_docker_proxy) unset HICLAW_DOCKER_PROXY; unset HICLAW_PROXY_ALLOWED_REGISTRIES ;;
        step_idle)      unset HICLAW_WORKER_IDLE_TIMEOUT ;;
        step_hostshare) unset HICLAW_HOST_SHARE_DIR ;;
    esac
}

# ============================================================
# Individual step functions
# ============================================================

step_lang() {
    local lang_default_choice="2"
    [ "${HICLAW_LANGUAGE}" = "zh" ] && lang_default_choice="1"
    log "$(msg lang.detected)"
    log "$(msg lang.switch_title)"
    echo "$(msg lang.option_zh)"
    echo "$(msg lang.option_en)"
    echo ""
    local LANG_CHOICE
    read -e -p "$(msg lang.prompt) [${lang_default_choice}]: " LANG_CHOICE
    LANG_CHOICE="${LANG_CHOICE:-${lang_default_choice}}"
    if [ "${LANG_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    case "${LANG_CHOICE}" in
        1) HICLAW_LANGUAGE="zh" ;;
        2) HICLAW_LANGUAGE="en" ;;
    esac
    export HICLAW_LANGUAGE
    log ""
}

step_mode() {
    log "$(msg install.mode.title)"
    echo ""
    echo "$(msg install.mode.choose)"
    echo "$(msg install.mode.quickstart)"
    echo "$(msg install.mode.manual)"
    echo ""
    local ONBOARDING_CHOICE
    read -e -p "$(msg install.mode.prompt): " ONBOARDING_CHOICE
    ONBOARDING_CHOICE="${ONBOARDING_CHOICE:-1}"
    if [ "${ONBOARDING_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    case "${ONBOARDING_CHOICE}" in
        1|quick|quickstart)
            log "$(msg install.mode.quickstart_selected)"
            HICLAW_QUICKSTART=1
            ;;
        2|manual)
            log "$(msg install.mode.manual_selected)"
            ;;
        *)
            log "$(msg install.mode.invalid)"
            HICLAW_QUICKSTART=1
            ;;
    esac
    log ""
}

step_version() {
    # Skip if version already provided via env var
    if [ -n "${HICLAW_VERSION}" ]; then
        resolve_image_tags
        return 0
    fi
    # Try to fetch the latest stable release from GitHub
    log "$(msg install.version.fetching)"
    local _fetched
    _fetched=$(curl -sf --max-time 5 \
        -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/alibaba/hiclaw/releases/latest" \
        2>/dev/null | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
    if [ -n "${_fetched}" ]; then
        HICLAW_KNOWN_STABLE_VERSION="${_fetched}"
    else
        log "$(msg install.version.fetch_failed "${HICLAW_KNOWN_STABLE_VERSION}")"
    fi
    log "$(msg install.version.title)"
    echo ""
    echo "$(msg install.version.choose)"
    echo "$(msg install.version.option_latest)"
    printf "%s\n" "$(msg install.version.option_stable "${HICLAW_KNOWN_STABLE_VERSION}")"
    echo "$(msg install.version.option_custom)"
    echo ""
    local VERSION_CHOICE
    read -e -p "$(msg install.version.prompt) [1]: " VERSION_CHOICE
    VERSION_CHOICE="${VERSION_CHOICE:-1}"
    if [ "${VERSION_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    case "${VERSION_CHOICE}" in
        1|latest)
            HICLAW_VERSION="latest"
            log "$(msg install.version.selected_latest)"
            ;;
        2|stable)
            HICLAW_VERSION="${HICLAW_KNOWN_STABLE_VERSION}"
            log "$(msg install.version.selected_stable "${HICLAW_VERSION}")"
            ;;
        3|custom)
            local CUSTOM_VERSION
            read -e -p "$(msg install.version.custom_prompt): " CUSTOM_VERSION
            HICLAW_VERSION="${CUSTOM_VERSION:-${HICLAW_KNOWN_STABLE_VERSION}}"
            log "$(msg install.version.selected_custom "${HICLAW_VERSION}")"
            ;;
        *)
            HICLAW_VERSION="${HICLAW_KNOWN_STABLE_VERSION}"
            log "$(msg install.version.invalid "${HICLAW_VERSION}")"
            ;;
    esac
    log ""
    resolve_image_tags
}

step_existing() {
    local existing_env="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
    log "$(msg install.existing.detected "${existing_env}")"
    local running_manager="" running_workers="" existing_workers=""
    if ${DOCKER_CMD} ps --format '{{.Names}}' | grep -q "^hiclaw-manager$"; then
        running_manager="hiclaw-manager"
    fi
    running_workers=$(${DOCKER_CMD} ps --format '{{.Names}}' | grep "^hiclaw-worker-" || true)
    existing_workers=$(${DOCKER_CMD} ps -a --format '{{.Names}}' | grep "^hiclaw-worker-" || true)
    local UPGRADE_CHOICE
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
        log "$(msg install.existing.upgrade_noninteractive)"
        UPGRADE_CHOICE="upgrade"
    else
        echo ""
        echo "$(msg install.existing.choose)"
        echo "$(msg install.existing.upgrade)"
        echo "$(msg install.existing.reinstall)"
        echo "$(msg install.existing.cancel)"
        echo ""
        read -e -p "$(msg install.existing.prompt): " UPGRADE_CHOICE
        UPGRADE_CHOICE="${UPGRADE_CHOICE:-1}"
        if [ "${UPGRADE_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    fi
    case "${UPGRADE_CHOICE}" in
        1|upgrade)
            HICLAW_UPGRADE=1
            log "$(msg install.existing.upgrading)"
            if [ -n "${running_manager}" ] || [ -n "${running_workers}" ]; then
                echo ""
                echo -e "\033[33m$(msg install.existing.warn_manager_stop)\033[0m"
                if [ -n "${existing_workers}" ]; then
                    echo -e "\033[33m$(msg install.existing.warn_worker_recreate)\033[0m"
                fi
                if [ "${HICLAW_NON_INTERACTIVE}" != "1" ]; then
                    echo ""
                    local CONFIRM_STOP
                    read -e -p "$(msg install.existing.continue_prompt): " CONFIRM_STOP
                    if [ "${CONFIRM_STOP}" = "b" ]; then STEP_RESULT="back"; return 0; fi
                    if [ "${CONFIRM_STOP}" != "y" ] && [ "${CONFIRM_STOP}" != "Y" ]; then
                        log "$(msg install.existing.cancelled)"
                        exit 0
                    fi
                fi
            fi
            UPGRADE_EXISTING_WORKERS="${existing_workers}"
            ;;
        2|reinstall)
            log "$(msg install.reinstall.performing)"
            local existing_workspace=""
            if [ -f "${existing_env}" ]; then
                existing_workspace=$(grep '^HICLAW_WORKSPACE_DIR=' "${existing_env}" 2>/dev/null | cut -d= -f2-)
            fi
            [ -z "${existing_workspace}" ] && existing_workspace="${HOME}/hiclaw-manager"
            echo ""
            echo -e "\033[33m$(msg install.reinstall.warn_stop)\033[0m"
            [ -n "${running_manager}" ] && echo -e "\033[33m   - ${running_manager} (manager)\033[0m"
            for w in ${running_workers}; do
                echo -e "\033[33m   - ${w} (worker)\033[0m"
            done
            echo ""
            echo -e "\033[31m$(msg install.reinstall.warn_delete)\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_volume)\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_env "${existing_env}")\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_workspace "${existing_workspace}")\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_workers)\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_proxy)\033[0m"
            echo -e "\033[31m$(msg install.reinstall.warn_network)\033[0m"
            echo ""
            echo -e "\033[31m$(msg install.reinstall.confirm_type)\033[0m"
            echo -e "\033[31m  ${existing_workspace}\033[0m"
            echo ""
            local CONFIRM_PATH
            read -e -p "$(msg install.reinstall.confirm_path): " CONFIRM_PATH
            if [ "${CONFIRM_PATH}" != "${existing_workspace}" ]; then
                error "$(msg install.reinstall.path_mismatch "${CONFIRM_PATH}" "${existing_workspace}")"
            fi
            log "$(msg install.reinstall.confirmed)"
            ${DOCKER_CMD} stop hiclaw-manager 2>/dev/null || true
            ${DOCKER_CMD} rm hiclaw-manager 2>/dev/null || true
            for w in $(${DOCKER_CMD} ps -a --format '{{.Names}}' | grep "^hiclaw-worker-" || true); do
                ${DOCKER_CMD} stop "${w}" 2>/dev/null || true
                ${DOCKER_CMD} rm "${w}" 2>/dev/null || true
                log "$(msg install.reinstall.removed_worker "${w}")"
            done
            if ${DOCKER_CMD} ps -a --format '{{.Names}}' | grep -q "^hiclaw-docker-proxy$"; then
                log "$(msg install.reinstall.removing_proxy)"
                ${DOCKER_CMD} stop hiclaw-docker-proxy 2>/dev/null || true
                ${DOCKER_CMD} rm hiclaw-docker-proxy 2>/dev/null || true
            fi
            if ${DOCKER_CMD} network ls --format '{{.Name}}' | grep -q "^hiclaw-net$"; then
                log "$(msg install.reinstall.removing_network)"
                ${DOCKER_CMD} network rm hiclaw-net 2>/dev/null || true
            fi
            if ${DOCKER_CMD} volume ls -q | grep -q "^hiclaw-data$"; then
                log "$(msg install.reinstall.removing_volume)"
                ${DOCKER_CMD} volume rm hiclaw-data 2>/dev/null || log "$(msg install.reinstall.warn_volume_fail)"
            fi
            if [ -d "${existing_workspace}" ]; then
                log "$(msg install.reinstall.removing_workspace "${existing_workspace}")"
                rm -rf "${existing_workspace}" || error "$(msg install.reinstall.failed_rm_workspace)"
            fi
            if [ -f "${existing_env}" ]; then
                log "$(msg install.reinstall.removing_env "${existing_env}")"
                rm -f "${existing_env}"
            fi
            log "$(msg install.reinstall.cleanup_done)"
            unset HICLAW_WORKSPACE_DIR
            return 0
            ;;
        3|cancel|*)
            log "$(msg install.existing.cancelled)"
            exit 0
            ;;
    esac
    # Load existing env file as fallback (shell env vars take priority)
    if [ -f "${existing_env}" ]; then
        log "$(msg install.loading_config "${existing_env}")"
        while IFS='=' read -r key value; do
            case "${key}" in \#*|"") continue ;; esac
            value="${value%%#*}"
            value="${value#"${value%%[![:space:]]*}"}"
            value="${value%"${value##*[![:space:]]}"}"
            eval "_existing_val=\"\${${key}+x}\""
            if [ -z "${_existing_val}" ]; then export "${key}=${value}"; fi
        done < "${existing_env}"
    fi
}

step_llm() {
    log "$(msg llm.title)"
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
        HICLAW_LLM_PROVIDER="${HICLAW_LLM_PROVIDER:-qwen}"
        HICLAW_DEFAULT_MODEL="${HICLAW_DEFAULT_MODEL:-qwen3.5-plus}"
        log "$(msg llm.provider.qwen_default "${HICLAW_LLM_PROVIDER}")"
        log "$(msg llm.model.default "${HICLAW_DEFAULT_MODEL}")"
        prompt HICLAW_LLM_API_KEY "$(msg llm.apikey_prompt)" "" "true"
        return 0
    fi
    echo ""
    echo "$(msg llm.providers_title)"
    echo "$(msg llm.provider.alibaba)"
    echo "$(msg llm.provider.openai_compat)"
    echo ""
    local PROVIDER_CHOICE
    if [ "${HICLAW_QUICKSTART}" = "1" ]; then
        read -e -p "$(msg llm.provider.select) [1]: " PROVIDER_CHOICE
        PROVIDER_CHOICE="${PROVIDER_CHOICE:-1}"
    else
        read -e -p "$(msg llm.provider.select): " PROVIDER_CHOICE
        PROVIDER_CHOICE="${PROVIDER_CHOICE:-1}"
    fi
    if [ "${PROVIDER_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    local ALIBABA_MODEL_CHOICE=""
    case "${PROVIDER_CHOICE}" in
        1|alibaba-cloud)
            if [ "${HICLAW_LANGUAGE}" = "en" ]; then
                HICLAW_LLM_PROVIDER="openai-compat"
                HICLAW_OPENAI_BASE_URL="https://coding-intl.dashscope.aliyuncs.com/v1"
                ALIBABA_MODEL_CHOICE="codingplan"
                echo ""
                echo "$(msg llm.codingplan.models_title)"
                echo "$(msg llm.codingplan.model.qwen35plus)"
                echo "$(msg llm.codingplan.model.glm5)"
                echo "$(msg llm.codingplan.model.kimi)"
                echo "$(msg llm.codingplan.model.minimax)"
                echo ""
                local CODINGPLAN_MODEL_CHOICE
                if [ "${HICLAW_QUICKSTART}" = "1" ]; then
                    read -e -p "$(msg llm.codingplan.model.select) [1]: " CODINGPLAN_MODEL_CHOICE
                    CODINGPLAN_MODEL_CHOICE="${CODINGPLAN_MODEL_CHOICE:-1}"
                else
                    read -e -p "$(msg llm.codingplan.model.select): " CODINGPLAN_MODEL_CHOICE
                    CODINGPLAN_MODEL_CHOICE="${CODINGPLAN_MODEL_CHOICE:-1}"
                fi
                if [ "${CODINGPLAN_MODEL_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
                case "${CODINGPLAN_MODEL_CHOICE}" in
                    1|qwen3.5-plus) HICLAW_DEFAULT_MODEL="qwen3.5-plus" ;;
                    2|glm-5)        HICLAW_DEFAULT_MODEL="glm-5" ;;
                    3|kimi-k2.5)    HICLAW_DEFAULT_MODEL="kimi-k2.5" ;;
                    4|MiniMax-M2.5) HICLAW_DEFAULT_MODEL="MiniMax-M2.5" ;;
                    *)              HICLAW_DEFAULT_MODEL="qwen3.5-plus" ;;
                esac
                log "$(msg llm.provider.selected_codingplan)"
                log "$(msg llm.model.label "${HICLAW_DEFAULT_MODEL}")"
            else
                echo ""
                echo "$(msg llm.alibaba.models_title)"
                echo "$(msg llm.alibaba.model.codingplan)"
                echo "$(msg llm.alibaba.model.qwen)"
                echo ""
                if [ "${HICLAW_QUICKSTART}" = "1" ]; then
                    read -e -p "$(msg llm.alibaba.model.select) [1]: " ALIBABA_MODEL_CHOICE
                    ALIBABA_MODEL_CHOICE="${ALIBABA_MODEL_CHOICE:-1}"
                else
                    read -e -p "$(msg llm.alibaba.model.select): " ALIBABA_MODEL_CHOICE
                    ALIBABA_MODEL_CHOICE="${ALIBABA_MODEL_CHOICE:-1}"
                fi
                if [ "${ALIBABA_MODEL_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
                case "${ALIBABA_MODEL_CHOICE}" in
                    2|qwen)
                        HICLAW_LLM_PROVIDER="qwen"
                        HICLAW_OPENAI_BASE_URL=""
                        echo ""
                        read -e -p "$(msg llm.qwen.model_prompt): " HICLAW_DEFAULT_MODEL
                        if [ "${HICLAW_DEFAULT_MODEL}" = "b" ]; then STEP_RESULT="back"; return 0; fi
                        HICLAW_DEFAULT_MODEL="${HICLAW_DEFAULT_MODEL:-qwen3.5-plus}"
                        log "$(msg llm.provider.selected_qwen)"
                        log "$(msg llm.model.label "${HICLAW_DEFAULT_MODEL}")"
                        prompt_custom_model_params "${HICLAW_DEFAULT_MODEL}" || return 0
                        ;;
                    *)
                        HICLAW_LLM_PROVIDER="openai-compat"
                        HICLAW_OPENAI_BASE_URL="https://coding.dashscope.aliyuncs.com/v1"
                        echo ""
                        echo "$(msg llm.codingplan.models_title)"
                        echo "$(msg llm.codingplan.model.qwen35plus)"
                        echo "$(msg llm.codingplan.model.glm5)"
                        echo "$(msg llm.codingplan.model.kimi)"
                        echo "$(msg llm.codingplan.model.minimax)"
                        echo ""
                        local CODINGPLAN_MODEL_CHOICE
                        if [ "${HICLAW_QUICKSTART}" = "1" ]; then
                            read -e -p "$(msg llm.codingplan.model.select) [1]: " CODINGPLAN_MODEL_CHOICE
                            CODINGPLAN_MODEL_CHOICE="${CODINGPLAN_MODEL_CHOICE:-1}"
                        else
                            read -e -p "$(msg llm.codingplan.model.select): " CODINGPLAN_MODEL_CHOICE
                            CODINGPLAN_MODEL_CHOICE="${CODINGPLAN_MODEL_CHOICE:-1}"
                        fi
                        if [ "${CODINGPLAN_MODEL_CHOICE}" = "b" ]; then STEP_RESULT="back"; return 0; fi
                        case "${CODINGPLAN_MODEL_CHOICE}" in
                            1|qwen3.5-plus) HICLAW_DEFAULT_MODEL="qwen3.5-plus" ;;
                            2|glm-5)        HICLAW_DEFAULT_MODEL="glm-5" ;;
                            3|kimi-k2.5)    HICLAW_DEFAULT_MODEL="kimi-k2.5" ;;
                            4|MiniMax-M2.5) HICLAW_DEFAULT_MODEL="MiniMax-M2.5" ;;
                            *)              HICLAW_DEFAULT_MODEL="qwen3.5-plus" ;;
                        esac
                        log "$(msg llm.provider.selected_codingplan)"
                        log "$(msg llm.model.label "${HICLAW_DEFAULT_MODEL}")"
                        ;;
                esac
            fi
            log ""
            log "$(msg llm.apikey_hint)"
            log "$(msg llm.apikey_url)"
            log ""
            prompt HICLAW_LLM_API_KEY "$(msg llm.apikey_prompt)" "" "true" || return 0
            if [ "${ALIBABA_MODEL_CHOICE}" = "2" ] || [ "${ALIBABA_MODEL_CHOICE}" = "qwen" ]; then
                test_llm_connectivity "https://dashscope.aliyuncs.com/compatible-mode/v1" "${HICLAW_LLM_API_KEY}" "${HICLAW_DEFAULT_MODEL}" || return 0
            else
                test_llm_connectivity "${HICLAW_OPENAI_BASE_URL}" "${HICLAW_LLM_API_KEY}" "${HICLAW_DEFAULT_MODEL}" "$(msg llm.openai.test.fail.codingplan)" || return 0
            fi
            ;;
        2|openai-compat)
            HICLAW_LLM_PROVIDER="openai-compat"
            log "$(msg llm.provider.selected_openai "${HICLAW_LLM_PROVIDER}")"
            echo ""
            read -e -p "$(msg llm.openai.base_url_prompt): " HICLAW_OPENAI_BASE_URL
            if [ "${HICLAW_OPENAI_BASE_URL}" = "b" ]; then STEP_RESULT="back"; return 0; fi
            read -e -p "$(msg llm.openai.model_prompt): " HICLAW_DEFAULT_MODEL
            if [ "${HICLAW_DEFAULT_MODEL}" = "b" ]; then STEP_RESULT="back"; return 0; fi
            HICLAW_DEFAULT_MODEL="${HICLAW_DEFAULT_MODEL:-gpt-5.4}"
            log "$(msg llm.openai.base_url_label "${HICLAW_OPENAI_BASE_URL}")"
            log "$(msg llm.model.label "${HICLAW_DEFAULT_MODEL}")"
            prompt_custom_model_params "${HICLAW_DEFAULT_MODEL}" || return 0
            log ""
            prompt HICLAW_LLM_API_KEY "$(msg llm.apikey_prompt)" "" "true" || return 0
            test_llm_connectivity "${HICLAW_OPENAI_BASE_URL}" "${HICLAW_LLM_API_KEY}" "${HICLAW_DEFAULT_MODEL}" || return 0
            ;;
        *)
            error "$(msg llm.provider.invalid "${PROVIDER_CHOICE}")"
            ;;
    esac
    export HICLAW_LLM_PROVIDER HICLAW_DEFAULT_MODEL
    [ -n "${HICLAW_OPENAI_BASE_URL+x}" ] && export HICLAW_OPENAI_BASE_URL
    log ""
}

step_admin() {
    log "$(msg admin.title)"
    prompt HICLAW_ADMIN_USER "$(msg admin.username_prompt)" "admin" || return 0
    if [ -z "${HICLAW_ADMIN_PASSWORD}" ]; then
        prompt_optional HICLAW_ADMIN_PASSWORD "$(msg admin.password_prompt)" "true" || return 0
        if [ -z "${HICLAW_ADMIN_PASSWORD}" ]; then
            HICLAW_ADMIN_PASSWORD="admin$(openssl rand -hex 6)"
            log "$(msg admin.password_generated)"
        fi
    else
        log "  $(msg prompt.preset "$(msg admin.password_prompt)")"
    fi
    if [ ${#HICLAW_ADMIN_PASSWORD} -lt 8 ]; then
        error "$(msg admin.password_too_short "${#HICLAW_ADMIN_PASSWORD}")"
    fi
    log ""
}

step_network() {
    log "$(msg port.local_only.title)"
    echo ""
    echo "  1) $(msg port.local_only.hint_yes)"
    echo "  2) $(msg port.local_only.hint_no)"
    echo ""
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
        HICLAW_LOCAL_ONLY="${HICLAW_LOCAL_ONLY:-1}"
    elif [ -z "${HICLAW_LOCAL_ONLY+x}" ]; then
        local _local_choice
        read -e -p "$(msg port.local_only.choice): " _local_choice
        if [ "${_local_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        _local_choice="${_local_choice:-1}"
        case "${_local_choice}" in
            2|n|N|no|NO) HICLAW_LOCAL_ONLY="0" ;;
            *)            HICLAW_LOCAL_ONLY="1" ;;
        esac
    fi
    export HICLAW_LOCAL_ONLY
    if [ "${HICLAW_LOCAL_ONLY}" = "1" ]; then
        log "$(msg port.local_only.selected_local)"
    else
        log "$(msg port.local_only.selected_external)"
        echo ""
        echo -e "\033[33m$(msg port.local_only.https_hint)\033[0m"
    fi
}

step_ports() {
    log "$(msg port.title)"
    prompt HICLAW_PORT_GATEWAY "$(msg port.gateway_prompt)" "18080" || return 0
    prompt HICLAW_PORT_CONSOLE "$(msg port.console_prompt)" "18001" || return 0
    prompt HICLAW_PORT_ELEMENT_WEB "$(msg port.element_prompt)" "18088" || return 0
    prompt HICLAW_PORT_OPENCLAW_CONSOLE "$(msg port.openclaw_console_prompt)" "18888" || return 0
    log ""
}

step_domains() {
    log "$(msg domain.title)"
    log "$(msg domain.hint)"
    prompt HICLAW_MATRIX_DOMAIN "$(msg domain.matrix_prompt)" "matrix-local.hiclaw.io:${HICLAW_PORT_GATEWAY}" || return 0
    prompt HICLAW_MATRIX_CLIENT_DOMAIN "$(msg domain.element_prompt)" "matrix-client-local.hiclaw.io" || return 0
    prompt HICLAW_AI_GATEWAY_DOMAIN "$(msg domain.gateway_prompt)" "aigw-local.hiclaw.io" || return 0
    prompt HICLAW_FS_DOMAIN "$(msg domain.fs_prompt)" "fs-local.hiclaw.io" || return 0
    prompt HICLAW_CONSOLE_DOMAIN "$(msg domain.console_prompt)" "console-local.hiclaw.io" || return 0
    log ""
}

step_github() {
    log "$(msg github.title)"
    prompt_optional HICLAW_GITHUB_TOKEN "$(msg github.token_prompt)" "true" || return 0
}

step_skills() {
    log ""
    log "$(msg skills.title)"
    prompt_optional HICLAW_SKILLS_API_URL "$(msg skills.url_prompt)" || return 0
    log ""
}

step_volume() {
    log "$(msg data.title)"
    if [ -z "${HICLAW_DATA_DIR+x}" ]; then
        local _input
        read -e -p "$(msg data.volume_prompt): " _input
        if [ "${_input}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        HICLAW_DATA_DIR="${_input:-hiclaw-data}"
        export HICLAW_DATA_DIR
    fi
    HICLAW_DATA_DIR="${HICLAW_DATA_DIR:-hiclaw-data}"
    log "$(msg data.volume_using "${HICLAW_DATA_DIR}")"
}

step_workspace() {
    log "$(msg workspace.title)"
    if [ -z "${HICLAW_WORKSPACE_DIR+x}" ]; then
        local _input
        read -e -p "$(msg workspace.dir_prompt "${HOME}/hiclaw-manager"): " _input
        if [ "${_input}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        HICLAW_WORKSPACE_DIR="${_input:-${HOME}/hiclaw-manager}"
        export HICLAW_WORKSPACE_DIR
    fi
    HICLAW_WORKSPACE_DIR="$(cd "${HICLAW_WORKSPACE_DIR}" 2>/dev/null && pwd || echo "${HICLAW_WORKSPACE_DIR}")"
    mkdir -p "${HICLAW_WORKSPACE_DIR}"
    log "$(msg workspace.dir_label "${HICLAW_WORKSPACE_DIR}")"
}

step_runtime() {
    log "$(msg worker_runtime.title)"
    echo ""
    echo "  1) $(msg worker_runtime.openclaw)"
    echo "  2) $(msg worker_runtime.copaw)"
    echo ""
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
        HICLAW_DEFAULT_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"
    elif [ "${HICLAW_UPGRADE}" = "1" ] && [ -n "${HICLAW_DEFAULT_WORKER_RUNTIME}" ]; then
        log "$(msg prompt.upgrade_keep "$(msg worker_runtime.title_short)" "${HICLAW_DEFAULT_WORKER_RUNTIME}")"
        local _runtime_choice
        read -e -p "$(msg worker_runtime.choice): " _runtime_choice
        if [ "${_runtime_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        if [ -n "${_runtime_choice}" ]; then
            case "${_runtime_choice}" in
                2) HICLAW_DEFAULT_WORKER_RUNTIME="copaw" ;;
                *) HICLAW_DEFAULT_WORKER_RUNTIME="openclaw" ;;
            esac
        fi
    elif [ -z "${HICLAW_DEFAULT_WORKER_RUNTIME+x}" ]; then
        local _runtime_choice
        read -e -p "$(msg worker_runtime.choice): " _runtime_choice
        if [ "${_runtime_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        _runtime_choice="${_runtime_choice:-1}"
        case "${_runtime_choice}" in
            2) HICLAW_DEFAULT_WORKER_RUNTIME="copaw" ;;
            *) HICLAW_DEFAULT_WORKER_RUNTIME="openclaw" ;;
        esac
    fi
    export HICLAW_DEFAULT_WORKER_RUNTIME
    log "$(msg worker_runtime.selected "${HICLAW_DEFAULT_WORKER_RUNTIME}")"
}

step_e2ee() {
    log ""
    log "$(msg matrix_e2ee.title)"
    echo ""
    echo -e "  $(msg matrix_e2ee.desc)"
    echo ""
    echo "  1) $(msg matrix_e2ee.disable)"
    echo "  2) $(msg matrix_e2ee.enable)"
    echo ""
    if [ "${HICLAW_UPGRADE}" = "1" ] && [ -n "${HICLAW_MATRIX_E2EE}" ]; then
        local _e2ee_display; if [ "${HICLAW_MATRIX_E2EE}" = "1" ]; then _e2ee_display="$(msg matrix_e2ee.val_enabled)"; else _e2ee_display="$(msg matrix_e2ee.val_disabled)"; fi
        log "$(msg prompt.upgrade_keep "$(msg matrix_e2ee.title_short)" "${_e2ee_display}")"
        local _e2ee_choice
        read -e -p "$(msg matrix_e2ee.choice): " _e2ee_choice
        if [ "${_e2ee_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        if [ -n "${_e2ee_choice}" ]; then
            case "${_e2ee_choice}" in
                2) HICLAW_MATRIX_E2EE="1" ;;
                *) HICLAW_MATRIX_E2EE="0" ;;
            esac
        fi
    elif [ -z "${HICLAW_MATRIX_E2EE+x}" ]; then
        local _e2ee_choice
        read -e -p "$(msg matrix_e2ee.choice): " _e2ee_choice
        if [ "${_e2ee_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        _e2ee_choice="${_e2ee_choice:-1}"
        case "${_e2ee_choice}" in
            2) HICLAW_MATRIX_E2EE="1" ;;
            *) HICLAW_MATRIX_E2EE="0" ;;
        esac
    fi
    HICLAW_MATRIX_E2EE="${HICLAW_MATRIX_E2EE:-0}"
    export HICLAW_MATRIX_E2EE
    if [ "${HICLAW_MATRIX_E2EE}" = "1" ]; then
        log "$(msg matrix_e2ee.selected_enabled)"
    else
        log "$(msg matrix_e2ee.selected_disabled)"
    fi
}

step_docker_proxy() {
    # Only relevant when socket mounting is enabled
    if [ "${HICLAW_MOUNT_SOCKET}" != "1" ]; then
        HICLAW_DOCKER_PROXY="0"
        return 0
    fi

    echo ""
    echo -e "  \033[1m$(msg docker_proxy.title)\033[0m"
    echo ""
    echo -e "  $(msg docker_proxy.desc)"
    echo ""
    echo "  1) $(msg docker_proxy.enable)"
    echo "  2) $(msg docker_proxy.disable)"
    echo ""

    if [ "${HICLAW_UPGRADE}" = "1" ] && [ -n "${HICLAW_DOCKER_PROXY}" ]; then
        local _proxy_display; if [ "${HICLAW_DOCKER_PROXY}" = "1" ]; then _proxy_display="$(msg docker_proxy.val_enabled)"; else _proxy_display="$(msg docker_proxy.val_disabled)"; fi
        log "$(msg prompt.upgrade_keep "$(msg docker_proxy.title_short)" "${_proxy_display}")"
        local _choice
        read -e -p "$(msg docker_proxy.choice): " _choice
        if [ "${_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        if [ -n "${_choice}" ]; then
            case "${_choice}" in
                2) HICLAW_DOCKER_PROXY="0" ;;
                *) HICLAW_DOCKER_PROXY="1" ;;
            esac
        fi
    elif [ -z "${HICLAW_DOCKER_PROXY+x}" ]; then
        local _choice
        read -e -p "$(msg docker_proxy.choice): " _choice
        if [ "${_choice}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        _choice="${_choice:-1}"
        case "${_choice}" in
            2) HICLAW_DOCKER_PROXY="0" ;;
            *) HICLAW_DOCKER_PROXY="1" ;;
        esac
    fi
    HICLAW_DOCKER_PROXY="${HICLAW_DOCKER_PROXY:-1}"
    export HICLAW_DOCKER_PROXY
    if [ "${HICLAW_DOCKER_PROXY}" = "1" ]; then
        log "$(msg docker_proxy.selected_enabled)"

        # Prompt for additional allowed image sources
        echo ""
        echo -e "  $(msg docker_proxy.registries_desc)"
        echo ""
        if [ "${HICLAW_UPGRADE}" = "1" ] && [ -n "${HICLAW_PROXY_ALLOWED_REGISTRIES}" ]; then
            log "$(msg prompt.upgrade_keep "$(msg docker_proxy.registries_label)" "${HICLAW_PROXY_ALLOWED_REGISTRIES}")"
            local _reg_input
            read -e -p "$(msg docker_proxy.registries_prompt): " _reg_input
            if [ "${_reg_input}" = "b" ]; then STEP_RESULT="back"; return 0; fi
            [ -n "${_reg_input}" ] && HICLAW_PROXY_ALLOWED_REGISTRIES="${_reg_input}"
        elif [ -z "${HICLAW_PROXY_ALLOWED_REGISTRIES+x}" ]; then
            local _reg_input
            read -e -p "$(msg docker_proxy.registries_prompt): " _reg_input
            if [ "${_reg_input}" = "b" ]; then STEP_RESULT="back"; return 0; fi
            HICLAW_PROXY_ALLOWED_REGISTRIES="${_reg_input:-}"
        fi
        export HICLAW_PROXY_ALLOWED_REGISTRIES
    else
        log "$(msg docker_proxy.selected_disabled)"
    fi
}

step_idle() {
    if [ "${HICLAW_UPGRADE}" = "1" ] && [ -n "${HICLAW_WORKER_IDLE_TIMEOUT}" ]; then
        log "$(msg prompt.upgrade_keep "$(msg idle_timeout.label)" "${HICLAW_WORKER_IDLE_TIMEOUT}")"
        local _idle_timeout
        read -e -p "$(msg idle_timeout.prompt): " _idle_timeout
        if [ "${_idle_timeout}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        [ -n "${_idle_timeout}" ] && HICLAW_WORKER_IDLE_TIMEOUT="${_idle_timeout}"
    elif [ -z "${HICLAW_WORKER_IDLE_TIMEOUT+x}" ]; then
        local _idle_timeout
        read -e -p "$(msg idle_timeout.prompt): " _idle_timeout
        if [ "${_idle_timeout}" = "b" ]; then STEP_RESULT="back"; return 0; fi
        HICLAW_WORKER_IDLE_TIMEOUT="${_idle_timeout:-720}"
    fi
    HICLAW_WORKER_IDLE_TIMEOUT="${HICLAW_WORKER_IDLE_TIMEOUT:-720}"
    export HICLAW_WORKER_IDLE_TIMEOUT
    log "$(msg idle_timeout.selected "${HICLAW_WORKER_IDLE_TIMEOUT}")"
}

step_hostshare() {
    local _share_dir
    read -e -p "$(msg host_share.prompt "$HOME"): " _share_dir
    if [ "${_share_dir}" = "b" ]; then STEP_RESULT="back"; return 0; fi
    HICLAW_HOST_SHARE_DIR="${_share_dir:-$HOME}"
    export HICLAW_HOST_SHARE_DIR
}

# ============================================================
# Manager Installation (Interactive)
# ============================================================

install_manager() {
    log "$(msg install.title)"
    log "$(msg install.registry "${HICLAW_REGISTRY}")"
    log ""
    log "$(msg install.dir "$(pwd)")"
    log "$(msg install.dir_hint)"
    log "$(msg install.dir_hint2)"
    log ""

    # Non-interactive fallback: resolve version immediately so image tags are available
    # before the step state machine runs. Interactive mode lets step_version handle it.
    if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
        HICLAW_VERSION="${HICLAW_VERSION:-${HICLAW_KNOWN_STABLE_VERSION}}"
        resolve_image_tags
    fi

    # Migrate legacy env file location before checks
    local existing_env="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
    if [ ! -f "${existing_env}" ] && [ -f "./hiclaw-manager.env" ]; then
        log "Migrating hiclaw-manager.env from current directory to ${existing_env}..."
        mv "./hiclaw-manager.env" "${existing_env}"
    fi

    # Orphan volume detection (only when no env file — step_existing handles the env-file case)
    if [ ! -f "${existing_env}" ]; then
        local data_vol="${HICLAW_DATA_DIR:-hiclaw-data}"
        if ${DOCKER_CMD} volume ls -q | grep -q "^${data_vol}$"; then
            echo ""
            log "$(msg install.orphan_volume.detected "${data_vol}")"
            log "$(msg install.orphan_volume.warn)"
            if [ "${HICLAW_NON_INTERACTIVE}" = "1" ]; then
                log "$(msg install.orphan_volume.clean_noninteractive)"
                ${DOCKER_CMD} stop hiclaw-manager 2>/dev/null || true
                ${DOCKER_CMD} rm hiclaw-manager 2>/dev/null || true
                for w in $(${DOCKER_CMD} ps -a --format '{{.Names}}' | grep "^hiclaw-worker-" || true); do
                    ${DOCKER_CMD} stop "${w}" 2>/dev/null || true
                    ${DOCKER_CMD} rm "${w}" 2>/dev/null || true
                done
                log "$(msg install.orphan_volume.cleaning)"
                ${DOCKER_CMD} volume rm "${data_vol}" 2>/dev/null || true
                log "$(msg install.orphan_volume.cleaned)"
            else
                echo ""
                echo "$(msg install.orphan_volume.choose)"
                echo "$(msg install.orphan_volume.clean)"
                echo "$(msg install.orphan_volume.keep)"
                echo ""
                local ORPHAN_CHOICE
                read -e -p "$(msg install.orphan_volume.prompt): " ORPHAN_CHOICE
                ORPHAN_CHOICE="${ORPHAN_CHOICE:-1}"
                case "${ORPHAN_CHOICE}" in
                    1|clean)
                        ${DOCKER_CMD} stop hiclaw-manager 2>/dev/null || true
                        ${DOCKER_CMD} rm hiclaw-manager 2>/dev/null || true
                        for w in $(${DOCKER_CMD} ps -a --format '{{.Names}}' | grep "^hiclaw-worker-" || true); do
                            ${DOCKER_CMD} stop "${w}" 2>/dev/null || true
                            ${DOCKER_CMD} rm "${w}" 2>/dev/null || true
                        done
                        log "$(msg install.orphan_volume.cleaning)"
                        ${DOCKER_CMD} volume rm "${data_vol}" 2>/dev/null || true
                        log "$(msg install.orphan_volume.cleaned)"
                        ;;
                    2|keep)
                        log "$(msg install.orphan_volume.keeping)"
                        ;;
                esac
            fi
        fi
    fi

    # ── State machine ─────────────────────────────────────────────────────────
    local _STEPS=( step_lang step_mode step_version step_existing step_llm step_admin step_network \
                   step_ports step_domains step_github step_skills step_volume \
                   step_workspace step_runtime step_e2ee step_docker_proxy step_idle step_hostshare )
    local _STEP_HISTORY=()
    local _step_idx=0
    while [ "${_step_idx}" -lt "${#_STEPS[@]}" ]; do
        local _step_fn="${_STEPS[$_step_idx]}"
        if should_skip_step "${_step_fn}"; then
            _step_idx=$((_step_idx + 1))
            continue
        fi
        if [ "${#_STEP_HISTORY[@]}" -gt 0 ]; then
            log "$(msg nav.back_hint)"
        fi
        STEP_RESULT=""
        "${_step_fn}"
        if [ "${STEP_RESULT}" = "back" ]; then
            if [ "${#_STEP_HISTORY[@]}" -gt 0 ]; then
                local _last=$(( ${#_STEP_HISTORY[@]} - 1 ))
                _step_idx="${_STEP_HISTORY[$_last]}"
                _STEP_HISTORY=("${_STEP_HISTORY[@]:0:${_last}}")
                clear_step_vars "${_STEPS[$_step_idx]}"
            fi
            # else: first step, ignore 'b'
        else
            _STEP_HISTORY+=("${_step_idx}")
            _step_idx=$((_step_idx + 1))
        fi
    done
    # ── End state machine ──────────────────────────────────────────────────────

    # Post-machine defaults for any steps that were skipped
    HICLAW_DATA_DIR="${HICLAW_DATA_DIR:-hiclaw-data}"
    if [ -z "${HICLAW_WORKSPACE_DIR+x}" ] || [ -z "${HICLAW_WORKSPACE_DIR}" ]; then
        HICLAW_WORKSPACE_DIR="${HOME}/hiclaw-manager"
        export HICLAW_WORKSPACE_DIR
    fi
    HICLAW_WORKSPACE_DIR="$(cd "${HICLAW_WORKSPACE_DIR}" 2>/dev/null && pwd || echo "${HICLAW_WORKSPACE_DIR}")"
    mkdir -p "${HICLAW_WORKSPACE_DIR}"
    HICLAW_DEFAULT_WORKER_RUNTIME="${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}"
    HICLAW_MATRIX_E2EE="${HICLAW_MATRIX_E2EE:-0}"
    export HICLAW_MATRIX_E2EE
    HICLAW_WORKER_IDLE_TIMEOUT="${HICLAW_WORKER_IDLE_TIMEOUT:-720}"
    export HICLAW_WORKER_IDLE_TIMEOUT
    HICLAW_HOST_SHARE_DIR="${HICLAW_HOST_SHARE_DIR:-$HOME}"
    export HICLAW_HOST_SHARE_DIR

    log ""

    # Generate secrets (only if not already set)
    log "$(msg install.generating_secrets)"
    HICLAW_MANAGER_PASSWORD="${HICLAW_MANAGER_PASSWORD:-$(generate_key)}"
    HICLAW_REGISTRATION_TOKEN="${HICLAW_REGISTRATION_TOKEN:-$(generate_key)}"
    HICLAW_MINIO_USER="${HICLAW_MINIO_USER:-${HICLAW_ADMIN_USER}}"
    HICLAW_MINIO_PASSWORD="${HICLAW_MINIO_PASSWORD:-${HICLAW_ADMIN_PASSWORD}}"
    HICLAW_MANAGER_GATEWAY_KEY="${HICLAW_MANAGER_GATEWAY_KEY:-$(generate_key)}"

    # Write .env file
    ENV_FILE="${HICLAW_ENV_FILE:-${HOME}/hiclaw-manager.env}"
    cat > "${ENV_FILE}" << EOF
# HiClaw Manager Configuration
# Generated by hiclaw-install.sh on $(date)

# Language
HICLAW_LANGUAGE=${HICLAW_LANGUAGE}

# LLM
HICLAW_LLM_PROVIDER=${HICLAW_LLM_PROVIDER}
HICLAW_DEFAULT_MODEL=${HICLAW_DEFAULT_MODEL}
HICLAW_LLM_API_KEY=${HICLAW_LLM_API_KEY}
HICLAW_OPENAI_BASE_URL=${HICLAW_OPENAI_BASE_URL:-}
HICLAW_MODEL_CONTEXT_WINDOW=${HICLAW_MODEL_CONTEXT_WINDOW:-}
HICLAW_MODEL_MAX_TOKENS=${HICLAW_MODEL_MAX_TOKENS:-}
HICLAW_MODEL_REASONING=${HICLAW_MODEL_REASONING:-}
HICLAW_MODEL_VISION=${HICLAW_MODEL_VISION:-}

# Admin
HICLAW_ADMIN_USER=${HICLAW_ADMIN_USER}
HICLAW_ADMIN_PASSWORD=${HICLAW_ADMIN_PASSWORD}

# Ports
HICLAW_LOCAL_ONLY=${HICLAW_LOCAL_ONLY}
HICLAW_PORT_GATEWAY=${HICLAW_PORT_GATEWAY}
HICLAW_PORT_CONSOLE=${HICLAW_PORT_CONSOLE}
HICLAW_PORT_ELEMENT_WEB=${HICLAW_PORT_ELEMENT_WEB}
HICLAW_PORT_OPENCLAW_CONSOLE=${HICLAW_PORT_OPENCLAW_CONSOLE:-18888}

# Matrix
HICLAW_MATRIX_DOMAIN=${HICLAW_MATRIX_DOMAIN}
HICLAW_MATRIX_CLIENT_DOMAIN=${HICLAW_MATRIX_CLIENT_DOMAIN}

# Gateway
HICLAW_AI_GATEWAY_DOMAIN=${HICLAW_AI_GATEWAY_DOMAIN}
HICLAW_MANAGER_GATEWAY_KEY=${HICLAW_MANAGER_GATEWAY_KEY}

# File System
HICLAW_FS_DOMAIN=${HICLAW_FS_DOMAIN}
HICLAW_CONSOLE_DOMAIN=${HICLAW_CONSOLE_DOMAIN}
HICLAW_MINIO_USER=${HICLAW_MINIO_USER}
HICLAW_MINIO_PASSWORD=${HICLAW_MINIO_PASSWORD}

# Internal
HICLAW_MANAGER_PASSWORD=${HICLAW_MANAGER_PASSWORD}
HICLAW_REGISTRATION_TOKEN=${HICLAW_REGISTRATION_TOKEN}

# GitHub (optional)
HICLAW_GITHUB_TOKEN=${HICLAW_GITHUB_TOKEN:-}

# Nacos package import defaults (optional)
HICLAW_NACOS_USERNAME=${HICLAW_NACOS_USERNAME:-}
HICLAW_NACOS_PASSWORD=${HICLAW_NACOS_PASSWORD:-}

# Skills Registry (optional, default: https://skills.sh)
HICLAW_SKILLS_API_URL=${HICLAW_SKILLS_API_URL:-}

# OpenClaw CMS plugin (optional)
HICLAW_CMS_TRACES_ENABLED=${HICLAW_CMS_TRACES_ENABLED:-false}
HICLAW_CMS_ENDPOINT=${HICLAW_CMS_ENDPOINT:-}
HICLAW_CMS_LICENSE_KEY=${HICLAW_CMS_LICENSE_KEY:-}
HICLAW_CMS_PROJECT=${HICLAW_CMS_PROJECT:-}
HICLAW_CMS_WORKSPACE=${HICLAW_CMS_WORKSPACE:-}
HICLAW_CMS_SERVICE_NAME=${HICLAW_CMS_SERVICE_NAME:-hiclaw-manager}
HICLAW_CMS_METRICS_ENABLED=${HICLAW_CMS_METRICS_ENABLED:-false}

# Worker images (for direct container creation)
HICLAW_WORKER_IMAGE=${WORKER_IMAGE}
HICLAW_COPAW_WORKER_IMAGE=${COPAW_WORKER_IMAGE}

# Default Worker runtime (openclaw | copaw)
HICLAW_DEFAULT_WORKER_RUNTIME=${HICLAW_DEFAULT_WORKER_RUNTIME:-openclaw}

# Matrix E2EE (0=disabled, 1=enabled; default: 0)
HICLAW_MATRIX_E2EE=${HICLAW_MATRIX_E2EE:-0}

# Docker API proxy (0=disabled, 1=enabled; default: 1)
HICLAW_DOCKER_PROXY=${HICLAW_DOCKER_PROXY:-1}

# Docker API proxy: additional allowed image sources (comma-separated)
HICLAW_PROXY_ALLOWED_REGISTRIES=${HICLAW_PROXY_ALLOWED_REGISTRIES:-}

# Worker idle timeout in minutes (default: 720 = 12 hours)
HICLAW_WORKER_IDLE_TIMEOUT=${HICLAW_WORKER_IDLE_TIMEOUT:-720}

# Higress WASM plugin image registry (auto-selected by timezone)
HIGRESS_ADMIN_WASM_PLUGIN_IMAGE_REGISTRY=${HICLAW_REGISTRY}

# Data persistence
HICLAW_DATA_DIR=${HICLAW_DATA_DIR:-hiclaw-data}
# Manager workspace (skills, memory, state — host-editable)
HICLAW_WORKSPACE_DIR=${HICLAW_WORKSPACE_DIR:-}
# Host directory sharing
HICLAW_HOST_SHARE_DIR=${HICLAW_HOST_SHARE_DIR:-}
EOF

    chmod 600 "${ENV_FILE}"
    log "$(msg install.config_saved "${ENV_FILE}")"

    # Detect container runtime socket
    SOCKET_MOUNT_ARGS=""
    if [ "${HICLAW_MOUNT_SOCKET}" = "1" ]; then
        CONTAINER_SOCK=$(detect_socket)
        if [ -n "${CONTAINER_SOCK}" ]; then
            log "$(msg install.socket_detected "${CONTAINER_SOCK}")"
            SOCKET_MOUNT_ARGS="-v ${CONTAINER_SOCK}:/var/run/docker.sock --security-opt label=disable"
        else
            log "$(msg install.socket_not_found)"
            # Interactive confirmation when socket not found
            if [ "${HICLAW_NON_INTERACTIVE}" != "1" ]; then
                echo ""
                echo -e "\033[33m$(msg install.socket_confirm.title)\033[0m"
                echo ""
                echo -e "$(msg install.socket_confirm.message)"
                echo ""
                read -p "$(msg install.socket_confirm.prompt)" SOCKET_CONFIRM
                if [ "${SOCKET_CONFIRM}" != "y" ] && [ "${SOCKET_CONFIRM}" != "Y" ]; then
                    log "$(msg install.socket_confirm.cancelled)"
                    exit 0
                fi
            fi
        fi
    fi

    # Create the data volume if it doesn't already exist (reuse on reinstall)
    if ! ${DOCKER_CMD} volume ls -q | grep -q "^${HICLAW_DATA_DIR}$"; then
        ${DOCKER_CMD} volume create "${HICLAW_DATA_DIR}" > /dev/null
    fi

    # Data mount: Docker volume
    DATA_MOUNT_ARGS="-v ${HICLAW_DATA_DIR}:/data"

    # Manager workspace mount (always a host directory, defaulting to ~/hiclaw-manager)
    WORKSPACE_MOUNT_ARGS="-v ${HICLAW_WORKSPACE_DIR}:/root/manager-workspace"

    # Pass host timezone to container so date/time commands reflect local time
    TZ_ARGS="-e TZ=${HICLAW_TIMEZONE}"

    # Host directory mount
    if [ -d "${HICLAW_HOST_SHARE_DIR}" ]; then
        HOST_SHARE_MOUNT_ARGS="-v ${HICLAW_HOST_SHARE_DIR}:/host-share"
        log "$(msg host_share.sharing "${HICLAW_HOST_SHARE_DIR}")"
    else
        log "$(msg host_share.not_exist "${HICLAW_HOST_SHARE_DIR}")"
        HOST_SHARE_MOUNT_ARGS="-v ${HICLAW_HOST_SHARE_DIR}:/host-share"
    fi

    # YOLO mode: pass through if set in environment (enables autonomous decisions)
    YOLO_ARGS=""
    if [ "${HICLAW_YOLO:-}" = "1" ]; then
        YOLO_ARGS="-e HICLAW_YOLO=1"
        log "$(msg install.yolo)"
    fi

    # E2EE is already in the env file; but also pass explicitly in case env file is not the source
    # (HICLAW_MATRIX_E2EE is already written to ENV_FILE above via --env-file)

    # Pull images (pull the selected runtime's worker image; on upgrade, also pull the other if present locally)
    LOCAL_IMAGE_PREFIX="hiclaw/"

    # Helper: pull or skip a single image
    # Args: $1=image  $2=exists_msg_key  $3=pulling_msg_key
    _pull_image() {
        local _img="$1" _exists_key="$2" _pull_key="$3"
        if echo "${_img}" | grep -q "^${LOCAL_IMAGE_PREFIX}"; then
            if ${DOCKER_CMD} image inspect "${_img}" >/dev/null 2>&1; then
                log "$(msg "${_exists_key}" "${_img}")"
                return 0
            fi
        fi
        log "$(msg "${_pull_key}" "${_img}")"
        ${DOCKER_CMD} pull "${_img}"
    }

    # Manager image is always required
    _pull_image "${MANAGER_IMAGE}" "install.image.exists" "install.image.pulling_manager"

    # Pull worker image for the selected runtime
    if [ "${HICLAW_DEFAULT_WORKER_RUNTIME}" = "copaw" ]; then
        _pull_image "${COPAW_WORKER_IMAGE}" "install.image.worker_exists" "install.image.pulling_worker"
    else
        _pull_image "${WORKER_IMAGE}" "install.image.worker_exists" "install.image.pulling_worker"
    fi

    # During upgrade, also pull the other worker image if containers using it exist locally.
    # This ensures ALL worker containers get updated, not just the ones matching the selected runtime.
    if [ "${HICLAW_UPGRADE:-0}" = "1" ]; then
        if [ "${HICLAW_DEFAULT_WORKER_RUNTIME}" = "copaw" ]; then
            # Selected copaw, check if any openclaw worker image exists locally
            if ${DOCKER_CMD} image inspect "${WORKER_IMAGE}" >/dev/null 2>&1; then
                _pull_image "${WORKER_IMAGE}" "install.image.worker_exists" "install.image.pulling_worker"
            fi
        else
            # Selected openclaw, check if any copaw worker image exists locally
            if ${DOCKER_CMD} image inspect "${COPAW_WORKER_IMAGE}" >/dev/null 2>&1; then
                _pull_image "${COPAW_WORKER_IMAGE}" "install.image.worker_exists" "install.image.pulling_worker"
            fi
        fi
    fi

    # Resolve and pull docker-proxy image (probes versioned tag, falls back to latest)
    if [ "${HICLAW_DOCKER_PROXY:-0}" = "1" ]; then
        resolve_docker_proxy_image
    fi

    # Stop and remove existing containers (deferred from upgrade detection
    # so that all configuration is collected and images are pulled first)
    if ${DOCKER_CMD} ps -a --format '{{.Names}}' | grep -q "^hiclaw-docker-proxy$"; then
        ${DOCKER_CMD} stop hiclaw-docker-proxy 2>/dev/null || true
        ${DOCKER_CMD} rm hiclaw-docker-proxy 2>/dev/null || true
    fi
    if ${DOCKER_CMD} ps -a --format '{{.Names}}' | grep -q "^hiclaw-manager$"; then
        log "$(msg install.removing_existing)"
        ${DOCKER_CMD} stop hiclaw-manager 2>/dev/null || true
        ${DOCKER_CMD} rm hiclaw-manager 2>/dev/null || true
    fi

    # Stop and remove worker containers saved during upgrade detection
    # (Manager IP changes on restart, so workers must be recreated)
    if [ -n "${UPGRADE_EXISTING_WORKERS:-}" ]; then
        log "$(msg install.existing.stopping_workers)"
        for w in ${UPGRADE_EXISTING_WORKERS}; do
            ${DOCKER_CMD} stop "${w}" 2>/dev/null || true
            ${DOCKER_CMD} rm "${w}" 2>/dev/null || true
            log "$(msg install.existing.removed "${w}")"
        done
    fi

    # Run Manager container
    log "$(msg install.starting_manager)"

    # Ensure hiclaw-net Docker network exists; Manager joins it so workers can reach
    # Manager services via Docker DNS (using the network aliases added below).
    NETWORK_ARGS=""
    NETWORK_ALIAS_ARGS=""
    if [ -n "${CONTAINER_SOCK:-}" ] || [ "${HICLAW_DOCKER_PROXY:-0}" = "1" ]; then
        ${DOCKER_CMD} network inspect hiclaw-net >/dev/null 2>&1 || ${DOCKER_CMD} network create hiclaw-net
        NETWORK_ARGS="--network hiclaw-net"
        # Workers hardcode these three internal domains to reach manager services,
        # so they must always be network aliases regardless of user domain config.
        NETWORK_ALIAS_ARGS="--network-alias matrix-local.hiclaw.io --network-alias aigw-local.hiclaw.io --network-alias fs-local.hiclaw.io"
        # Also alias any *-local.hiclaw.io user-configured domains that differ from the fixed ones above.
        for _domain in "${HICLAW_MATRIX_CLIENT_DOMAIN}" "${HICLAW_CONSOLE_DOMAIN}"; do
            if [[ "${_domain}" == *-local.hiclaw.io ]]; then
                NETWORK_ALIAS_ARGS="${NETWORK_ALIAS_ARGS} --network-alias ${_domain}"
            fi
        done
    fi

    # Start Docker API proxy if enabled (security layer between Manager and Docker daemon)
    PROXY_ARGS=""
    if [ "${HICLAW_DOCKER_PROXY:-0}" = "1" ] && [ -n "${CONTAINER_SOCK:-}" ]; then
        local _proxy_image="${DOCKER_PROXY_IMAGE}"
        log "Starting Docker API proxy..."
        ${DOCKER_CMD} run -d \
            --name hiclaw-docker-proxy \
            --network hiclaw-net \
            -v "${CONTAINER_SOCK}:/var/run/docker.sock" \
            --security-opt label=disable \
            ${HICLAW_PROXY_ALLOWED_REGISTRIES:+-e HICLAW_PROXY_ALLOWED_REGISTRIES="${HICLAW_PROXY_ALLOWED_REGISTRIES}"} \
            --restart unless-stopped \
            "${_proxy_image}"
        PROXY_ARGS="-e HICLAW_CONTAINER_API=http://hiclaw-docker-proxy:2375"
        SOCKET_MOUNT_ARGS=""  # Manager no longer needs direct socket access
    fi

    # Build port binding args (127.0.0.1 prefix for local-only mode)
    if [ "${HICLAW_LOCAL_ONLY:-1}" = "1" ]; then
        _port_prefix="127.0.0.1:"
    else
        _port_prefix=""
    fi
    # shellcheck disable=SC2086
    ${DOCKER_CMD} run -d \
        --name hiclaw-manager \
        --env-file "${ENV_FILE}" \
        -e HOME=/root/manager-workspace \
        -w /root/manager-workspace \
        -e HOST_ORIGINAL_HOME="${HICLAW_HOST_SHARE_DIR}" \
        ${YOLO_ARGS} \
        ${TZ_ARGS} \
        ${SOCKET_MOUNT_ARGS} \
        ${NETWORK_ARGS} \
        ${NETWORK_ALIAS_ARGS} \
        ${PROXY_ARGS} \
        -p "${_port_prefix}${HICLAW_PORT_GATEWAY}:8080" \
        -p "${_port_prefix}${HICLAW_PORT_CONSOLE}:8001" \
        -p "${_port_prefix}${HICLAW_PORT_ELEMENT_WEB:-18088}:8088" \
        -p "127.0.0.1:${HICLAW_PORT_OPENCLAW_CONSOLE:-18888}:18888" \
        ${DATA_MOUNT_ARGS} \
        ${WORKSPACE_MOUNT_ARGS} \
        ${HOST_SHARE_MOUNT_ARGS} \
        --restart unless-stopped \
        "${MANAGER_IMAGE}"
    unset _port_prefix

    # Wait for Manager agent to be ready
    wait_manager_ready "hiclaw-manager"

    # Wait for Matrix server to be ready
    wait_matrix_ready "hiclaw-manager"

    # Post-install verification (non-fatal: warnings only)
    local _verify_script
    _verify_script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/hiclaw-verify.sh"
    if [ -f "${_verify_script}" ]; then
        bash "${_verify_script}" "hiclaw-manager" || {
            log "WARNING: Some post-install checks failed. Re-run: bash install/hiclaw-verify.sh"
        }
    fi

    log ""
    log "$(msg success.title)"
    log ""
    log "$(msg success.domains_configured)"
    log "  ${HICLAW_MATRIX_DOMAIN%%:*} ${HICLAW_MATRIX_CLIENT_DOMAIN} ${HICLAW_AI_GATEWAY_DOMAIN} ${HICLAW_FS_DOMAIN} ${HICLAW_CONSOLE_DOMAIN}"
    log ""
    local lan_ip
    lan_ip=$(detect_lan_ip)
    echo -e "\033[33m━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\033[0m"
    echo -e "\033[33m  $(msg success.open_url)\033[0m"
    echo -e "\033[33m                                                                                 \033[0m"
    echo -e "\033[1;36m    http://127.0.0.1:${HICLAW_PORT_ELEMENT_WEB:-18088}/#/login\033[0m"
    echo -e "\033[33m                                                                                 \033[0m"
    echo -e "\033[33m  $(msg success.login_with)\033[0m"
    echo -e "\033[33m    $(msg success.username "${HICLAW_ADMIN_USER}")\033[0m"
    echo -e "\033[33m    $(msg success.password "${HICLAW_ADMIN_PASSWORD}")\033[0m"
    echo -e "\033[33m                                                                                 \033[0m"
    echo -e "\033[33m  $(msg success.after_login)\033[0m"
    echo -e "\033[33m    $(msg success.tell_it)\033[0m"
    echo -e "\033[33m    $(msg success.manager_auto)\033[0m"
    echo -e "\033[33m                                                                                 \033[0m"
    echo -e "\033[33m  ─────────────────────────────────────────────────────────────────────────────  \033[0m"
    echo -e "\033[33m  $(msg success.mobile_title)\033[0m"
    echo -e "\033[33m                                                                                 \033[0m"
    if [ -n "${lan_ip}" ]; then
        echo -e "\033[33m    $(msg success.mobile_step1)\033[0m"
        echo -e "\033[33m    $(msg success.mobile_step2 "http://${lan_ip}:${HICLAW_PORT_GATEWAY}")\033[0m"
        echo -e "\033[33m    $(msg success.mobile_step3)\033[0m"
        echo -e "\033[33m         $(msg success.mobile_username "${HICLAW_ADMIN_USER}")\033[0m"
        echo -e "\033[33m         $(msg success.mobile_password "${HICLAW_ADMIN_PASSWORD}")\033[0m"
    else
        echo -e "\033[33m    $(msg success.mobile_step1)\033[0m"
        echo -e "\033[33m    $(msg success.mobile_step2_noip "${HICLAW_PORT_GATEWAY}")\033[0m"
        echo -e "\033[33m    $(msg success.mobile_noip_hint)\033[0m"
        echo -e "\033[33m    $(msg success.mobile_step3)\033[0m"
        echo -e "\033[33m         $(msg success.mobile_username "${HICLAW_ADMIN_USER}")\033[0m"
        echo -e "\033[33m         $(msg success.mobile_password "${HICLAW_ADMIN_PASSWORD}")\033[0m"
    fi
    echo -e "\033[33m                                                                                 \033[0m"
    echo -e "\033[33m━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\033[0m"
    log ""
    log "$(msg success.other_consoles)"
    log "$(msg success.higress_console "${HICLAW_PORT_CONSOLE}" "${HICLAW_ADMIN_USER}" "${HICLAW_ADMIN_PASSWORD}")"
    log "$(msg success.openclaw_console "${HICLAW_PORT_OPENCLAW_CONSOLE:-18888}")"
    log "$(msg success.openclaw_console_gateway "${HICLAW_ADMIN_USER}" "${HICLAW_ADMIN_PASSWORD}")"
    log ""
    log "$(msg success.switch_llm.title)"
    log "$(msg success.switch_llm.hint)"
    log "$(msg success.switch_llm.docs)"
    log "$(msg success.switch_llm.url)"
    log ""
    log "$(msg success.tip)"
    log ""
    if [ "${HICLAW_LOCAL_ONLY:-1}" != "1" ]; then
        echo -e "\033[33m$(msg port.local_only.https_hint)\033[0m"
        log ""
    fi
    log "$(msg success.config_file "${ENV_FILE}")"
    log "$(msg success.data_volume "${HICLAW_DATA_DIR}")"
    log "$(msg success.workspace "${HICLAW_WORKSPACE_DIR}")"
}

# ============================================================
# Worker Installation (One-Click)
# ============================================================

install_worker() {
    local WORKER_NAME=""
    local FS=""
    local FS_KEY=""
    local FS_SECRET=""
    local RESET=false
    local SKILLS_API_URL=""

    # Parse arguments
    while [ $# -gt 0 ]; do
        case $1 in
            --name)       WORKER_NAME="$2"; shift 2 ;;
            --fs)         FS="$2"; shift 2 ;;
            --fs-key)     FS_KEY="$2"; shift 2 ;;
            --fs-secret)  FS_SECRET="$2"; shift 2 ;;
            --skills-api-url) SKILLS_API_URL="$2"; shift 2 ;;
            --reset)      RESET=true; shift ;;
            *)            error "$(msg error.unknown_option "$1")" ;;
        esac
    done

    # Validate required params
    [ -z "${WORKER_NAME}" ] && error "$(msg error.name_required)"
    [ -z "${FS}" ] && error "$(msg error.fs_required)"
    [ -z "${FS_KEY}" ] && error "$(msg error.fs_key_required)"
    [ -z "${FS_SECRET}" ] && error "$(msg error.fs_secret_required)"

    local CONTAINER_NAME="hiclaw-worker-${WORKER_NAME}"

    # Handle reset
    if [ "${RESET}" = true ]; then
        log "$(msg worker.resetting "${WORKER_NAME}")"
        ${DOCKER_CMD} stop "${CONTAINER_NAME}" 2>/dev/null || true
        ${DOCKER_CMD} rm "${CONTAINER_NAME}" 2>/dev/null || true
    fi

    # Check for existing container
    if ${DOCKER_CMD} ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        error "$(msg worker.exists "${CONTAINER_NAME}")"
    fi

    log "$(msg worker.starting "${WORKER_NAME}")"

    # Build docker run args
    local DOCKER_ENV=""
    DOCKER_ENV="${DOCKER_ENV} -e HOME=/root/hiclaw-fs/agents/${WORKER_NAME}"
    DOCKER_ENV="${DOCKER_ENV} -w /root/hiclaw-fs/agents/${WORKER_NAME}"
    DOCKER_ENV="${DOCKER_ENV} -e HICLAW_WORKER_NAME=${WORKER_NAME}"
    DOCKER_ENV="${DOCKER_ENV} -e HICLAW_FS_ENDPOINT=${FS}"
    DOCKER_ENV="${DOCKER_ENV} -e HICLAW_FS_ACCESS_KEY=${FS_KEY}"
    DOCKER_ENV="${DOCKER_ENV} -e HICLAW_FS_SECRET_KEY=${FS_SECRET}"

    # Add SKILLS_API_URL if specified
    if [ -n "${SKILLS_API_URL}" ]; then
        DOCKER_ENV="${DOCKER_ENV} -e SKILLS_API_URL=${SKILLS_API_URL}"
        log "$(msg worker.skills_url "${SKILLS_API_URL}")"
    fi

    # shellcheck disable=SC2086
    ${DOCKER_CMD} run -d \
        --name "${CONTAINER_NAME}" \
        ${DOCKER_ENV} \
        --restart unless-stopped \
        "${WORKER_IMAGE}"

    log ""
    log "$(msg worker.started "${WORKER_NAME}")"
    log "$(msg worker.container "${CONTAINER_NAME}")"
    log "$(msg worker.view_logs "${CONTAINER_NAME}")"
}

# ============================================================
# Main
# ============================================================

# ============================================================
# LLM API connectivity test
# ============================================================

test_llm_connectivity() {
    local base_url="$1"
    local api_key="$2"
    local model="$3"
    local hint="${4:-}"  # optional: extra hint shown on failure
    if ! command -v curl >/dev/null 2>&1; then
        echo -e "\033[33m$(msg llm.openai.test.no_curl)\033[0m"
        return
    fi
    log "$(msg llm.openai.test.testing)"
    local _body _http_code _tmpfile
    _tmpfile=$(mktemp)
    _http_code=$(curl -s -o "${_tmpfile}" -w "%{http_code}" \
        -X POST "${base_url%/}/chat/completions" \
        -H "Authorization: Bearer ${api_key}" \
        -H "Content-Type: application/json" \
        -H "User-Agent: HiClaw/${HICLAW_VERSION:-latest}" \
        --max-time 30 \
        -d "{\"model\":\"${model}\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" \
        2>/dev/null)
    _body=$(cat "${_tmpfile}")
    rm -f "${_tmpfile}"
    if [ "${_http_code}" = "200" ] || [ "${_http_code}" = "201" ]; then
        log "$(msg llm.openai.test.ok)"
    else
        echo -e "\033[33m$(msg llm.openai.test.fail "${_http_code}" "${_body}")\033[0m"
        if [ -n "${hint}" ]; then
            echo -e "\033[33m${hint}\033[0m"
        fi
        if [ "${HICLAW_NON_INTERACTIVE}" != "1" ]; then
            local _confirm
            read -e -p "$(msg llm.openai.test.confirm)" _confirm
            if [ "${_confirm}" = "b" ]; then
                STEP_RESULT="back"
                return 1
            fi
            if [ "${_confirm}" != "y" ] && [ "${_confirm}" != "Y" ]; then
                log "$(msg llm.openai.test.aborted)"
                exit 1
            fi
        fi
    fi
}

# ============================================================
# Check container runtime (docker or podman)
# ============================================================

check_container_runtime() {
    if command -v docker >/dev/null 2>&1; then
        DOCKER_CMD="docker"
    elif command -v podman >/dev/null 2>&1; then
        DOCKER_CMD="podman"
    else
        echo -e "\033[31m[HiClaw ERROR]\033[0m $(msg error.docker_not_found)" >&2
        exit 1
    fi

    # Command exists — check if daemon is running
    if ! ${DOCKER_CMD} ps >/dev/null 2>&1; then
        echo -e "\033[31m[HiClaw ERROR]\033[0m $(msg error.docker_not_running)" >&2
        exit 1
    fi
}

check_container_runtime

case "${1:-}" in
    manager|"")
        # Default to manager installation if no argument or explicit "manager"
        install_manager
        ;;
    worker)
        shift
        install_worker "$@"
        ;;
    *)
        echo "Usage: $0 [manager|worker [options]]"
        echo ""
        echo "Commands:"
        echo "  manager              Interactive Manager installation (default)"
        echo "                       Choose Quick Start (all defaults) or Manual mode"
        echo "  worker               Worker installation (requires --name and connection params)"
        echo ""
        echo "Quick Start (fastest):"
        echo "  $0"
        echo "  # Then select '1' for Quick Start mode"
        echo ""
        echo "Non-interactive (for automation):"
        echo "  HICLAW_NON_INTERACTIVE=1 HICLAW_LLM_API_KEY=sk-xxx $0"
        echo ""
        echo "Worker Options:"
        echo "  --name <name>        Worker name (required)"
        echo "  --fs <url>           MinIO endpoint URL (required)"
        echo "  --fs-key <key>       MinIO access key (required)"
        echo "  --fs-secret <secret> MinIO secret key (required)"
        echo "  --reset              Remove existing Worker container before creating"
        exit 1
        ;;
esac
