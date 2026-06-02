# pdf-img-merge

PDFの各ページに分割埋め込みされたJPEG画像を縦結合して、1ページ1枚のJPEGファイルとして出力するツール。

**外部コマンド・CGO・外部ライブラリ不要**のPure Go実装。`go build` 一発でWindows向け単一EXEが生成できる。

---

## 背景・対象とするPDF

PDF作成ツールによっては、1ページに表示される画像を内部で上下2分割して保存することがある。
このようなPDFから一般的な画像抽出ツール（`pdfimages` 等）を使うと、1ページあたり2枚の画像が出力されてしまう。

本ツールはその分割を自動的に検出・結合し、**1ページ→1枚のJPEG**として出力する。

### 対象PDFの条件

| 項目 | 条件 |
|---|---|
| 埋め込み画像フォーマット | **JPEG (DCTDecode)** のみ |
| xref 形式 | 従来型テーブル・xref ストリーム (PDF 1.5+) 両対応 |
| ObjStm (圧縮オブジェクトストリーム) | 対応 |
| 1ページあたり画像数 | 1枚でも2枚以上でも動作（名前昇順に縦結合） |

---

## ビルド

### 前提条件

- Go 1.18 以上（標準ライブラリのみ使用）

### Windowsネイティブ向けビルド

```powershell
cd pdf-img-merge
go build -o pdf-img-merge.exe .
```

### Linux/Mac からWindowsクロスコンパイル

```bash
GOOS=windows GOARCH=amd64 go build -o pdf-img-merge.exe .
```

---

## 使い方

### コマンドライン

```
pdf-img-merge.exe [オプション] input.pdf
```

### ドラッグ&ドロップ

PDFファイルを `pdf-img-merge.exe` のアイコン上にドロップするだけで動作する。
全オプションはデフォルト値で処理される。処理完了後はEnterキーで終了する。

### オプション

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-o <dir>` | PDFと同じディレクトリ | 出力先ディレクトリ |
| `-p <prefix>` | `<PDFファイル名>_page` | 出力ファイル名のプレフィックス |
| `-q <1-100>` | `95` | JPEG出力品質 |

### 出力ファイル名

`<prefix><ゼロ埋めページ番号>.jpg` の形式。

`-p` を省略した場合、入力PDFのファイル名（拡張子なし）に `_page` を付けたものがプレフィックスになる。

| 入力ファイル | デフォルトプレフィックス | 出力例 |
|---|---|---|
| `WA100.pdf` | `WA100_page` | `WA100_page01.jpg` |
| `report.PDF` | `report_page` | `report_page01.jpg` |

ページ数に応じてゼロ埋め桁数が自動決定される。

- 9ページ以下 → `WA100_page01.jpg`, `WA100_page02.jpg`, ...
- 99ページ以下 → `WA100_page01.jpg` ... `WA100_page99.jpg`
- 100ページ以上 → `WA100_page001.jpg` ...

### 実行例

```powershell
# デフォルト: PDFと同じディレクトリに WA100_page01.jpg ... が出力される
pdf-img-merge.exe WA100.pdf

# 出力先ディレクトリを指定（プレフィックスは自動）
pdf-img-merge.exe -o C:\output WA100.pdf

# プレフィックス・品質を明示指定
pdf-img-merge.exe -o C:\output -p photo -q 90 WA100.pdf
```

### 実行時の出力例

```
読み込み完了: WA100.pdf (9462144 bytes)
ページ数: 41
  ページ 1: 2枚結合 → C:\Users\user\Documents\WA100_page01.jpg
  ページ 2: 2枚結合 → C:\Users\user\Documents\WA100_page02.jpg
  ...
  ページ 41: 2枚結合 → C:\Users\user\Documents\WA100_page41.jpg

完了: 41/41ページ処理しました

