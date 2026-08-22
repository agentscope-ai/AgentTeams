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

- 🦞 **カスタマイズ可能な Agent**: 各 Agent は OpenClaw、Copaw、NanoClaw、ZeroClaw、企業独自の Agent など、柔軟な構成をサポートし、個別の「エビ養殖」からフルスケールの「エビ農場」運営まで対応します。

- 📦 **MinIO 共有ファイルシステム**: Agent 間の情報共有のための共有ファイルシステムを導入し、マルチエージェント連携シナリオにおけるトークン消費を大幅に削減します。

- 🔐 **Higress AI ゲートウェイ**: トラフィック管理を一元化し、認証情報に関連するリスクを軽減します。ネイティブの Lobster フレームワークにおけるセキュリティ上の懸念を解消します。

- ☎️ **Element IM クライアント + Tuwunel IM サーバー（共に Matrix プロトコルベース）**: DingTalk/Lark 統合の手間や企業承認ワークフローを排除します。IM 環境でモデルサービスの「快適さ」を素早く体験でき、ネイティブの OpenClaw IM 統合との互換性も維持します。

## ニュース

- **2026-03-14**: HiClaw 1.0.6 — エンタープライズグレードの MCP Server 管理、認証情報のゼロ露出。[ブログ](blog/hiclaw-1.0.6-release.md)
- **2026-03-10**: HiClaw 1.0.4 — CoPaw Worker サポート、メモリ使用量 80% 削減。[ブログ](blog/hiclaw-1.0.4-release.md)
- **2026-03-04**: HiClaw オープンソース化。[アナウンス](blog/hiclaw-announcement.md)

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

**Windows（PowerShell 5+）:**
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

## アーキテクチャ

```
┌─────────────────────────────────────────────┐
│         hiclaw-manager-agent                │
│  Higress │ Tuwunel │ MinIO │ Element Web    │
│  Manager Agent (OpenClaw)                   │
└──────────────────┬──────────────────────────┘
                   │ Matrix + HTTP Files
┌──────────────────┴──────┐  ┌────────────────┐
│  hiclaw-worker-agent    │  │  hiclaw-worker │
│  Worker Alice (OpenClaw)│  │  Worker Bob    │
└─────────────────────────┘  └────────────────┘
```

| コンポーネント | 役割 |
|-----------|------|
| Higress AI ゲートウェイ | LLM プロキシ、MCP Server ホスティング、認証情報管理 |
| Tuwunel（Matrix） | すべての Agent + 人間のコミュニケーション用セルフホスト IM サーバー |
| Element Web | ブラウザクライアント、ゼロ設定 |
| MinIO | 一元化ファイルストレージ、Worker はステートレス |
| OpenClaw | Matrix プラグインとスキルを備えた Agent ランタイム |

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

- ~~**CoPaw** — 軽量 Agent ランタイム~~ [1.0.4 でリリース](blog/hiclaw-1.0.4-release.md): メモリ使用量約 150MB（OpenClaw の約 500MB に対して）、さらにブラウザ自動操作用のローカルホストモードに対応。
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
