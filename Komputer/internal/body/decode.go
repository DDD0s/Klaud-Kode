package body

import (
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// Console output on Windows is rarely UTF-8: on a Japanese system it is
// cp932, on a Chinese one gbk. Decoding it as UTF-8 regardless is how tools
// end up showing mojibake, so UTF-8 is tried strictly first and the console
// code page is the fallback.
var codepageLabels = map[int]string{
	932: "shift_jis", 936: "gbk", 949: "euc-kr", 950: "big5",
	1200: "utf-16le", 65001: "utf-8",
}

var consoleEncodingOnce struct {
	sync.Once
	value string
}

// DetectConsoleEncoding reports the console encoding label for this host.
func DetectConsoleEncoding() string {
	consoleEncodingOnce.Do(func() {
		consoleEncodingOnce.value = detectConsoleEncoding()
	})
	return consoleEncodingOnce.value
}

func decoderFor(label string) *encoding.Decoder {
	switch label {
	case "shift_jis":
		return japanese.ShiftJIS.NewDecoder()
	case "gbk":
		return simplifiedchinese.GBK.NewDecoder()
	case "euc-kr":
		return korean.EUCKR.NewDecoder()
	case "big5":
		return traditionalchinese.Big5.NewDecoder()
	case "utf-16le":
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
	case "windows-1252":
		return charmap.Windows1252.NewDecoder()
	default:
		return nil
	}
}

func latin1String(buf []byte) string {
	runes := make([]rune, len(buf))
	for i, b := range buf {
		runes[i] = rune(b)
	}
	return string(runes)
}

// Decoded is output text plus the encoding it was decoded with.
type Decoded struct {
	Text     string
	Encoding string
}

// DecodeOutput decodes buf as strict UTF-8, else with fallbackEncoding. An
// unknown or UTF-8 fallback uses a reversible Latin-1 mapping, labelled as
// such, instead of destroying bytes behind replacement characters.
func DecodeOutput(buf []byte, fallbackEncoding string) Decoded {
	if len(buf) == 0 {
		return Decoded{Text: "", Encoding: "utf-8"}
	}
	if utf8.Valid(buf) {
		return Decoded{Text: string(buf), Encoding: "utf-8"}
	}
	if fallbackEncoding == "" {
		fallbackEncoding = DetectConsoleEncoding()
	}
	if fallbackEncoding == "utf-8" || fallbackEncoding == "latin1" {
		return Decoded{Text: latin1String(buf), Encoding: "latin1"}
	}
	decoder := decoderFor(fallbackEncoding)
	if decoder == nil {
		return Decoded{Text: latin1String(buf), Encoding: "latin1"}
	}
	text, _, err := transform.String(decoder, string(buf))
	if err != nil {
		return Decoded{Text: latin1String(buf), Encoding: "latin1"}
	}
	return Decoded{Text: text, Encoding: fallbackEncoding}
}

var codepagePattern = regexp.MustCompile(`([0-9]{3,5})`)

func detectConsoleEncodingPlatform() (string, bool) {
	out, err := exec.Command("chcp.com").Output()
	if err != nil {
		return "windows-1252", true
	}
	// chcp output is ASCII-safe in Latin-1 for the digits we need.
	text := latin1String(out)
	m := codepagePattern.FindStringSubmatch(text)
	if m == nil {
		return "windows-1252", true
	}
	cp, err := strconv.Atoi(m[1])
	if err != nil {
		return "windows-1252", true
	}
	if label, ok := codepageLabels[cp]; ok {
		return label, true
	}
	return "windows-1252", true
}
