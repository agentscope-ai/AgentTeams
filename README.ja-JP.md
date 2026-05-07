<h1 align="center">
    <img src="https://img.alicdn.com/imgextra/i2/O1CN01hTYQMO28B3H9qP7RV_!!6000000007893-2-tps-1490-392.png" alt="HiClaw"  width="290" height="72.5">
  <br>
</h1>

[English](./README.md) | [中文](./README.zh-CN.md) | [日本語](./README.ja-JP.md)

<p align="center">
  <a href="https://deepwiki.com/higress-group/hiclaw"><img src="https://img.shields.io/badge/DeepWiki-Ask_AI-navy.svg?logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACwAAAAyCAYAAAAnWDnqAAAAAXNSR0IArs4c6QAAA05JREFUaEPtmUtyEzEQhtWTQyQLHNak2AB7ZnyXZMEjXMGeK/AIi+QuHrMnbChYY7MIh8g01fJoopFb0uhhEqqcbWTp06/uv1saEDv4O3n3dV60RfP947Mm9/SQc0ICFQgzfc4CYZoTPAswgSJCCUJUnAAoRHOAUOcATwbmVLWdGoH//PB8mnKqScAhsD0kYP3j/Yt5LPQe2KvcXmGvRHcDnpxfL2zOYJ1mFwrryWTz0advv1Ut4CJgf5uhDuDj5eUcAUoahrdY/56ebRWeraTjMt/00Sh3UDtjgHtQNHwcRGOC98BJEAEymycmYcWwOprTgcB6VZ5JK5TAJ+fXGLBm3FDAmn6oPPjR4rKCAoJCal2eAiQp2x0vxTPB3ALO2CRkwmDy5WohzBDwSEFKRwPbknEggCPB/imwrycgxX2NzoMCHhPkDwqYMr9tRcP5qNrMZHkVnOjRMWwLCcr8ohBVb1OMjxLwGCvjTikrsBOiA6fNyCrm8V1rP93iVPpwaE+gO0SsWmPiXB+jikdf6SizrT5qKasx5j8ABbHpFTx+vFXp9EnYQmLx02h1QTTrl6eDqxLnGjporxl3NL3agEvXdT0WmEost648sQOYAeJS9Q7bfUVoMGnjo4AZdUMQku50McDcMWcBPvr0SzbTAFDfvJqwLzgxwATnCgnp4wDl6Aa+Ax283gghmj+vj7feE2KBBRMW3FzOpLOADl0Isb5587h/U4gGvkt5v60Z1VLG8BhYjbzRwyQZemwAd6cCR5/XFWLYZRIMpX39AR0tjaGGiGzLVyhse5C9RKC6ai42ppWPKiBagOvaYk8lO7DajerabOZP46Lby5wKjw1HCRx7p9sVMOWGzb/vA1hwiWc6jm3MvQDTogQkiqIhJV0nBQBTU+3okKCFDy9WwferkHjtxib7t3xIUQtHxnIwtx4mpg26/HfwVNVDb4oI9RHmx5WGelRVlrtiw43zboCLaxv46AZeB3IlTkwouebTr1y2NjSpHz68WNFjHvupy3q8TFn3Hos2IAk4Ju5dCo8B3wP7VPr/FGaKiG+T+v+TQqIrOqMTL1VdWV1DdmcbO8KXBz6esmYWYKPwDL5b5FA1a0hwapHiom0r/cKaoqr+27/XcrS5UwSMbQAAAABJRU5ErkJggg==" alt="DeepWiki"></a>
  <a href="https://discord.com/invite/NVjNA4BAVw"><img src="https://img.shields.io/badge/Discord-Join_Us-blueviolet.svg?logo=discord" alt="Discord"></a>
</p>

**HiClaw は、透明性の高い Human-in-the-Loop のタスク連携を Matrix ルームで実現する、オープンソースの協調型マルチエージェント OS です。**

**Manager-Workers アーキテクチャ**により、Manager Agent を通じて複数の Worker Agent を連携させ、複雑なタスクを完了できます。すべての会話は Matrix ルームで可視化され、いつでも介入できます。