Enterキーを押すと終了します...
```

---

## 技術概要

### Pure Go PDF パーサ

標準ライブラリのみで以下を実装している。

**xref 解析**
- 従来型テーブル（`xref ... trailer`）
- xref ストリーム（PDF 1.5+、`/Type/XRef`）
- `/Prev` チェーンによる世代管理

**オブジェクト取得**
- type1（直接オブジェクト）: ファイルオフセットから直接読み取り
- type2（圧縮オブジェクト）: ObjStm（`/Type/ObjStm`）を FlateDecode 展開後、ヘッダの `objNum offset` ペアから位置を逆引き
- ObjStm 展開結果はキャッシュし、同一ストリームへの重複アクセスを防ぐ

**ストリームデコード**
- `FlateDecode` → `compress/zlib` で展開
- `DCTDecode` (JPEG) → そのまま返す（再エンコードなし）

**値パーサ**  
`parseValue` / `parseDict` / `parseArray` はすべて `data []byte` を明示的に受け取る純粋関数として実装。ObjStm 内の展開済みバイト列も同じパーサで処理できる。

### 画像結合

1. `image/jpeg` で各JPEGをデコード
2. `image.NewRGBA` で結合後サイズのキャンバスを確保
3. `image/draw` で上から順に貼り付け
4. `image/jpeg` で指定品質で再エンコード

### 画像順序の決定

ページ内の XObject 名（`Image1`, `Image2`, ...）を昇順ソートして結合順とする。  
ページの content stream（描画命令）には依存しない。

---

## 制限事項

- **JPEG以外の画像フォーマットは未対応**  
  JPEG2000 (`JPXDecode`)、CCITT Fax、PNG系 (`FlateDecode` + ColorSpace) 等の画像は抽出をスキップする。

- **JPEG再エンコードによる画質劣化**  
  分割が2枚以上の場合は `image/jpeg` で再エンコードするため、元のJPEGより画質が若干低下する可能性がある。`-q 100` を指定することで劣化を最小限に抑えられる。  
  1枚のみの場合は元のJPEGバイト列をそのまま書き出す。

- **暗号化PDFは非対応**  
  `/Encrypt` を持つパスワード保護されたPDFは解析に失敗する。

- **複雑なページ継承は未対応**  
  ページの `/Resources` が親 Pages ノードから継承されている構造は考慮していない。直接 `/Resources` を持たないページでは画像が抽出されない。

---

## 参考資料

### PDF仕様 公式ドキュメント

- **PDF 32000-1:2008**（ISO公式、PDF 1.7仕様）  
  https://opensource.adobe.com/dc-acrobat-sdk-docs/pdfstandards/PDF32000_2008.pdf  
  xref テーブル・ストリーム・オブジェクト構造・画像XObjectの一次資料。本ツールが参照した仕様の大半はここに基づく。

- **PDF 2.0 (ISO 32000-2)**（最新仕様）  
  https://www.iso.org/standard/75839.html  
  有償だが、Adobe による無償サマリが公開されている:  
  https://developer.adobe.com/document-services/docs/assets/5b15559b96303194340b99820d3a70fa/PDF_ISO_32000-2_PublicPatentLicense.pdf

### PDF内部構造 解説

- **PDF Explained** (O'Reilly, John Whitington著)  
  https://www.oreilly.com/library/view/pdf-explained/9781449321581/  
  PDF構造をゼロから丁寧に解説した書籍。xref・オブジェクト・ストリームの概念理解に最適。

- **PDF Structure** - pdflib.com による技術解説  
  https://www.pdflib.com/pdf-knowledge-base/pdf-document-structure/  
  ページ構造・リソース辞書・XObjectの関係がコンパクトにまとまっている。

- **Cross-Reference Streams** (PDF 1.5+) の詳細解説  
  https://www.pdfa.org/norm-refs/PDF32000_2008.pdf  
  § 7.5.8 に xref ストリームの W フィールド・Index フィールドの仕様が記載されている（上記 PDF32000 の該当箇所）。

- **Zopfli / FlateDecode** — RFC 1950 (zlib) / RFC 1951 (DEFLATE)  
  https://www.rfc-editor.org/rfc/rfc1950  
  ObjStm や xref ストリームで使われる FlateDecode の基礎仕様。
