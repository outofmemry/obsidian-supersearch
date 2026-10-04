package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// rtfText returns the visible text of an RTF document: control words are
// dropped, \par and \line become newlines, \'hh is Windows-1252, \uN is
// Unicode (skipping its \ucN fallback characters), and destinations that
// hold no body text (fonts, colors, styles, pictures, metadata) are skipped.
// Used where macOS textutil isn't available.
func rtfText(b []byte) string {
	var sb strings.Builder
	type group struct {
		skip bool
		uc   int // fallback chars after \uN
	}
	stack := []group{{uc: 1}}
	pendingSkip := 0 // fallback chars still to drop after a \uN
	cur := func() *group { return &stack[len(stack)-1] }
	emit := func(r rune) {
		if pendingSkip > 0 {
			pendingSkip--
			return
		}
		if !cur().skip {
			sb.WriteRune(r)
		}
	}
	for i := 0; i < len(b); {
		c := b[i]
		switch c {
		case '{':
			stack = append(stack, *cur())
			i++
		case '}':
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			i++
		case '\r', '\n':
			i++
		case '\\':
			i++
			if i >= len(b) {
				break
			}
			switch n := b[i]; {
			case n == '\'' && i+2 < len(b):
				if v, err := strconv.ParseUint(string(b[i+1:i+3]), 16, 8); err == nil {
					emit(cp1252(byte(v)))
				}
				i += 3
			case n == '*': // {\*\dest …}: an optional destination, never body text
				cur().skip = true
				i++
			case n == '~':
				emit(' ')
				i++
			case n == '-' || n == '_':
				i++
			case !isASCIILetter(n): // \{ \} \\ and other escaped symbols
				emit(rune(n))
				i++
			default:
				start := i
				for i < len(b) && isASCIILetter(b[i]) {
					i++
				}
				word := string(b[start:i])
				numStart := i
				if i < len(b) && (b[i] == '-' || (b[i] >= '0' && b[i] <= '9')) {
					i++
					for i < len(b) && b[i] >= '0' && b[i] <= '9' {
						i++
					}
				}
				num, hasNum := 0, i > numStart
				if hasNum {
					num, _ = strconv.Atoi(string(b[numStart:i]))
				}
				if i < len(b) && b[i] == ' ' {
					i++ // the delimiting space belongs to the control word
				}
				switch word {
				case "par", "line", "row", "sect", "page":
					emit('\n')
				case "tab", "cell":
					emit('\t')
				case "uc":
					cur().uc = num
				case "u":
					if num < 0 {
						num += 65536
					}
					emit(rune(num))
					pendingSkip = cur().uc
				case "fonttbl", "colortbl", "stylesheet", "info", "pict", "header", "footer",
					"headerl", "headerr", "footerl", "footerr", "object", "listtable",
					"listoverridetable", "rsidtbl", "xmlnstbl", "themedata", "colorschememapping",
					"datastore", "latentstyles", "generator", "filetbl", "revtbl":
					cur().skip = true
				}
			}
		default:
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError {
				r = rune(c)
				size = 1
			}
			emit(r)
			i += size
		}
	}
	return sb.String()
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// cp1252 decodes one Windows-1252 byte: Latin-1 except for 0x80–0x9F.
func cp1252(c byte) rune {
	if c >= 0x80 && c < 0xa0 {
		if r := cp1252High[c-0x80]; r != 0 {
			return r
		}
	}
	return rune(c)
}

var cp1252High = [32]rune{
	'€', 0, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0, 'Ž', 0,
	0, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0, 'ž', 'Ÿ',
}
