// pdf-img-merge: PDFの各ページに分割埋め込みされたJPEG画像を
// 縦結合して1枚のJPEGとして出力するツール。
// 外部コマンド・CGO不要のPure Go実装。
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────
// PDF 型定義
// ─────────────────────────────────────────────

type (
	pdfDict  map[string]interface{}
	pdfArray []interface{}
	pdfName  string
	pdfRef   int // 間接参照 (オブジェクト番号のみ保持)
)

// xrefEntry はクロスリファレンステーブルの1エントリ。
type xrefEntry struct {
	entryType int   // 0=free 1=normal 2=compressed(ObjStm内)
	offset    int64 // type1: ファイル内オフセット
	objStmID  int   // type2: 所属する ObjStm のオブジェクト番号
}

// ─────────────────────────────────────────────
// PDF パーサ
// ─────────────────────────────────────────────

type pdfParser struct {
	data        []byte
	xref        map[int]xrefEntry
	objStmCache map[int][]byte // 展開済みObjStmバイト列キャッシュ
}

func newPDFParser(data []byte) (*pdfParser, error) {
	p := &pdfParser{
		data:        data,
		xref:        make(map[int]xrefEntry),
		objStmCache: make(map[int][]byte),
	}
	if err := p.parseXRef(); err != nil {
		return nil, fmt.Errorf("xref parse error: %w", err)
	}
	return p, nil
}

// ── xref 解析 ───────────────────────────────

func (p *pdfParser) parseXRef() error {
	tail := p.data
	if len(tail) > 2048 {
		tail = tail[len(tail)-2048:]
	}
	idx := bytes.LastIndex(tail, []byte("startxref"))
	if idx < 0 {
		return fmt.Errorf("startxref not found")
	}
	pos := len(p.data) - len(tail) + idx + len("startxref")
	pos = skipWS(p.data, pos)
	numEnd := pos
	for numEnd < len(p.data) && isDigit(p.data[numEnd]) {
		numEnd++
	}
	xrefOffset, err := strconv.ParseInt(string(p.data[pos:numEnd]), 10, 64)
	if err != nil {
		return fmt.Errorf("startxref value: %w", err)
	}
	return p.parseXRefAt(int(xrefOffset))
}

func (p *pdfParser) parseXRefAt(offset int) error {
	pos := skipWS(p.data, offset)
	if bytes.HasPrefix(p.data[pos:], []byte("xref")) {
		return p.parseXRefTable(pos)
	}
	return p.parseXRefStream(pos)
}

func (p *pdfParser) parseXRefTable(pos int) error {
	pos += 4
	pos = skipWS(p.data, pos)
	for pos < len(p.data) {
		if bytes.HasPrefix(p.data[pos:], []byte("trailer")) {
			pos += 7
			pos = skipWS(p.data, pos)
			dict, _, err := parseDict(p.data, pos)
			if err != nil {
				return err
			}
			if prev, ok := dictInt(dict, "Prev"); ok {
				if err := p.parseXRefAt(prev); err != nil {
					return err
				}
			}
			return nil
		}
		line, next := readLine(p.data, pos)
		parts := strings.Fields(string(line))
		if len(parts) != 2 {
			break
		}
		firstObj, err1 := strconv.Atoi(parts[0])
		count, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			break
		}
		pos = next
		for i := 0; i < count; i++ {
			entryLine, nextLine := readLine(p.data, pos)
			pos = nextLine
			ep := strings.Fields(string(entryLine))
			if len(ep) < 3 {
				continue
			}
			off, _ := strconv.ParseInt(ep[0], 10, 64)
			objNum := firstObj + i
			if _, exists := p.xref[objNum]; !exists {
				t := 0
				if ep[2] == "n" {
					t = 1
				}
				p.xref[objNum] = xrefEntry{entryType: t, offset: off}
			}
		}
	}
	return nil
}

