# arboretum

*[English README](README.md)*

Apple の [`container`](https://github.com/apple/container) ランタイムを基盤にした
`docker-compose` 互換 CLI。compose ファイルを公式の compose-spec パーサで解析し、
`container` CLI 呼び出しへ変換します。各サービスは**サービス単位のメモリ/CPU 制限**を
持つ軽量 VM で動き、停止中のフットプリントはゼロです。

動機: colima はライフタイム全体で固定 VM（例: 4 GiB）を確保し続けます。Apple
`container` はコンテナごとに 1 VM をサービス単位のサイズ（`--memory 256m --cpus 1`）で
立て、停止時に解放します。arboretum はそのモデルの上に `docker compose` の使い勝手を
載せたものです。

## スコープ

arboretum が狙うのは **ローカルでの開発・検証**です。Mac 上で compose スタックを立ち上げ、
開発し、片付ける、という用途です。本番は通常 Linux（k8s / 素の Docker など）で動くため、
arboretum は本番オーケストレータを目指さず、「ローカルで開発・検証するのに*十分な*
`docker compose` 互換性」を目標にしています。

正確な機能対応は [対応状況 / 注意点 / 未対応](#対応状況--注意点--未対応) を、実装メモは
`docs/STATUS.md` を参照してください。

`--dry-run` で、実行せずに発行される `container` コマンドを確認できます:

```sh
arboretum up --dry-run -f examples/compose.yaml
```

## 必要環境

- container 間ネットワーク / DNS をフルに使うには macOS 26 (Tahoe)。
- Apple `container` がインストール済み（`container` が PATH 上にあること）。Docker は不要。
  未インストールの場合、arboretum が手順を案内します（`--dry-run` は無しでも動きます）。

## インストール

### スクリプト（最新リリース）

```sh
curl -fsSL https://raw.githubusercontent.com/nori-kamiya/arboretum/main/install.sh | sh
```

`~/.local/bin` に入れ（`BINDIR=...` で変更可）、リリースのチェックサムを検証します。

### 手動ダウンロード

[リリースページ](https://github.com/nori-kamiya/arboretum/releases)から Mac 用アーカイブを
取得して:

```sh
tar -xzf arboretum_*_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./arboretum   # バイナリは未公証(notarize)のため
sudo mv arboretum /usr/local/bin/
```

### ソースから

```sh
go install github.com/nori-kamiya/arboretum@latest   # Go 1.26+ が必要
# またはクローンから、バージョン情報を埋め込んでインストール:
make install
```

`arboretum version` で確認できます。

## 使い方

`arbo` は `arboretum` のショートハンドで、相互に同じものです（例: `arbo up -d`）。
以下の例は長い方の名前を使っています。

```sh
arboretum up -d                # ビルド・network/volume 作成・依存順に起動
arboretum up --force-recreate  # 設定が同じでも作り直す
arboretum ps                   # 表形式: SERVICE / NAME / STATE / PORTS
arboretum ps -q                # 名前のみ;  ps --format json でスクリプト連携
arboretum logs --follow        # ログ追尾（色付き・サービス名プレフィックス）
arboretum exec db psql         # 稼働中コンテナでコマンド実行
arboretum run web sh           # サービスの使い捨てワンオフコンテナ
arboretum stop|start|restart   # 既存コンテナを操作（撤去はしない）
arboretum build | pull         # 起動せずにイメージのビルド / 取得
arboretum config               # 解決済み compose を出力（--services, --format json）
arboretum down -v              # コンテナ・network・volume を削除
```

`up` は冪等です: 変更のないコンテナはそのまま、停止中は再起動、設定が変わったサービスは
自動で作り直します（config-hash の差分で判定）。`--force-recreate` は無条件で作り直します。
`down` は、compose ファイルに無くなったサービスのコンテナを `--remove-orphans` を付けない
限り残します。

フラグ: `-f/--file`（複数可）、`-p/--project-name`、`--profile`（複数可）、`--dry-run`。

### サービス間ネットワークと DNS（プロジェクトごとに一度だけ事前設定）

コンテナは `<service>.<project>` という名前で起動します。サービスに**名前でアクセス**する
には（host からでも、別コンテナからでも）、先にそのプロジェクトのローカル DNS ドメインを
**一度だけ**作成する必要があります（`sudo` が必要。`/etc/resolver/<project>` を書くため
で、Apple `container` 側の要件です）:

```sh
sudo container system dns create foo     # プロジェクト名ごとに一度だけ
arboretum up -d -f foo/compose.yaml
```

以降、各サービスに `http://<service>.<project>:<port>` でアクセスできます。コンテナ自身の
ポートに直接届くので、`ports:` の publish も、プロジェクト間の `localhost` ポート衝突も
不要です:

```sh
# プロジェクト "foo" と "bar" を、どちらも :3000 で、同時に:
curl http://web.foo:3000
curl http://api.foo:3000
curl http://web.bar:3000
```

注意:

- **プロジェクト名ごと**に必要で、`sudo` が要るのは `dns create`/`delete` だけです。
  通常の `up`/`down`/`ps`/… は sudo 不要。ドメイン未作成時は `up` が作成コマンドを
  ヒント表示します。
- ドメインが無くてもコンテナは起動し、**IP では**通信できます。名前解決だけが使えません。
- アドレスは `<service>.<project>`（例 `web.foo`）です。**プロジェクト名だけ**（`foo`
  単体）では解決しません。
- DNS/sudo を使いたくない場合は、ホストポートを分けて publish し `localhost` で区別します
  （例 `3000:3000` と `3001:3000` → `localhost:3000` / `:3001`）。

### compose ファイルの探索

`-f` 無しの場合、作業ディレクトリの `compose.yaml` → `compose.yml` →
`docker-compose.yml` → `docker-compose.yaml` をこの順で自動探索し、対応する
`*.override.{yml,yaml}` を上にマージします。`-f`（複数可）で特定ファイルを指定でき、
複数指定時は Docker Compose 同様に左から右へマージされます。

### ビルド（Dockerfile）

サービスの `build:` は `container build` で実行され、生成イメージが `run` に使われます。
そのため Dockerfile の `ENTRYPOINT`/`CMD`/`ENV` などが反映されます。compose のビルド
オプション `dockerfile`・`target`・`args`・`labels` を `container build` に転送します
（`-f` / `--target` / `--build-arg` / `--label`）。

### リソース（CPU / メモリ）

各コンテナが独立した VM なので、サイズ指定が重要です。arboretum は CPU/メモリを次の順で
解決します: `deploy.resources.limits` → 旧式 `mem_limit`/`cpus` →
`deploy.resources.reservations`/`mem_reservation`。例:

```yaml
services:
  db:
    image: postgres:16
    deploy:
      resources:
        limits: { cpus: "2", memory: 1g }   # 旧式なら  cpus: 2 / mem_limit: 1g
```

いずれも未指定のサービス（プレーンな image や Dockerfile ビルドなど）では `--memory`/
`--cpus` を渡さず、`container` 自身のデフォルト（`[container]` システムプロパティ）で
立てます。`container` は**整数 CPU** のみ割り当てるため、小数の `cpus` は切り上げられます。

シェル補完は cobra に内蔵: `arboretum completion zsh > ...`（`bash`/`fish`/`powershell` も）。

### ビルダー管理

Apple `container` は最初のイメージビルド後、長寿命のヘルパーコンテナ（BuildKit ベースの
ビルダー）を稼働させ続けます。これはどの compose プロジェクトにも属さないため、`down` は
触りません（compose のセマンティクスに準拠）。明示的に管理できます:

```sh
arboretum builder status   # ビルダーの状態表示
arboretum builder stop     # 停止（約2GBを解放）
arboretum builder start    # 再起動
arboretum builder delete   # 完全削除

arboretum down --prune-builder   # プロジェクト撤去 + ビルダー停止
```

これらは独立した名前空間に置いています（`docker compose` と `docker builder` の関係と
同じ）。そのため追加しても arboretum は compose CLI の厳密な上位互換のままです。

## 開発

TDD/BDD、**ステートメントカバレッジ 100%** がこのリポジトリの基準です。

```sh
go test ./... -cover                       # パッケージ別カバレッジ（100% 想定）
go test ./... -coverpkg=./... -coverprofile=cover.out && go tool cover -func=cover.out
```

構成:

- `internal/compose` — compose ファイルを `*types.Project` に読み込む（compose-go）。
- `internal/orch` — Project を `container` コマンドへ変換（中核）。
- `internal/backend` — `container` CLI の薄いラッパー（テスト差し替え用シーム付き）。
- `main.go` — cobra の配線（`run()` がテスト可能なエントリポイント）。

テスト用シーム（テストで差し替える変数）: `backend.Bin`、`backend.DryRun`、
`backend.Stdout`、`backend.SetExecForTest`、`osExit`。

よく使うタスクは `Makefile` にあります（`make build` / `make cover` / `make snapshot`）。

## リリース

リリースは [GoReleaser](https://goreleaser.com) と GitHub Actions で行います:

1. 変更を `main` に入れる（CI が `go vet` と 100% カバレッジゲートを強制）。
2. タグを切って push: `git tag v0.1.0 && git push origin v0.1.0`。
3. `.github/workflows/release.yml` が `goreleaser release --clean` を実行し、macOS
   `arm64`/`amd64` バイナリをクロスコンパイル、チェックサムと changelog を生成して
   GitHub Release を公開します。標準の `GITHUB_TOKEN` のみで、追加シークレットは不要。

まずローカルで全体を試走: `make snapshot`（`./dist` に出力、アップロードはしない）。
設定検証は `make release-check`。

Homebrew tap 公開（`brew install nori-kamiya/tap/arboretum`）は `.goreleaser.yaml` に
配線済み（コメントアウト）。`homebrew-tap` リポジトリと `HOMEBREW_TAP_GITHUB_TOKEN`
シークレットを用意したら有効化できます。

## ライセンス

[MIT](LICENSE)。

## 対応状況 / 注意点 / 未対応

Apple `container` 1.0.0 / macOS 26 (arm64) でエンドツーエンド検証済み。

### ✅ できること（docker-compose ライク）

- **コマンド**: `up`（`-d`、`--force-recreate`）、`down`（`-v`、`--remove-orphans`、
  `--prune-builder`）、`ps`（`-q`、`--format json`）、`logs`（`--follow`）、`exec`、
  `run`、`start`/`stop`/`restart`、`build`、`pull`、`config`、`builder`、`version`。
- **ファイル**: `compose.yaml`/`.yml` と `docker-compose.yml`/`.yaml` の探索、
  `*.override.*` マージ、複数 `-f`、`--profile`、`env_file`、`.env`。
- **サービス**: `image`；`build`（context・dockerfile・target・args・labels）；
  `environment`；`ports`（公開ポート）；`volumes`（named + bind）；`depends_on`
  （起動順 + `service_healthy` + `service_completed_successfully`）；CPU/メモリ
  （`deploy.resources.limits` → `mem_limit`/`cpus` → `reservations`）；
  `working_dir`・`user`・`entrypoint`・サービス `labels`。
- **挙動**: config-hash による自動再作成つき冪等 `up`；プロジェクト単位ネットワーク；
  サービス名 DNS；プロジェクト間の名前分離；色付き並行ログと Ctrl-C 一括停止。

### ⚠️ 注意点（若い Apple `container` ランタイム由来。arboretum の限界ではない）

- **macOS 26 (Tahoe) + Apple silicon 専用。**
- **サービス名 DNS には一度だけ `sudo container system dns create <project>` が必要**
  （管理者のみ。未作成時は `up` がヒントを表示）。無くてもコンテナは起動し、IP では
  互いに到達できますが、名前では解決できません。
- **ヘルスチェックはネイティブ非対応** — arboretum が compose の `healthcheck.test` を
  `exec` でポーリングして `service_healthy` を代替します。
- **restart ポリシー非対応** — `restart:` は「非対応」と通知します（arboretum は常駐
  デーモンではなく CLI のため）。
- **`service_completed_successfully`** は依存先が「終了したこと」は確認しますが、
  「終了コード 0」までは確認できません（ランタイムが終了コードを公開しないため）。

### ❌ 未対応

以下の compose 機能は現状**無視されます**（エラーにはなりません）。ローカルで依存すると
本物の Docker 環境と黙って差異が出るため、避けてください:

- **サービスごとの複数ネットワーク**（`networks: [a, b]`）— すべてプロジェクト単位の
  単一ネットワークに参加します。network alias / external / カスタムサブネットも未対応。
- **`secrets` と `configs`。**
- **スケーリング**（`up --scale`、`deploy.replicas`）。
- 高度な `ports`（プロトコル・範囲・ホスト IP）、`extra_hosts`、`cap_add/drop`、
  `devices`、`sysctls`、`tmpfs`、`extends`。
- サブコマンド `cp`、`top`、`kill`、`pause`/`unpause`、`events`。

## Nix

純 Go の単一バイナリなので、`buildGoModule` で flake に載せられます（Swift ベースの
代替と違い）、宣言的管理下に置けます。
