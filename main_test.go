// main_test.go: pdf-img-merge の自動テスト。
//
// ─────────────────────────────────────────────
// Go のテストの基本
// ─────────────────────────────────────────────
//
//   - ファイル名が `_test.go` で終わるファイルは `go build` では無視され、
//     `go test` のときだけコンパイルされる。
//   - `func TestXxx(t *testing.T)` という形の関数が1つのテストケース。
//     `go test ./...` で全部まとめて実行される。
//   - 失敗を報告するには t.Errorf / t.Fatalf を使う。
//     t.Errorf: 失敗を記録して、そのテスト関数の続きも実行する
//     t.Fatalf: 失敗を記録して、そのテスト関数をその場で打ち切る
//     「ここで失敗したら後続の確認に意味がない」ときは Fatalf を使う。
//   - t.Run("名前", func(t *testing.T){...}) でサブテストを作ると、
//     1つのテスト関数の中で複数のケースを個別に報告できる。
//   - package は main.go と同じ `main` にしてある。こうすると
//     extractPageImages など非公開(小文字始まり)の関数も直接呼べる。
//
// 実行方法:
//
//	go test ./...          # 全テスト実行
//	go test -v ./...       # 各テストの名前と結果を表示
//	go test -run Split ./... # 名前に "Split" を含むテストだけ実行
//
// ─────────────────────────────────────────────
// このテストの方針
// ─────────────────────────────────────────────
//
// 実案件のPDFは顧客情報を含むうえサイズも大きいので、リポジトリには置かない。
// 代わりに、問題を再現する最小構造のPDFをテスト内でその場で生成する。
// 「どんな構造のPDFで」「何が起きるべきか」がコードとして残るので、
// 将来バグが再発したときにどこが壊れたか特定しやすい。
//
// 再現する構造は3つ:
//  1. 全ページで /Resources を共有し、各ページは1枚だけ描画する
//     (2026-10 に見つかったバグ: 全ページで全画像を結合してしまった)
//  2. 1ページの画像が上下2分割され、名前順・描画順が位置順と逆
//     (位置順で結合できることの確認)
//  3. /Resources がページではなく親 /Pages に置かれている(継承)
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// ─────────────────────────────────────────────
// フィクスチャ(テスト用データ)生成ヘルパー
// ─────────────────────────────────────────────

// solidJPEG は単色で塗りつぶした w×h の JPEG バイト列を返す。
// 結合結果の「どの位置にどの画像が来たか」を色で判定するために使う。
func solidJPEG(t *testing.T, w, h int, c color.RGBA) []byte {
	// t.Helper() を呼んでおくと、この関数内で t.Fatalf した場合に
	// エラー行として「呼び出し元」の行番号が表示される(デバッグしやすい)。
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("テスト用JPEGの生成に失敗: %v", err)
	}
	return buf.Bytes()
}

// imageXObject は JPEG を埋め込んだ画像 XObject のオブジェクト本体を返す。
// (本体 = "N 0 obj" と "endobj" の間の部分)
func imageXObject(jpegData []byte, w, h int) string {
	return fmt.Sprintf(
		"<< /Type /XObject /Subtype /Image /Width %d /Height %d "+
			"/ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\n"+
			"stream\n%s\nendstream",
		w, h, len(jpegData), jpegData)
}

// contentStream は描画命令(content stream)のオブジェクト本体を返す。
func contentStream(ops string) string {
	return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(ops), ops)
}

// buildPDF はオブジェクト本体の一覧から、xref テーブル付きの完全なPDFを組み立てる。
// objs[0] がオブジェクト番号 1、objs[1] が 2、... になる。
// objs[0] は /Type /Catalog でなければならない(trailer の /Root が 1 0 R 固定のため)。
//
// PDF ファイルの最小構成:
//
//	%PDF-1.4            ← ヘッダ
//	1 0 obj ... endobj  ← オブジェクト列
//	xref                ← 各オブジェクトのファイル内オフセット表
//	trailer             ← ルート(Catalog)への参照
//	startxref           ← xref テーブルの位置
//	%%EOF
func buildPDF(objs []string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")

	// 各オブジェクトを書き出しつつ、開始位置を記録する(xref に必要)
	offsets := make([]int, len(objs)+1) // offsets[0] は未使用(オブジェクト番号は1始まり)
	for i, body := range objs {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}

	// xref テーブル: 1行は必ず20バイト("%010d %05d n \n")
	xrefPos := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(objs)+1)
	out.WriteString("0000000000 65535 f \n") // 0番は常に free エントリ
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}

	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\n", len(objs)+1)
	fmt.Fprintf(&out, "startxref\n%d\n%%%%EOF\n", xrefPos)
	return out.Bytes()
}