func (p *pdfParser) parseXRefStream(pos int) error {
	obj, streamData, err := p.parseObjectAt(p.data, pos)
	if err != nil {
		return fmt.Errorf("xref stream: %w", err)
	}
	dict, ok := obj.(pdfDict)
	if !ok {
		return fmt.Errorf("xref stream is not a dict")
	}

	wRaw, ok := dict["W"]
	if !ok {
		return fmt.Errorf("xref stream missing /W")
	}
	wArr, ok := wRaw.(pdfArray)
	if !ok || len(wArr) < 3 {
		return fmt.Errorf("xref /W invalid")
	}
	w := [3]int{toInt(wArr[0]), toInt(wArr[1]), toInt(wArr[2])}
	entrySize := w[0] + w[1] + w[2]
	if entrySize == 0 {
		return fmt.Errorf("xref entry size is 0")
	}

	var indexPairs [][2]int
	if idxRaw, ok := dict["Index"]; ok {
		if idxArr, ok := idxRaw.(pdfArray); ok {
			for i := 0; i+1 < len(idxArr); i += 2 {
				indexPairs = append(indexPairs, [2]int{toInt(idxArr[i]), toInt(idxArr[i+1])})
			}
		}
	}
	if len(indexPairs) == 0 {
		size := 0
		if s, ok := dictInt(dict, "Size"); ok {
			size = s
		}
		indexPairs = [][2]int{{0, size}}
	}

	i := 0
	for _, pair := range indexPairs {
		first, count := pair[0], pair[1]
		for j := 0; j < count; j++ {
			start := i * entrySize
			if start+entrySize > len(streamData) {
				break
			}
			entry := streamData[start : start+entrySize]
			i++
			t := readBE(entry, 0, w[0])
			off := readBE(entry, w[0], w[1])
			objNum := first + j
			if _, exists := p.xref[objNum]; !exists {
				switch t {
				case 0:
					p.xref[objNum] = xrefEntry{entryType: 0}
				case 1:
					p.xref[objNum] = xrefEntry{entryType: 1, offset: int64(off)}
				case 2:
					p.xref[objNum] = xrefEntry{entryType: 2, objStmID: off}
				}
			}
		}
	}

	if prev, ok := dictInt(dict, "Prev"); ok {
		return p.parseXRefAt(prev)
	}
	return nil
}

// ── オブジェクト取得 ─────────────────────────

// getObject はオブジェクト番号からオブジェクト値とストリームデータを返す。
func (p *pdfParser) getObject(objNum int) (val interface{}, streamData []byte, err error) {
	entry, ok := p.xref[objNum]
	if !ok {
		return nil, nil, fmt.Errorf("object %d not in xref", objNum)
	}
	switch entry.entryType {
	case 0:
		return nil, nil, fmt.Errorf("object %d is free", objNum)
	case 1:
		return p.parseObjectAt(p.data, int(entry.offset))
	case 2:
		return p.getCompressedObject(objNum, entry.objStmID)
	}
	return nil, nil, fmt.Errorf("unknown entry type %d", entry.entryType)
}

// getCompressedObject は ObjStm 内の targetObjNum を取得する。
func (p *pdfParser) getCompressedObject(targetObjNum, objStmID int) (interface{}, []byte, error) {
	// ObjStm を取得
	stmEntry, ok := p.xref[objStmID]
	if !ok || stmEntry.entryType != 1 {
		return nil, nil, fmt.Errorf("ObjStm %d not found", objStmID)
	}
	stmObj, stmData, err := p.parseObjectAt(p.data, int(stmEntry.offset))
	if err != nil {
		return nil, nil, fmt.Errorf("ObjStm %d: %w", objStmID, err)
	}
	dict, _ := stmObj.(pdfDict)

	// stmData をキャッシュ
	if _, cached := p.objStmCache[objStmID]; !cached {
		p.objStmCache[objStmID] = stmData
	} else {
		stmData = p.objStmCache[objStmID]
	}

	firstOffset, _ := dictInt(dict, "First")
	n, _ := dictInt(dict, "N")

	if firstOffset <= 0 || firstOffset > len(stmData) {
		return nil, nil, fmt.Errorf("ObjStm %d: invalid First=%d", objStmID, firstOffset)
	}

	// ヘッダから targetObjNum のオフセットを探す
	header := strings.Fields(string(stmData[:firstOffset]))
	objOffset := -1
	objEnd := len(stmData) - firstOffset
	for i := 0; i+1 < len(header) && i/2 < n; i += 2 {
		num, _ := strconv.Atoi(header[i])
		off, _ := strconv.Atoi(header[i+1])
		if num == targetObjNum {
			objOffset = off
			if i+3 < len(header) {
				nextOff, _ := strconv.Atoi(header[i+3])
				objEnd = nextOff
			}
			break
		}
	}
	if objOffset < 0 {
		return nil, nil, fmt.Errorf("obj %d not found in ObjStm %d", targetObjNum, objStmID)
	}

	// ObjStm のデータ部分で parseValue を呼ぶ
	// 重要: p.data ではなく stmData を渡す
	objData := stmData[firstOffset+objOffset : firstOffset+objEnd]
	val, _, err2 := parseValue(objData, 0)
	return val, nil, err2
}