チャットルームにいる AI チームのようなものです。Manager に必要なことを伝えると、Worker が起動し、すべてがリアルタイムで進行する様子を見ることができます。

## 主な特徴

- 🧬 **Manager-Workers アーキテクチャ**: 個々の Worker Claw を人間が監視する必要がなくなり、Agent が Agent を管理することを実現します。

- 🤝 **マルチランタイム協調**: OpenClaw、QwenPaw、Hermes の Worker が同じ IM ルーム内で共存します。決定論的な Agent（OpenClaw/QwenPaw）をリーダーとしてタスクを編成し、Hermes Worker に自律的なコード実行を担当させる — それぞれのランタイムが得意なことを担当します。

- 📦 **MinIO 共有ファイルシステム**: Agent 間の情報共有のための共有ファイルシステムを導入し、マルチエージェント連携シナリオにおけるトークン消費を大幅に削減します。

- 🔐 **Higress AI ゲートウェイ**: トラフィック管理を一元化し、認証情報に関連するリスクを軽減します。ネイティブの Lobster フレームワークにおけるセキュリティ上の懸念を解消します。

- ☎️ **Element IM クライアント + Tuwunel IM サーバー（共に Matrix プロトコルベース）**: DingTalk/Lark 統合の手間や企業承認ワークフローを排除します。IM 環境でモデルサービスの「快適さ」を素早く体験でき、ネイティブの OpenClaw IM 統合との互換性も維持します。

## ニュース

- **2026-04-24**: [English](blog/hiclaw-1.1.0-release.md) | [中文](blog/zh-cn/hiclaw-1.1.0-release.md) — HiClaw v1.1.0：Kubernetes ネイティブコントロールプレーン、Hermes 自律コーディング Agent ランタイム、1.7 GB イメージ縮小、hiclaw CLI がシェルスクリプトに代わる。
- **2026-04-14**: [English](blog/hiclaw-k8s-native-multi-agent-collaboration.md) | [中文](blog/zh-cn/hiclaw-k8s-native-multi-agent-collaboration.zh-CN.md) — Kubernetes ネイティブなマルチ Agent 協調オーケストレーションとしての HiClaw の解説。
- **2026-04-03**: [English](docs/declarative-resource-management.md) | [中文](docs/zh-cn/declarative-resource-management.md) — HiClaw 1.0.9：宣言型リソース管理、Worker テンプレートマーケット、Manager QwenPaw、Nacos Skills 登録センターなど。
- **2026-03-14**: [English](blog/hiclaw-1.0.6-release.md) | [中文](blog/zh-cn/hiclaw-1.0.6-release.md) — HiClaw 1.0.6：エンタープライズ MCP Server 管理、認証情報ゼロ露出。
- **2026-03-10**: [English](blog/hiclaw-1.0.4-release.md) | [中文](blog/zh-cn/hiclaw-1.0.4-release.md) — HiClaw 1.0.4：QwenPaw（旧CoPaw）Worker、メモリ約 80% 削減。
- **2026-03-04**: [English](blog/hiclaw-announcement.md) | [中文](blog/zh-cn/hiclaw-announcement.md) — HiClaw オープンソース化。

## HiClaw を選ぶ理由

- **エンタープライズグレードのセキュリティ**: Worker Agent はコンシューマートークンのみで動作します。実際の認証情報（API キー、GitHub PAT）はゲートウェイに保管され、Worker からも攻撃者からも見えません。

- **完全プライベート**: Matrix は分散型のオープンプロトコルです。自前でホスティングし、必要に応じて他のサーバーとフェデレーションできます。ベンダーロックインもデータ収集もありません。

- **デフォルトで Human-in-the-Loop**: すべての Matrix ルームにあなた、Manager、関連する Worker が参加しています。すべてを観察でき、いつでも介入できます。ブラックボックスはありません。

- **ゼロ設定の IM**: 内蔵の Matrix サーバーにより、ボットアプリケーション不要、API 承認不要、待ち時間なし。Element Web を開いてすぐにチャットを開始できます。