// extractAllPages は PDF バイト列を解析し、ページごとに extractPageImages を
// 呼んだ結果をまとめて返す。main() のループ部分と同じ流れを辿る。
func extractAllPages(t *testing.T, pdf []byte) [][]imgEntry {
	t.Helper()
	parser, err := newPDFParser(pdf)
	if err != nil {
		t.Fatalf("PDF解析に失敗: %v", err)
	}
	pages, err := findPageObjects(parser)
	if err != nil {
		t.Fatalf("ページ一覧の取得に失敗: %v", err)
	}
	var result [][]imgEntry
	for i, pg := range pages {
		entries, err := extractPageImages(parser, pg)
		if err != nil {
			t.Fatalf("ページ %d の画像抽出に失敗: %v", i+1, err)
		}
		result = append(result, entries)
	}
	return result
}

// pixelAt は JPEG バイト列をデコードし、(x, y) の色を 0-255 の RGB で返す。
func pixelAt(t *testing.T, jpegData []byte, x, y int) (r, g, b uint8) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		t.Fatalf("JPEGデコードに失敗: %v", err)
	}
	// RGBA() は 0-65535 の16bit値を返すので 8bit に落とす
	r16, g16, b16, _ := img.At(x, y).RGBA()
	return uint8(r16 >> 8), uint8(g16 >> 8), uint8(b16 >> 8)
}

// assertColor は (r,g,b) が期待色に近いことを確認する。
// JPEG は非可逆圧縮なので完全一致ではなく許容誤差つきで比較する。
func assertColor(t *testing.T, label string, r, g, b uint8, want color.RGBA) {
	t.Helper()
	const tolerance = 16
	diff := func(a, b uint8) int {
		if a > b {
			return int(a - b)
		}
		return int(b - a)
	}
	if diff(r, want.R) > tolerance || diff(g, want.G) > tolerance || diff(b, want.B) > tolerance {
		t.Errorf("%s: 色が (%d,%d,%d) だが、期待は (%d,%d,%d) 付近",
			label, r, g, b, want.R, want.G, want.B)
	}
}

// よく使う色
var (
	red   = color.RGBA{255, 0, 0, 255}
	green = color.RGBA{0, 255, 0, 255}
	blue  = color.RGBA{0, 0, 255, 255}
)

// ─────────────────────────────────────────────
// テスト 1: /Resources を全ページで共有するPDF
// ─────────────────────────────────────────────

// TestSharedResources_OnlyDrawnImagesAreExtracted は、
// 全ページが同じ /Resources(画像3枚入り)を参照し、各ページが1枚だけ
// 描画するPDFで、「描画した1枚だけ」が抽出されることを確認する。
//
// 修正前のコードは /XObject 辞書を丸ごと列挙していたため、
// このPDFでは全ページで3枚結合されてしまっていた(2026-10 のバグ)。
func TestSharedResources_OnlyDrawnImagesAreExtracted(t *testing.T) {
	jpgA := solidJPEG(t, 100, 50, red)
	jpgB := solidJPEG(t, 100, 50, green)
	jpgC := solidJPEG(t, 100, 50, blue)

	pdf := buildPDF([]string{
		// 1: Catalog
		"<< /Type /Catalog /Pages 2 0 R >>",
		// 2: Pages(ページツリーの根)
		"<< /Type /Pages /Kids [3 0 R 4 0 R 5 0 R] /Count 3 >>",
		// 3,4,5: 各ページ。全員が同じ Resources(obj 6)を指しているのがポイント
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 50] /Resources 6 0 R /Contents 7 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 50] /Resources 6 0 R /Contents 8 0 R >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 50] /Resources 6 0 R /Contents 9 0 R >>",
		// 6: 共有 Resources。3ページ分の画像が全部ここに並ぶ
		"<< /XObject << /I0 10 0 R /I1 11 0 R /I2 12 0 R >> >>",
		// 7,8,9: 各ページの描画命令。それぞれ1枚だけ Do する
		contentStream("q 100 0 0 50 0 0 cm /I0 Do Q"),
		contentStream("q 100 0 0 50 0 0 cm /I1 Do Q"),
		contentStream("q 100 0 0 50 0 0 cm /I2 Do Q"),
		// 10,11,12: 画像本体
		imageXObject(jpgA, 100, 50),
		imageXObject(jpgB, 100, 50),
		imageXObject(jpgC, 100, 50),
	})

	pages := extractAllPages(t, pdf)

	if len(pages) != 3 {
		t.Fatalf("ページ数が %d だが、期待は 3", len(pages))
	}

	// ページ番号 → そのページで描画されているはずの XObject 名
	wantNames := []string{"I0", "I1", "I2"}
	for i, entries := range pages {
		// サブテストにしておくと、どのページで失敗したかが出力で分かる
		t.Run(fmt.Sprintf("page%d", i+1), func(t *testing.T) {
			if len(entries) != 1 {
				t.Fatalf("抽出画像数が %d 枚だが、期待は 1 枚(共有Resourcesの他画像を拾っていないか?)", len(entries))
			}
			if entries[0].name != wantNames[i] {
				t.Errorf("抽出画像が %s だが、期待は %s", entries[0].name, wantNames[i])
			}
		})
	}
}