// parseObjectAt は指定オフセットから "N G obj ... endobj" を解析する。
// data には p.data を渡す（ファイル内の通常オブジェクト用）。
func (p *pdfParser) parseObjectAt(data []byte, pos int) (val interface{}, streamData []byte, err error) {
	pos = skipWS(data, pos)
	// "N G obj" の3トークンをスキップ
	for i := 0; i < 3; i++ {
		for pos < len(data) && !isWS(data[pos]) && !isDelim(data[pos]) {
			pos++
		}
		pos = skipWS(data, pos)
	}

	val, pos, err = parseValue(data, pos)
	if err != nil {
		return nil, nil, err
	}
	pos = skipWS(data, pos)

	if pos < len(data) && bytes.HasPrefix(data[pos:], []byte("stream")) {
		pos += 6
		if pos < len(data) && data[pos] == '\r' {
			pos++
		}
		if pos < len(data) && data[pos] == '\n' {
			pos++
		}
		d, _ := val.(pdfDict)
		length := 0
		if l, ok := dictInt(d, "Length"); ok {
			length = l
		}
		if pos+length > len(data) {
			length = len(data) - pos
		}
		raw := data[pos : pos+length]
		streamData = decodeStream(d, raw)
	}
	return val, streamData, nil
}

// decodeStream はストリームフィルタを適用する（FlateDecode / DCTDecode のみ）。
func decodeStream(dict pdfDict, raw []byte) []byte {
	if dict == nil {
		return raw
	}
	filter := ""
	if f, ok := dict["Filter"]; ok {
		switch fv := f.(type) {
		case pdfName:
			filter = string(fv)
		case pdfArray:
			if len(fv) > 0 {
				if n, ok := fv[0].(pdfName); ok {
					filter = string(n)
				}
			}
		}
	}
	switch filter {
	case "FlateDecode":
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw
		}
		defer r.Close()
		decoded, err := io.ReadAll(r)
		if err != nil {
			return raw
		}
		return decoded
	case "DCTDecode":
		return raw
	default:
		return raw
	}
}

// ── 値パーサ (すべて data []byte を受け取る純粋関数) ──

// parseValue はバイト列 data の pos から1つの PDF 値を解析する。
func parseValue(data []byte, pos int) (interface{}, int, error) {
	pos = skipWS(data, pos)
	if pos >= len(data) {
		return nil, pos, nil
	}
	switch {
	case data[pos] == '<' && pos+1 < len(data) && data[pos+1] == '<':
		return parseDict(data, pos)
	case data[pos] == '[':
		return parseArray(data, pos)
	case data[pos] == '/':
		return parseName(data, pos)
	case data[pos] == '(':
		return parseString(data, pos)
	case data[pos] == '<':
		return parseHexString(data, pos)
	case bytes.HasPrefix(data[pos:], []byte("true")):
		return true, pos + 4, nil
	case bytes.HasPrefix(data[pos:], []byte("false")):
		return false, pos + 5, nil
	case bytes.HasPrefix(data[pos:], []byte("null")):
		return nil, pos + 4, nil
	default:
		return parseNumberOrRef(data, pos)
	}
}

// parseDict は "<<" から始まる辞書を解析する。
// 終端が ">>" でない場合は "endobj"/"stream" でも終端とする。
func parseDict(data []byte, pos int) (pdfDict, int, error) {
	pos += 2 // "<<"
	dict := make(pdfDict)
	for {
		pos = skipWS(data, pos)
		if pos >= len(data) {
			break
		}
		if pos+1 < len(data) && data[pos] == '>' && data[pos+1] == '>' {
			return dict, pos + 2, nil
		}
		if bytes.HasPrefix(data[pos:], []byte("endobj")) ||
			bytes.HasPrefix(data[pos:], []byte("stream")) {
			return dict, pos, nil
		}
		if data[pos] != '/' {
			pos++
			continue
		}
		name, next, err := parseName(data, pos)
		if err != nil {
			return dict, pos, err
		}
		pos = next
		val, next2, err2 := parseValue(data, pos)
		if err2 != nil {
			return dict, pos, err2
		}
		dict[string(name)] = val
		pos = next2
	}
	return dict, pos, nil
}