- **ワンコマンドセットアップ**: `curl | bash` だけで完了 — AI ゲートウェイ、Matrix サーバー、ファイルストレージ、Web クライアント、Manager Agent のすべてが揃います。

- **スキルエコシステム**: Worker は必要に応じて [skills.sh](https://skills.sh)（80,000 以上のコミュニティスキル）からスキルを取得します。Worker は実際の認証情報にアクセスできないため、安全に利用できます。

## クイックスタート

**前提条件**: Docker Desktop（Windows/macOS）または Docker Engine（Linux）。

**リソース**: 最低 2 CPU コア + 4 GB RAM。複数の Worker を利用する場合は 4 コア + 8 GB を推奨。

### インストール

**macOS / Linux:**
```bash
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

**Windows（PowerShell 7+ 推奨）:**
```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; iex $wc.DownloadString('https://higress.ai/hiclaw/install.ps1')
```

インストーラーが以下の手順をガイドします：
1. LLM プロバイダーの選択（OpenAI 互換 API に対応）
2. API キーの入力
3. ネットワークモードの選択（ローカル専用 or 外部アクセス）
4. セットアップ完了まで待機

### アクセス

ブラウザで http://127.0.0.1:18088 を開き、Element Web にログインしてください。Manager が挨拶し、最初の Worker の作成方法を説明してくれます。

**モバイル**: 任意の Matrix クライアント（Element、FluffyChat）を使い、サーバーアドレスに接続してください。

**以上です。** ボットアプリケーション不要。外部サービス不要。AI チーム全体があなたのマシン上で動作します。

## アップグレード

```bash
# 最新版にアップグレード（データはすべて保持）
bash <(curl -sSL https://higress.ai/hiclaw/install.sh)

# 特定バージョンにアップグレード
HICLAW_VERSION=v1.0.5 bash <(curl -sSL https://higress.ai/hiclaw/install.sh)
```

## アンインストール

**macOS / Linux:**
```bash
bash <(curl -fsSL https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.sh) uninstall
```

**Windows (PowerShell):**
```powershell
Set-ExecutionPolicy Bypass -Scope Process -Force; $wc=New-Object Net.WebClient; $wc.Encoding=[Text.Encoding]::UTF8; $s=$wc.DownloadString('https://raw.githubusercontent.com/higress-group/hiclaw/main/install/hiclaw-install.ps1'); & ([scriptblock]::Create($s)) uninstall
```

すべての HiClaw コンテナ（Manager、Worker、docker-proxy）、Docker ボリューム、ネットワーク、env ファイル、ワークスペースディレクトリ、インストールログが削除されます。

## Kubernetes へのインストール（Helm）

チーム共有や本番運用では、公式 Helm Chart を使って任意の Kubernetes クラスタに HiClaw をインストールできます。デフォルト構成には Higress AI ゲートウェイ、Tuwunel（Matrix）、MinIO、HiClaw Controller がすべて含まれており、外部依存はありません。

**前提条件**

- Kubernetes 1.24+（kind / minikube / k3s / マネージド K8s すべて対応）
- Helm 3.7+
- デフォルトの StorageClass（Tuwunel と MinIO の PVC 用）

**インストール（OpenAI / OpenAI 互換モード）**

```bash
helm repo add higress.io https://higress.io/helm-charts
helm repo update

helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

OpenAI 互換 API を提供する他のプロバイダーを使用する場合は、`llmBaseUrl` も設定してください：

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.llmBaseUrl=https://your-provider.example.com/v1 \
  --set credentials.defaultModel=your-model-name \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

<details>
<summary>Qwen（通義千問）を使用する場合</summary>

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set credentials.llmApiKey=<your-qwen-api-key> \
  --set credentials.llmProvider=qwen \
  --set credentials.defaultModel=qwen3.5-plus \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

</details>

| 値 | 必須 | 説明 |
|---|---|---|
| `credentials.llmApiKey` | 必須 | LLM プロバイダーの API キー |
| `gateway.publicURL` | 必須 | ユーザーが Element Web にアクセスする公開 URL（port-forward 環境では `http://localhost:18080`、本番では `https://hiclaw.example.com` 等） |
| `credentials.adminPassword` | 推奨 | Matrix 管理者パスワード。空のままだと自動生成（後で Secret から読み出す必要あり） |
| `credentials.llmProvider` | 任意 | LLM プロバイダー名、デフォルトは `openai-compat` |
| `credentials.defaultModel` | 任意 | デフォルトモデル、デフォルトは `gpt-5.4` |
| `credentials.llmBaseUrl` | 任意 | OpenAI 互換のベース URL（例: `https://api.deepseek.com/v1`）。公式 OpenAI API を使用する場合は空のまま |
| `manager.runtime` | 任意 | Manager エージェントランタイム: `openclaw`（デフォルト）、`copaw`、または `hermes` |
| `worker.defaultRuntime` | 任意 | Worker デフォルトランタイム: `openclaw`（デフォルト）、`copaw`、または `hermes` |

<details>
<summary>代替ランタイムの使用（QwenPaw Manager + Hermes Workers）</summary>

```bash
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace --devel \
  --set manager.runtime=copaw \
  --set worker.defaultRuntime=hermes \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.llmBaseUrl=https://your-provider.example.com/v1 \
  --set credentials.defaultModel=your-model-name \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

各コンポーネントのイメージはランタイムに基づいて自動的に選択されます（Manager: `hiclaw-manager` / `hiclaw-manager-copaw`、Worker: `hiclaw-worker` / `hiclaw-copaw-worker` / `hiclaw-hermes-worker`）。

</details>

**マルチリージョンイメージレジストリ**

デフォルトの `global.imageRegistry` は中国リージョン（`higress-registry.cn-hangzhou.cr.aliyuncs.com/higress`）を指しています。中国大陸以外にデプロイする場合は、近いリージョンに切り替えてイメージプルを高速化できます：

| リージョン | レジストリ |
|---|---|
| 中国（デフォルト） | `higress-registry.cn-hangzhou.cr.aliyuncs.com/higress` |
| 北米 | `higress-registry.us-west-1.cr.aliyuncs.com/higress` |
| 東南アジア | `higress-registry.ap-southeast-7.cr.aliyuncs.com/higress` |

```bash
# 例: 北米リージョンのレジストリを使用してデプロイ
helm install hiclaw higress.io/hiclaw \
  -n hiclaw-system --create-namespace \
  --render-subchart-notes \
  --set global.imageRegistry=higress-registry.us-west-1.cr.aliyuncs.com/higress \
  --set credentials.llmApiKey=<your-api-key> \
  --set credentials.adminPassword=<your-admin-password> \
  --set gateway.publicURL=http://localhost:18080
```

設定可能な全パラメータ（ゲートウェイ／ストレージの provider、イメージタグ、リソース、永続化など）は [`helm/hiclaw/values.yaml`](helm/hiclaw/values.yaml) を参照してください。

**アクセス**

```bash
kubectl port-forward -n hiclaw-system svc/higress-gateway 18080:80
```

ブラウザで http://localhost:18080 を開き Element Web にログインしてください。本番クラスタでは Ingress / LoadBalancer / DNS を `svc/higress-gateway` に向け、それに合わせて `gateway.publicURL` を設定してください。

**アップグレード**

```bash
helm repo update
helm upgrade hiclaw higress.io/hiclaw -n hiclaw-system --reuse-values
```

**アンインストール**

```bash
helm uninstall hiclaw -n hiclaw-system
kubectl delete namespace hiclaw-system
```

Kubernetes ネイティブなアーキテクチャ（CRD、Controller、宣言的な `Worker` / `Team` / `Human` リソース）の詳細は [docs/k8s-native-agent-orch.md](docs/k8s-native-agent-orch.md) を参照してください。

## 仕組み

### Manager — あなたの AI チーフオブスタッフ

```
あなた: alice という名前のフロントエンド開発用 Worker を作成して

Manager: 完了しました。Worker alice が準備できました。
         ルーム: Worker: Alice
         alice にタスクを指示してください。

あなた: @alice React でログインページを実装して

Alice: 承知しました...[数分後]
       完了しました。PR を提出しました: https://github.com/xxx/pull/1
```

<p align="center">
  <img src="https://img.alicdn.com/imgextra/i4/O1CN01wHWaJQ29KV3j5vryD_!!6000000008049-0-tps-589-1280.jpg" width="240" />
  &nbsp;&nbsp;&nbsp;&nbsp;
  <img src="https://img.alicdn.com/imgextra/i2/O1CN01q9L67J245mFT0fPXH_!!6000000007340-0-tps-589-1280.jpg" width="240" />
</p>
<p align="center">
  <sub>① Manager が Worker を作成しタスクを割り当て</sub>
  &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;
  <sub>② ルーム内で直接 Worker に指示することも可能</sub>
</p>

### セキュリティモデル

```
Worker（コンシューマートークンのみ）
    → Higress AI ゲートウェイ（実際の API キー、GitHub PAT を保持）
        → LLM API / GitHub API / MCP Server
```

Worker は自身のコンシューマートークンしか見えません。ゲートウェイがすべての実際の認証情報を管理します。Manager は Worker の作業内容を把握していますが、実際のキーに触れることはありません。

### Human in the Loop

すべての Matrix ルームにあなた、Manager、関連する Worker が参加しています：

```
あなた: @bob 待って、パスワードルールを最低 8 文字に変更して
Bob: 了解、更新しました。
Alice: フロントエンドのバリデーションも更新しました。
```

隠れた Agent 間通信はありません。すべてが可視化され、介入可能です。

## マルチランタイム協調

HiClaw は 3 つの Worker ランタイムをサポートし、**同じ IM ルーム内で共存・協調**できます：

- **OpenClaw**（Node.js）— 豊富なスキルエコシステムを持つ汎用 Agent、タスクオーケストレーションやツール呼び出しに最適
- **QwenPaw**（Python）— 軽量ランタイム、ブラウザ自動化やクイックタスクに適している
- **Hermes**（[hermes-agent](https://github.com/NousResearch/hermes-agent)）— ターミナルサンドボックス、自己改善スキル、永続メモリを備えた自律コーディング Agent

各ランタイムは異なるタスクに優れています。推奨パターン：決定論的な Agent（OpenClaw/QwenPaw）をリーダーとしてタスク分解と割り当てを行い、Hermes Worker に自律的なコード実行を担当させる。すべてのランタイムは同じルーム内で Matrix `m.mentions` を介して通信し、完全に可視で、いつでも介入可能です。

```bash
# 任意の Worker のランタイムをその場で切り替え
hiclaw update worker --runtime hermes
```

## アーキテクチャ

```
┌───────────────────────────────────────────────┐
│            hiclaw-controller                  │
│  Higress │ Tuwunel │ MinIO │ Element Web      │
└──────────────────┬────────────────────────────┘
                   │ Matrix + HTTP Files
┌──────────────────┴──────────┐
│     hiclaw-manager-agent     │
│     Manager (OpenClaw/       │
│       QwenPaw)               │
└──────────────────┬──────────┘
                   │
┌──────────────────┼────────────────────────────┐
│                  │                            │
▼                  ▼                            ▼
Worker Alice    Worker Bob              Worker Charlie
(OpenClaw)      (QwenPaw)               (Hermes)
```

| コンポーネント | 役割 |
|-----------|------|
| hiclaw-controller | Kubernetes ネイティブコントロールプレーン、Worker/Team/Manager CR を調整 |
| Higress AI ゲートウェイ | LLM プロキシ、MCP Server ホスティング、認証情報管理 |
| Tuwunel（Matrix） | すべての Agent + 人間のコミュニケーション用セルフホスト IM サーバー |
| Element Web | ブラウザクライアント、ゼロ設定 |
| MinIO | 一元化ファイルストレージ、Worker はステートレス |
## HiClaw vs OpenClaw ネイティブ

| | OpenClaw ネイティブ | HiClaw |
|---|---|---|
| デプロイ | 単一プロセス | 分散コンテナ |
| Agent 作成 | 手動設定 + 再起動 | 対話形式 |
| 認証情報 | 各 Agent が実際のキーを保持 | Worker はコンシューマートークンのみ保持 |
| 人間の可視性 | オプション | 組み込み（Matrix ルーム） |
| モバイルアクセス | チャネル設定に依存 | 任意の Matrix クライアント、ゼロ設定 |
| 監視 | なし | Manager ハートビート、ルーム内で確認可能 |

## ロードマップ

### ✅ リリース済み

- ~~**QwenPaw（旧CoPaw）** — 軽量 Agent ランタイム~~ [1.0.4 でリリース](blog/hiclaw-1.0.4-release.md): メモリ使用量約 150MB（OpenClaw の約 500MB に対して）、さらにブラウザ自動操作用のローカルホストモードに対応。
- ~~**ユニバーサル MCP サービスサポート** — MCP サーバー統合~~ [1.0.6 でリリース](blog/hiclaw-1.0.6-release.md): 任意の MCP サーバーをゲートウェイ経由で安全に Worker に公開可能。Worker は Higress 発行のトークンのみを使用し、実際の認証情報はゲートウェイの外に出ません。

### 進行中

#### 軽量 Worker ランタイム

- **ZeroClaw** — Rust ベースの超軽量ランタイム。3.4MB バイナリ、コールドスタート 10ms 未満。
- **NanoClaw** — 最小限の OpenClaw 代替。4000 行未満のコード、コンテナベースの分離。

目標: Worker あたりのメモリ使用量を約 500MB から 100MB 未満に削減。

### 計画中

#### チーム管理センター

Agent チームを観察・制御するための組み込みダッシュボード — リアルタイム観察、能動的な中断、タスクタイムライン、リソース監視。

---

## ドキュメント

| | |
|---|---|
| [docs/quickstart.md](docs/quickstart.md) | ステップバイステップガイド |
| [docs/architecture.md](docs/architecture.md) | システムアーキテクチャの詳細 |
| [docs/manager-guide.md](docs/manager-guide.md) | Manager の設定 |
| [docs/worker-guide.md](docs/worker-guide.md) | Worker のデプロイ |
| [docs/development.md](docs/development.md) | コントリビュートとローカル開発 |

## トラブルシューティング

```bash
docker exec -it hiclaw-manager cat /var/log/hiclaw/manager-agent.log
```

よくある問題については [docs/zh-cn/faq.md](docs/zh-cn/faq.md) を参照してください。

### バグ報告

Issue を提出する前に、Matrix メッセージログをエクスポートし、AI ツールでコードベースと照合して分析することをお勧めします。これによりバグの修正が大幅に速くなります。

```bash
# デバッグログのエクスポート（Matrix メッセージ + Agent セッション、PII は自動マスク）
python scripts/export-debug-log.py --range 1h
```

次に、Cursor、Claude Code などの AI ツールで HiClaw リポジトリを開き、以下のように質問してください：

> "debug-log/ 内の JSONL ファイルを読み込み、Matrix メッセージログと Agent セッションログを合わせて分析してください。HiClaw のコードベースと照合し、[バグの内容を記述] の根本原因を特定してください。"

AI の分析結果を [バグレポート](https://github.com/alibaba/hiclaw/issues/new?template=bug_report.yml) に含めてください。

## ビルド & テスト

```bash
make build          # 全イメージをビルド
make test           # ビルド + 全統合テストを実行
make test-quick     # スモークテストのみ
```

## その他のコマンド

```bash
make replay TASK="alice という名前のフロントエンド開発用 Worker を作成して"
make uninstall
make help
```

## コミュニティ

- [Discord](https://discord.gg/NVjNA4BAVw)
- [GitHub Issues](https://github.com/alibaba/hiclaw/issues)

## ライセンス

Apache License 2.0