// ─────────────────────────────────────────────
// テスト 2: 上下2分割された画像の結合順
// ─────────────────────────────────────────────

// TestSplitImage_MergedInDrawingPositionOrder は、1ページの画像が上下に
// 分割されているとき、「描画位置」の上→下の順で結合されることを確認する。
//
// 意地悪な条件として、XObject 名の順(A < B)も描画命令の出現順(A が先)も
// 位置とは逆にしてある。名前順や出現順で結合すると上下が入れ替わる。
//
//	PDF 座標系は左下が原点で、y は上に行くほど大きい。
//	ページ(高さ200)の下半分 y=0..100 に A(赤)、上半分 y=100..200 に B(青)。
//	期待する結合結果: 上が青、下が赤。
func TestSplitImage_MergedInDrawingPositionOrder(t *testing.T) {
	jpgA := solidJPEG(t, 200, 100, red)  // 下半分
	jpgB := solidJPEG(t, 200, 100, blue) // 上半分

	pdf := buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources 4 0 R /Contents 5 0 R >>",
		"<< /XObject << /A 6 0 R /B 7 0 R >> >>",
		// cm の6数値は [a b c d e f]。画像は単位正方形を a×d に拡大し (e, f) に平行移動して描かれる。
		//   "200 0 0 100 0 0 cm /A Do"   → 幅200 高さ100 を (0,  0) に = 下半分
		//   "200 0 0 100 0 100 cm /B Do" → 幅200 高さ100 を (0,100) に = 上半分
		contentStream("q 200 0 0 100 0 0 cm /A Do Q q 200 0 0 100 0 100 cm /B Do Q"),
		imageXObject(jpgA, 200, 100),
		imageXObject(jpgB, 200, 100),
	})

	pages := extractAllPages(t, pdf)
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("1ページ・2枚のはずが、ページ数=%d", len(pages))
	}

	// 抽出順の確認: B(上) が先、A(下) が後
	if pages[0][0].name != "B" || pages[0][1].name != "A" {
		t.Errorf("抽出順が [%s, %s] だが、期待は [B, A](上→下)", pages[0][0].name, pages[0][1].name)
	}

	// 実際に結合して、画素の色で上下を確認する
	merged, err := mergeImages(pages[0], 90)
	if err != nil {
		t.Fatalf("結合に失敗: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(merged))
	if err != nil {
		t.Fatalf("結合結果のデコードに失敗: %v", err)
	}
	if got := img.Bounds().Size(); got.X != 200 || got.Y != 200 {
		t.Errorf("結合後サイズが %v だが、期待は 200x200", got)
	}
	r, g, b := pixelAt(t, merged, 100, 10) // 上端付近
	assertColor(t, "上端", r, g, b, blue)
	r, g, b = pixelAt(t, merged, 100, 190) // 下端付近
	assertColor(t, "下端", r, g, b, red)
}

// ─────────────────────────────────────────────
// テスト 3: /Resources の継承
// ─────────────────────────────────────────────

// TestInheritedResources_ResolvedFromParentPages は、ページ自身に /Resources が
// 無く、親の /Pages ノードに置かれている場合でも画像を抽出できることを確認する。
// (PDF 仕様 §7.7.3.4 で認められている書き方。修正前は README で「未対応」としていた)
func TestInheritedResources_ResolvedFromParentPages(t *testing.T) {
	jpg := solidJPEG(t, 100, 50, green)

	pdf := buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		// Resources をここ(Pages)に直接書く。子ページには書かない。
		"<< /Type /Pages /Kids [3 0 R] /Count 1 /Resources << /XObject << /Im 5 0 R >> >> >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 50] /Contents 4 0 R >>",
		contentStream("q 100 0 0 50 0 0 cm /Im Do Q"),
		imageXObject(jpg, 100, 50),
	})

	pages := extractAllPages(t, pdf)
	if len(pages) != 1 {
		t.Fatalf("ページ数が %d だが、期待は 1", len(pages))
	}
	if len(pages[0]) != 1 || pages[0][0].name != "Im" {
		t.Errorf("継承 Resources から画像を取れていない: %+v", pages[0])
	}
}

