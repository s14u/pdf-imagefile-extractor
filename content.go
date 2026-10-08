// content.go: ページのコンテンツストリームを走査し、実際に描画される
// 画像 XObject を描画位置順に特定する。
//
// 背景: /Resources は「ページが参照してよい素材の辞書」であって
// 「ページに描かれているもの」ではない。全ページで1つの /Resources を
// 共有するPDF(画像→PDF変換ツールの出力に多い)では、/XObject に
// 全ページ分の画像が並ぶため、辞書を列挙するだけでは他ページの画像まで
// 拾ってしまう。描画されているかどうかは content stream の `Do` 演算子
// でしか判定できない。
package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// matrix は PDF の変換行列 [a b c d e f]。
type matrix [6]float64

var identity = matrix{1, 0, 0, 1, 0, 0}

// mul は m × n を返す(PDF の cm 演算子の意味論: CTM' = M × CTM)。
func (m matrix) mul(n matrix) matrix {
	return matrix{
		m[0]*n[0] + m[1]*n[2],
		m[0]*n[1] + m[1]*n[3],
		m[2]*n[0] + m[3]*n[2],
		m[2]*n[1] + m[3]*n[3],
		m[4]*n[0] + m[5]*n[2] + n[4],
		m[4]*n[1] + m[5]*n[3] + n[5],
	}
}

// apply は点 (x, y) を変換する。
func (m matrix) apply(x, y float64) (float64, float64) {
	return m[0]*x + m[2]*y + m[4], m[1]*x + m[3]*y + m[5]
}

// drawOp は content stream 中の1回の `Do` 呼び出し。
type drawOp struct {
	name  string  // XObject 名 (/I0 なら "I0")
	top   float64 // 描画先矩形の上端 (PDF 座標、上が大きい)
	left  float64 // 描画先矩形の左端
	order int     // 出現順
}

// scanContentDraws は content stream を走査し、`Do` 演算子ごとに
// 描画先の位置を記録して返す。CTM は q/Q/cm を追跡する。
//
// 画像 XObject は単位正方形 (0,0)-(1,1) を CTM で変換した位置に描かれる
// (PDF 32000-1 §8.9.4) ので、4隅を変換して上端・左端を求める。
func scanContentDraws(content []byte) []drawOp {
	var (
		ops      []drawOp
		ctm      = identity
		stack    []matrix
		operands []float64 // 数値オペランド
		lastName string    // 直前の名前オペランド
	)
	clearOperands := func() {
		operands = operands[:0]
		lastName = ""
	}

	pos := 0
	for pos < len(content) {
		pos = skipWS(content, pos)
		if pos >= len(content) {
			break
		}
		b := content[pos]
		switch {
		case b == '/':
			n, next, _ := parseName(content, pos)
			lastName = string(n)
			pos = next
		case b == '(':
			_, next, _ := parseString(content, pos)
			pos = next
		case b == '<' && pos+1 < len(content) && content[pos+1] == '<':
			_, next, _ := parseDict(content, pos)
			pos = next
		case b == '<':
			_, next, _ := parseHexString(content, pos)
			pos = next
		case b == '[' || b == ']' || b == '{' || b == '}':
			pos++
		case isDigit(b) || b == '-' || b == '+' || b == '.':
			start := pos
			pos++
			for pos < len(content) && (isDigit(content[pos]) || content[pos] == '.' ||
				content[pos] == '-' || content[pos] == '+') {
				pos++
			}
			if f, err := strconv.ParseFloat(string(content[start:pos]), 64); err == nil {
				operands = append(operands, f)
			}
		default:
			// 演算子: 空白・区切り文字以外の連続 (q, Q, cm, Do, BI, T*, ' など)
			start := pos
			for pos < len(content) && !isWS(content[pos]) && !isDelim(content[pos]) {
				pos++
			}
			if pos == start {
				pos++ // 想定外のバイトは読み飛ばす
				clearOperands()
				continue
			}
			op := string(content[start:pos])
			switch op {
			case "q":
				stack = append(stack, ctm)
			case "Q":
				if len(stack) > 0 {
					ctm = stack[len(stack)-1]
					stack = stack[:len(stack)-1]
				}
			case "cm":
				if len(operands) >= 6 {
					o := operands[len(operands)-6:]
					ctm = matrix{o[0], o[1], o[2], o[3], o[4], o[5]}.mul(ctm)
				}
			case "Do":
				if lastName != "" {
					top, left := math.Inf(-1), math.Inf(1)
					for _, c := range [][2]float64{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
						x, y := ctm.apply(c[0], c[1])
						top = math.Max(top, y)
						left = math.Min(left, x)
					}
					ops = append(ops, drawOp{name: lastName, top: top, left: left, order: len(ops)})
				}
			case "BI":
				// インライン画像: バイナリ部に偽トークンが含まれ得るので EI まで飛ばす
				pos = skipInlineImage(content, pos)
			}
			clearOperands()
		}
	}
	return ops
}