func parseArray(data []byte, pos int) (pdfArray, int, error) {
	pos++ // "["
	var arr pdfArray
	for {
		pos = skipWS(data, pos)
		if pos >= len(data) || data[pos] == ']' {
			if pos < len(data) {
				pos++
			}
			return arr, pos, nil
		}
		val, next, err := parseValue(data, pos)
		if err != nil {
			return arr, next, err
		}
		arr = append(arr, val)
		pos = next
	}
}

func parseName(data []byte, pos int) (pdfName, int, error) {
	pos++ // "/"
	start := pos
	for pos < len(data) && !isDelim(data[pos]) && !isWS(data[pos]) {
		pos++
	}
	return pdfName(data[start:pos]), pos, nil
}

func parseString(data []byte, pos int) (string, int, error) {
	pos++ // "("
	depth := 1
	var buf []byte
	for pos < len(data) {
		b := data[pos]
		pos++
		if b == '\\' && pos < len(data) {
			buf = append(buf, data[pos])
			pos++
			continue
		}
		if b == '(' {
			depth++
		} else if b == ')' {
			depth--
			if depth == 0 {
				break
			}
		}
		buf = append(buf, b)
	}
	return string(buf), pos, nil
}

func parseHexString(data []byte, pos int) (string, int, error) {
	pos++ // "<"
	end := bytes.IndexByte(data[pos:], '>')
	if end < 0 {
		return "", pos, fmt.Errorf("hex string not terminated")
	}
	return string(data[pos : pos+end]), pos + end + 1, nil
}

// parseNumberOrRef は数値または "N G R" 形式の間接参照を解析する。
func parseNumberOrRef(data []byte, pos int) (interface{}, int, error) {
	start := pos
	if pos < len(data) && (data[pos] == '-' || data[pos] == '+') {
		pos++
	}
	isFloat := false
	for pos < len(data) && (isDigit(data[pos]) || data[pos] == '.') {
		if data[pos] == '.' {
			isFloat = true
		}
		pos++
	}
	if pos == start || (pos == start+1 && !isDigit(data[start])) {
		return nil, start + 1, nil
	}
	s := string(data[start:pos])
	if isFloat {
		f, err := strconv.ParseFloat(s, 64)
		return f, pos, err
	}
	n1, err := strconv.Atoi(s)
	if err != nil {
		return nil, pos, err
	}

	// "G R" が続くか確認（間接参照）
	savedPos := pos
	pos2 := skipWS(data, pos)
	numStart2 := pos2
	for pos2 < len(data) && isDigit(data[pos2]) {
		pos2++
	}
	if numStart2 == pos2 {
		return n1, savedPos, nil
	}
	pos3 := skipWS(data, pos2)
	if pos3 < len(data) && data[pos3] == 'R' {
		return pdfRef(n1), pos3 + 1, nil
	}
	return n1, savedPos, nil
}

// ── ユーティリティ ───────────────────────────

func skipWS(data []byte, pos int) int {
	for pos < len(data) {
		b := data[pos]
		if b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == 0 {
			pos++
		} else if b == '%' {
			for pos < len(data) && data[pos] != '\n' && data[pos] != '\r' {
				pos++
			}
		} else {
			break
		}
	}
	return pos
}

func isWS(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n' || b == '\f' || b == 0
}
func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isDelim(b byte) bool {
	return b == '(' || b == ')' || b == '<' || b == '>' ||
		b == '[' || b == ']' || b == '{' || b == '}' || b == '/' || b == '%'
}

func readLine(data []byte, pos int) (line []byte, next int) {
	start := pos
	for pos < len(data) && data[pos] != '\n' && data[pos] != '\r' {
		pos++
	}
	line = data[start:pos]
	if pos < len(data) && data[pos] == '\r' {
		pos++
	}
	if pos < len(data) && data[pos] == '\n' {
		pos++
	}
	return line, pos
}

func readBE(data []byte, offset, size int) int {
	if size == 0 {
		return 1
	}
	var buf [8]byte
	copy(buf[8-size:], data[offset:offset+size])
	return int(binary.BigEndian.Uint64(buf[:]))
}

func dictInt(dict pdfDict, key string) (int, bool) {
	if v, ok := dict[key]; ok {
		return toInt(v), true
	}
	return 0, false
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case pdfRef:
		return int(n)
	}
	return 0
}

// ─────────────────────────────────────────────
// 画像抽出ロジック
// ─────────────────────────────────────────────

type imgEntry struct {
	name   string
	data   []byte
	width  int
	height int
}