// ─────────────────────────────────────────────
// テスト 4: /Contents が配列のとき
// ─────────────────────────────────────────────

// TestContentsArray_StreamsAreConcatenated は、/Contents が複数ストリームの
// 配列になっている場合に、全ストリームを連結して走査できることを確認する。
// PDF 仕様では配列の各ストリームを連結したものが1つの描画命令列とみなされるので、
// cm と Do が別ストリームに分かれていても正しく位置を追えなければならない。
func TestContentsArray_StreamsAreConcatenated(t *testing.T) {
	jpgA := solidJPEG(t, 100, 50, red)
	jpgB := solidJPEG(t, 100, 50, blue)

	pdf := buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		// Contents を配列にする。obj 5 と obj 6 を順に連結して読む
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Resources 4 0 R /Contents [5 0 R 6 0 R] >>",
		"<< /XObject << /A 7 0 R /B 8 0 R >> >>",
		// ストリーム1: A を下に描き、B 用の cm まで書いて終わる
		contentStream("q 100 0 0 50 0 0 cm /A Do Q q 100 0 0 50 0 50 cm"),
		// ストリーム2: B の Do だけ。前ストリームの cm が効いているはず
		contentStream("/B Do Q"),
		imageXObject(jpgA, 100, 50),
		imageXObject(jpgB, 100, 50),
	})

	pages := extractAllPages(t, pdf)
	if len(pages) != 1 || len(pages[0]) != 2 {
		t.Fatalf("1ページ・2枚のはずが、結果=%+v", pages)
	}
	if pages[0][0].name != "B" || pages[0][1].name != "A" {
		t.Errorf("抽出順が [%s, %s] だが、期待は [B, A]", pages[0][0].name, pages[0][1].name)
	}
}

// ─────────────────────────────────────────────
// テスト 5: Do が無いときのフォールバック
// ─────────────────────────────────────────────

// TestFallback_NoDoOperator_ListsAllImagesInNaturalOrder は、描画命令から
// Do が1つも見つからない場合に、従来通り /XObject の全 JPEG を返し、
// その順序が自然順(I2 < I10)になっていることを確認する。
//
// 修正前は文字列比較だったため I10 < I2 となり、10枚以上で順序が崩れていた。
func TestFallback_NoDoOperator_ListsAllImagesInNaturalOrder(t *testing.T) {
	jpg := solidJPEG(t, 10, 10, red)

	pdf := buildPDF([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 10 10] /Resources 4 0 R /Contents 5 0 R >>",
		// 辞書の記述順はわざとバラバラにしてある
		"<< /XObject << /I10 6 0 R /I2 7 0 R /I1 8 0 R >> >>",
		contentStream("0 0 m 10 10 l S"), // 線を引くだけで Do が無い
		imageXObject(jpg, 10, 10),
		imageXObject(jpg, 10, 10),
		imageXObject(jpg, 10, 10),
	})

	pages := extractAllPages(t, pdf)
	if len(pages) != 1 {
		t.Fatalf("ページ数が %d だが、期待は 1", len(pages))
	}
	var got []string
	for _, e := range pages[0] {
		got = append(got, e.name)
	}
	want := []string{"I1", "I2", "I10"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("フォールバック時の順序が %v だが、期待は %v", got, want)
	}
}

// ─────────────────────────────────────────────
// テスト 6: content stream 走査の細かい挙動(単体テスト)
// ─────────────────────────────────────────────