// skipInlineImage は `BI` 直後の位置から、空白に挟まれた `EI` の直後まで進める。
func skipInlineImage(data []byte, pos int) int {
	for pos+1 < len(data) {
		if data[pos] == 'E' && data[pos+1] == 'I' &&
			(pos == 0 || isWS(data[pos-1])) &&
			(pos+2 >= len(data) || isWS(data[pos+2]) || isDelim(data[pos+2])) {
			return pos + 2
		}
		pos++
	}
	return len(data)
}

// pageContent はページの /Contents(単一 or 配列)を連結して返す。
func pageContent(parser *pdfParser, pageDict pdfDict) []byte {
	var out []byte
	appendStream := func(v interface{}) {
		if r, ok := v.(pdfRef); ok {
			if _, s, err := parser.getObject(int(r)); err == nil {
				out = append(out, s...)
				out = append(out, '\n') // ストリーム境界でトークンが連結しないように
			}
		}
	}
	switch c := pageDict["Contents"].(type) {
	case pdfRef:
		// 参照先が配列の場合もある
		if obj, _, err := parser.getObject(int(c)); err == nil {
			if arr, ok := obj.(pdfArray); ok {
				for _, e := range arr {
					appendStream(e)
				}
				return out
			}
		}
		appendStream(c)
	case pdfArray:
		for _, e := range c {
			appendStream(e)
		}
	}
	return out
}

// pageResources はページの /Resources を返す。ページ自身に無ければ
// /Parent を遡って継承元を探す(PDF 32000-1 §7.7.3.4)。
func pageResources(parser *pdfParser, pageDict pdfDict) (pdfDict, error) {
	node := pageDict
	for depth := 0; node != nil && depth < 64; depth++ {
		res, err := resolveToDict(parser, node["Resources"])
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res, nil
		}
		parent, err := resolveToDict(parser, node["Parent"])
		if err != nil {
			return nil, err
		}
		node = parent
	}
	return nil, nil
}

// extractPageImages は1ページで実際に描画される JPEG 画像を、
// 描画位置(上→下、同じ高さなら左→右)の順に返す。
//
// content stream から `Do` が1つも見つからない場合は従来通り
// /XObject 内の全 JPEG を名前の自然順で返す(フォールバック)。
func extractPageImages(parser *pdfParser, pageObjNum int) ([]imgEntry, error) {
	pageObj, _, err := parser.getObject(pageObjNum)
	if err != nil {
		return nil, fmt.Errorf("page object %d: %w", pageObjNum, err)
	}
	pageDict, ok := pageObj.(pdfDict)
	if !ok {
		return nil, fmt.Errorf("page %d is not a dict", pageObjNum)
	}

	resDict, err := pageResources(parser, pageDict)
	if err != nil || resDict == nil {
		return nil, err
	}
	xobjDict, err := resolveToDict(parser, resDict["XObject"])
	if err != nil || xobjDict == nil {
		return nil, err
	}

	// 名前 → JPEG 画像 の解決(Image かつ DCTDecode 以外は nil)
	loadJPEG := func(name string) *imgEntry {
		r, ok := xobjDict[name].(pdfRef)
		if !ok {
			return nil
		}
		obj, stmData, err := parser.getObject(int(r))
		if err != nil {
			return nil
		}
		d, ok := obj.(pdfDict)
		if !ok || d["Subtype"] != pdfName("Image") || d["Filter"] != pdfName("DCTDecode") {
			return nil
		}
		w, _ := dictInt(d, "Width")
		h, _ := dictInt(d, "Height")
		return &imgEntry{name: name, data: stmData, width: w, height: h}
	}

	draws := scanContentDraws(pageContent(parser, pageDict))
	if len(draws) > 0 {
		// 上端が高い順(PDF 座標は上が大きい)、同一なら左から、同一なら出現順
		sort.SliceStable(draws, func(i, j int) bool {
			const eps = 0.5
			if math.Abs(draws[i].top-draws[j].top) > eps {
				return draws[i].top > draws[j].top
			}
			if math.Abs(draws[i].left-draws[j].left) > eps {
				return draws[i].left < draws[j].left
			}
			return draws[i].order < draws[j].order
		})
		var entries []imgEntry
		for _, d := range draws {
			if e := loadJPEG(d.name); e != nil {
				entries = append(entries, *e)
			}
		}
		return entries, nil
	}

	// フォールバック: 描画命令が読めない場合は /XObject を全列挙
	var entries []imgEntry
	for name := range xobjDict {
		if e := loadJPEG(name); e != nil {
			entries = append(entries, *e)
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return naturalLess(entries[i].name, entries[j].name)
	})
	return entries, nil
}

// naturalLess は "I2" < "I10" となる自然順比較。
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isDigit(a[i]) && isDigit(b[j]) {
			si, sj := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na, _ := strconv.Atoi(a[si:i])
			nb, _ := strconv.Atoi(b[sj:j])
			if na != nb {
				return na < nb
			}
			continue
		}
		if a[i] != b[j] {
			return a[i] < b[j]
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}