// extractPageImages は1ページの /XObject から JPEG 画像を名前順に返す。
func extractPageImages(parser *pdfParser, pageObjNum int) ([]imgEntry, error) {
	pageObj, _, err := parser.getObject(pageObjNum)
	if err != nil {
		return nil, fmt.Errorf("page object %d: %w", pageObjNum, err)
	}
	pageDict, ok := pageObj.(pdfDict)
	if !ok {
		return nil, fmt.Errorf("page %d is not a dict", pageObjNum)
	}

	resDict, err := resolveToDict(parser, pageDict["Resources"])
	if err != nil || resDict == nil {
		return nil, err
	}

	xobjDict, err := resolveToDict(parser, resDict["XObject"])
	if err != nil || xobjDict == nil {
		return nil, err
	}

	var entries []imgEntry
	for name, ref := range xobjDict {
		r, ok := ref.(pdfRef)
		if !ok {
			continue
		}
		obj, stmData, err := parser.getObject(int(r))
		if err != nil {
			continue
		}
		d, ok := obj.(pdfDict)
		if !ok {
			continue
		}
		if d["Subtype"] != pdfName("Image") {
			continue
		}
		if d["Filter"] != pdfName("DCTDecode") {
			continue
		}
		w, _ := dictInt(d, "Width")
		h, _ := dictInt(d, "Height")
		entries = append(entries, imgEntry{name: name, data: stmData, width: w, height: h})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].name < entries[j].name
	})
	return entries, nil
}

// resolveToDict は値を dict として解決する（pdfRef を辿る）。
func resolveToDict(parser *pdfParser, v interface{}) (pdfDict, error) {
	if v == nil {
		return nil, nil
	}
	if ref, ok := v.(pdfRef); ok {
		obj, _, err := parser.getObject(int(ref))
		if err != nil {
			return nil, err
		}
		v = obj
	}
	d, _ := v.(pdfDict)
	return d, nil
}

// findPageObjects はカタログからページオブジェクト番号を順番に収集する。
func findPageObjects(parser *pdfParser) ([]int, error) {
	catalogNum := -1
	for objNum, entry := range parser.xref {
		if entry.entryType == 0 {
			continue
		}
		obj, _, err := parser.getObject(objNum)
		if err != nil {
			continue
		}
		d, ok := obj.(pdfDict)
		if !ok {
			continue
		}
		if d["Type"] == pdfName("Catalog") {
			catalogNum = objNum
			break
		}
	}
	if catalogNum < 0 {
		return nil, fmt.Errorf("catalog not found")
	}

	catalogObj, _, err := parser.getObject(catalogNum)
	if err != nil {
		return nil, err
	}
	catalog, _ := catalogObj.(pdfDict)

	pagesRef, ok := catalog["Pages"]
	if !ok {
		return nil, fmt.Errorf("catalog has no /Pages")
	}
	r, ok := pagesRef.(pdfRef)
	if !ok {
		return nil, fmt.Errorf("/Pages is not a ref")
	}

	var pages []int
	if err := collectPages(parser, int(r), &pages); err != nil {
		return nil, err
	}
	return pages, nil
}

func collectPages(parser *pdfParser, nodeNum int, pages *[]int) error {
	obj, _, err := parser.getObject(nodeNum)
	if err != nil {
		return err
	}
	d, ok := obj.(pdfDict)
	if !ok {
		return fmt.Errorf("page node %d is not a dict", nodeNum)
	}
	switch d["Type"] {
	case pdfName("Pages"):
		kids, ok := d["Kids"].(pdfArray)
		if !ok {
			return fmt.Errorf("Pages node %d: /Kids is not an array", nodeNum)
		}
		for _, kid := range kids {
			if r, ok := kid.(pdfRef); ok {
				if err := collectPages(parser, int(r), pages); err != nil {
					return err
				}
			}
		}
	case pdfName("Page"):
		*pages = append(*pages, nodeNum)
	}
	return nil
}

// ─────────────────────────────────────────────
// 画像結合
// ─────────────────────────────────────────────