// TestScanContentDraws は scanContentDraws 単体の挙動を確認する。
// PDF を組み立てず、描画命令の文字列を直接渡せるので、
// 「この命令列でこの位置になるはず」というケースを素早く増やせる。
func TestScanContentDraws(t *testing.T) {
	// テーブル駆動テスト: 入力と期待値の組を並べて、ループで回す Go の定番パターン。
	// ケースを追加したいときは、この配列に1要素足すだけでよい。
	cases := []struct {
		name     string  // サブテスト名(失敗時に表示される)
		content  string  // 入力の描画命令
		wantName string  // 最初の Do の XObject 名
		wantTop  float64 // 最初の Do の上端 y 座標
		wantLeft float64 // 最初の Do の左端 x 座標
		wantN    int     // Do の総数
	}{
		{
			name:     "単純な配置",
			content:  "q 100 0 0 50 10 20 cm /A Do Q",
			wantName: "A", wantTop: 70, wantLeft: 10, wantN: 1,
		},
		{
			name: "cm の連結(平行移動 → 拡大)",
			// 先に (0,100) へ移動し、その座標系の中で 100×50 に拡大。
			// 結果の上端は 100 + 50 = 150。
			content:  "q 1 0 0 1 0 100 cm 100 0 0 50 0 0 cm /A Do Q",
			wantName: "A", wantTop: 150, wantLeft: 0, wantN: 1,
		},
		{
			name: "Q で CTM が戻る",
			// 最初の q〜Q の中の cm は、Q の後には効かない。
			// 2つ目の Do は原点の 1×1 に描かれるので上端は 1。
			content:  "q 100 0 0 50 0 500 cm Q /A Do",
			wantName: "A", wantTop: 1, wantLeft: 0, wantN: 1,
		},
		{
			name: "上下反転(d が負)でも上端を正しく取る",
			// d=-50, f=50 → 単位正方形の y=0 が 50、y=1 が 0 に写る。上端は 50。
			content:  "q 100 0 0 -50 0 50 cm /A Do Q",
			wantName: "A", wantTop: 50, wantLeft: 0, wantN: 1,
		},
		{
			name: "インライン画像のバイナリ部にある Do を拾わない",
			// BI〜EI の間は画像データ。その中に "/X Do" という文字列があっても無視する。
			content:  "BI /W 1 /H 1 ID \x00/X Do\x00 EI q 10 0 0 10 0 0 cm /A Do Q",
			wantName: "A", wantTop: 10, wantLeft: 0, wantN: 1,
		},
		{
			name:     "複数の Do",
			content:  "q 10 0 0 10 0 0 cm /A Do Q q 10 0 0 10 0 10 cm /B Do Q",
			wantName: "A", wantTop: 10, wantLeft: 0, wantN: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ops := scanContentDraws([]byte(tc.content))
			if len(ops) != tc.wantN {
				t.Fatalf("Do の数が %d だが、期待は %d: %+v", len(ops), tc.wantN, ops)
			}
			if tc.wantN == 0 {
				return
			}
			got := ops[0]
			if got.name != tc.wantName {
				t.Errorf("名前が %q だが、期待は %q", got.name, tc.wantName)
			}
			// 浮動小数点は誤差が出るので許容範囲つきで比較する
			const eps = 1e-6
			if d := got.top - tc.wantTop; d > eps || d < -eps {
				t.Errorf("上端が %v だが、期待は %v", got.top, tc.wantTop)
			}
			if d := got.left - tc.wantLeft; d > eps || d < -eps {
				t.Errorf("左端が %v だが、期待は %v", got.left, tc.wantLeft)
			}
		})
	}
}

// ─────────────────────────────────────────────
// テスト 7: 自然順比較(単体テスト)
// ─────────────────────────────────────────────

// TestNaturalLess は naturalLess が「数字部分は数値として」比較することを確認する。
func TestNaturalLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool // a < b であるべきか
	}{
		{"I2", "I10", true},   // 文字列比較だと false になる(これが本題)
		{"I10", "I2", false},  // 逆向き
		{"I1", "I1", false},   // 同じものは「より小さい」ではない
		{"Im1", "Im1a", true}, // 接頭辞は短い方が先
		{"A9", "B1", true},    // 文字が違えば文字で決まる
		{"Image2", "Image10", true},
	}
	for _, tc := range cases {
		if got := naturalLess(tc.a, tc.b); got != tc.want {
			t.Errorf("naturalLess(%q, %q) = %v だが、期待は %v", tc.a, tc.b, got, tc.want)
		}
	}
}