func mergeImages(entries []imgEntry, quality int) ([]byte, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("no images")
	}
	if len(entries) == 1 {
		return entries[0].data, nil
	}

	imgs := make([]image.Image, len(entries))
	totalHeight := 0
	maxWidth := 0
	for i, e := range entries {
		img, err := jpeg.Decode(bytes.NewReader(e.data))
		if err != nil {
			return nil, fmt.Errorf("JPEG decode %s: %w", e.name, err)
		}
		imgs[i] = img
		b := img.Bounds()
		totalHeight += b.Dy()
		if b.Dx() > maxWidth {
			maxWidth = b.Dx()
		}
	}

	combined := image.NewRGBA(image.Rect(0, 0, maxWidth, totalHeight))
	draw.Draw(combined, combined.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)

	yOffset := 0
	for _, img := range imgs {
		b := img.Bounds()
		dst := image.Rect(0, yOffset, b.Dx(), yOffset+b.Dy())
		draw.Draw(combined, dst, img, b.Min, draw.Src)
		yOffset += b.Dy()
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, combined, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("JPEG encode: %w", err)
	}
	return buf.Bytes(), nil
}

// ─────────────────────────────────────────────
// main
// ─────────────────────────────────────────────

func main() {
	var (
		outputDir = flag.String("o", "", "出力ディレクトリ (省略時: PDFと同じディレクトリ)")
		prefix    = flag.String("p", "", "出力ファイル名プレフィックス (省略時: <PDFファイル名>_page)")
		quality   = flag.Int("q", 95, "JPEG品質 (1-100)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "使い方: pdf-img-merge [オプション] input.pdf\n\n")
		fmt.Fprintf(os.Stderr, "  PDFの各ページに分割埋め込みされたJPEG画像を縦結合して出力します。\n\n")
		fmt.Fprintf(os.Stderr, "オプション:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n例:\n")
		fmt.Fprintf(os.Stderr, "  pdf-img-merge input.pdf              # → WA100_page01.jpg ...\n")
		fmt.Fprintf(os.Stderr, "  pdf-img-merge -o ./images -p photo -q 90 input.pdf\n")
	}
	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	inputPath := flag.Arg(0)

	// -o 未指定の場合は PDF と同じディレクトリをデフォルトとする
	if *outputDir == "" {
		*outputDir = filepath.Dir(inputPath)
	}

	// -p 未指定の場合は "<PDFファイル名>_page" をデフォルトとする
	// 例: WA100.pdf → WA100_page
	if *prefix == "" {
		base := filepath.Base(inputPath)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		*prefix = stem + "_page"
	}

	data, err := os.ReadFile(inputPath)
	if err != nil {
		fatalf("PDFファイルの読み込みに失敗しました: %v", err)
	}
	fmt.Printf("読み込み完了: %s (%d bytes)\n", filepath.Base(inputPath), len(data))

	parser, err := newPDFParser(data)
	if err != nil {
		fatalf("PDF解析に失敗しました: %v", err)
	}

	pages, err := findPageObjects(parser)
	if err != nil {
		fatalf("ページ一覧の取得に失敗しました: %v", err)
	}
	fmt.Printf("ページ数: %d\n", len(pages))

	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fatalf("出力ディレクトリの作成に失敗しました: %v", err)
	}

	digits := len(strconv.Itoa(len(pages)))
	if digits < 2 {
		digits = 2
	}
	format := fmt.Sprintf("%%s%%0%dd.jpg", digits)

	successCount := 0
	for i, pageObjNum := range pages {
		pageNum := i + 1
		entries, err := extractPageImages(parser, pageObjNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ページ %d: 画像抽出エラー: %v\n", pageNum, err)
			continue
		}
		if len(entries) == 0 {
			fmt.Printf("  ページ %d: 画像なし (スキップ)\n", pageNum)
			continue
		}

		merged, err := mergeImages(entries, *quality)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ページ %d: 画像結合エラー: %v\n", pageNum, err)
			continue
		}

		outPath := filepath.Join(*outputDir, fmt.Sprintf(format, *prefix, pageNum))
		if err := os.WriteFile(outPath, merged, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "  ページ %d: 書き込みエラー: %v\n", pageNum, err)
			continue
		}
		fmt.Printf("  ページ %d: %d枚結合 → %s\n", pageNum, len(entries), outPath)
		successCount++
	}

	fmt.Printf("\n完了: %d/%dページ処理しました\n", successCount, len(pages))
	pause()
}

// pause はウィンドウが即座に閉じないよう、Enterキー待ちにする。
// ターミナルから起動した場合は何もしない。
// ドラッグ&ドロップ／ダブルクリック起動時のみ待機する。
func pause() {
	fmt.Print("\nEnterキーを押すと終了します...")
	fmt.Scanln()
}


func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "エラー: "+format+"\n", args...)
	pause()
	os.Exit(1)
}
