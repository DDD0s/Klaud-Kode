package body

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

var ignoredDirs = map[string]bool{
	"node_modules": true, ".git": true, ".svn": true, "__pycache__": true,
	".venv": true, "venv": true, ".next": true, "dist": true, "build": true, ".cache": true,
}

var imageTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".webp": "image/webp", ".bmp": "image/bmp", ".ico": "image/x-icon",
}

func formatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func isTextBytes(buf []byte) bool {
	n := min(len(buf), 4096)
	for _, b := range buf[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}

// WalkFiles visits entries under root, skipping vendored and build
// directories, until visit returns false or depth 20 is reached.
func WalkFiles(root string, visit func(abs, rel string, isDir bool) bool) {
	var walk func(dir string, depth int) bool
	walk = func(dir string, depth int) bool {
		if depth > 20 {
			return true
		}
		f, err := os.Open(dir)
		if err != nil {
			return true
		}
		entries, _ := f.ReadDir(-1)
		f.Close()
		for _, entry := range entries {
			child := filepath.Join(dir, entry.Name())
			rel, err := filepath.Rel(root, child)
			if err != nil {
				continue
			}
			if entry.IsDir() && ignoredDirs[entry.Name()] {
				continue
			}
			if !visit(child, rel, entry.IsDir()) {
				return false
			}
			if entry.IsDir() {
				if !walk(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	_ = walk(root, 0)
}

// GlobPattern compiles the small glob subset find_files accepts: ** crosses
// directories, * does not, ? is one character.
func GlobPattern(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(globSource(pattern))
	if err != nil {
		// QuoteMeta output is always valid, so this is unreachable; fall back
		// to a literal match rather than panicking in a tool call.
		return regexp.MustCompile("^" + regexp.QuoteMeta(pattern) + "$")
	}
	return re
}

func globSource(pattern string) string {
	var re strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				re.WriteString(".*")
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
				}
			} else {
				re.WriteString("[^/" + string([]byte{92, 92}) + "]*")
			}
		case '?':
			re.WriteString("[^/" + string([]byte{92, 92}) + "]")
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	flags := "^"
	if IsWindows {
		// Windows paths are case-insensitive and may arrive with either
		// separator, so match both and compare without case.
		flags = "(?i)^"
	}
	return flags + re.String() + "$"
}

// FileInfoResult is one stat result in tool-output shape.
type FileInfoResult struct {
	Path     string  `json:"path"`
	Root     string  `json:"root"`
	Exists   bool    `json:"exists"`
	Type     *string `json:"type"`
	Size     *int64  `json:"size"`
	Mode     *int    `json:"mode"`
	Modified *string `json:"modified"`
	Created  *string `json:"created"`
}

func statOne(abs string) (os.FileInfo, string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "", err
	}
	typ := "other"
	switch {
	case info.IsDir():
		typ = "directory"
	case info.Mode().IsRegular():
		typ = "file"
	}
	return info, typ, nil
}

// StatPath stats one resolved path without following the not-found case.
func StatPath(res Resolved) FileInfoResult {
	out := FileInfoResult{Path: res.Rel, Root: res.RootName}
	info, typ, err := statOne(res.Abs)
	if err != nil || info == nil {
		return out
	}
	out.Exists = true
	out.Type = &typ
	size := info.Size()
	out.Size = &size
	mode := fileMode(info)
	out.Mode = &mode
	modified := formatTime(info.ModTime())
	created := formatTime(birthTime(info))
	out.Modified, out.Created = &modified, &created
	return out
}

// StatFile returns the stat_path tool shape and its not-found error.
func StatFile(res Resolved) (map[string]any, error) {
	info, typ, err := statOne(res.Abs)
	if err != nil {
		return nil, err
	}
	if typ == "directory" {
		typ = "dir"
	}
	return map[string]any{
		"path": res.Rel, "absolute": res.Abs, "type": typ, "size": info.Size(),
		"modified": formatTime(info.ModTime()), "created": formatTime(birthTime(info)), "mode": fileMode(info),
	}, nil
}

// TextFile is one whole decoded text file.
type TextFile struct {
	Text     string
	Encoding string
	Size     int64
}

// ReadTextFile reads a whole file under the size limit, decoded.
func ReadTextFile(abs string, maxBytes int) (TextFile, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return TextFile{}, err
	}
	if info.Size() > int64(maxBytes) {
		return TextFile{}, fmt.Errorf("file is %d bytes, over the %d limit; read it in pieces with read_file offset/limit or use run_command", info.Size(), maxBytes)
	}
	buf, err := os.ReadFile(abs)
	if err != nil {
		return TextFile{}, err
	}
	decoded := DecodeOutput(buf, DetectConsoleEncoding())
	return TextFile{Text: decoded.Text, Encoding: decoded.Encoding, Size: info.Size()}, nil
}

var errInvalidUTF8 = errors.New("invalid utf-8")

// TextWindow is a decoded line window of one file.
type TextWindow struct {
	Content    string
	Encoding   string
	TotalLines int
	FirstLine  int
	LastLine   int
}

// ReadTextWindow streams a file by lines, retaining only the requested
// window. A window whose decoded bytes exceed maxBytes is an error.
func ReadTextWindow(abs string, maxBytes, offset, limit int, hasOffset, hasLimit bool) (TextWindow, error) {
	first := 1
	if hasOffset {
		first = offset
	}
	afterLast := 0 // zero means unbounded
	if hasLimit {
		afterLast = first + limit
	}
	scan := func(encoding string, strict bool) (TextWindow, error) {
		f, err := os.Open(abs)
		if err != nil {
			return TextWindow{}, err
		}
		defer f.Close()
		reader := bufio.NewReaderSize(f, 65536)
		var lines []string
		keptBytes := 0
		lineNumber := 1
		for {
			chunk, readErr := reader.ReadBytes(10)
			selected := lineNumber >= first && (afterLast == 0 || lineNumber < afterLast)
			terminated := len(chunk) > 0 && chunk[len(chunk)-1] == 10
			rawLine := chunk
			if terminated {
				rawLine = rawLine[:len(rawLine)-1]
			}
			if strict && !utf8.Valid(rawLine) {
				return TextWindow{}, errInvalidUTF8
			}
			if selected {
				line := rawLine
				if terminated && len(line) > 0 && line[len(line)-1] == 13 {
					line = line[:len(line)-1]
				}
				text := DecodeOutput(line, encoding).Text
				added := len(text)
				if len(lines) > 0 {
					added++
				}
				keptBytes += added
				if keptBytes > maxBytes {
					return TextWindow{}, fmt.Errorf("requested window exceeds the %d byte limit; use a smaller limit", maxBytes)
				}
				lines = append(lines, text)
			}
			if terminated {
				lineNumber++
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return TextWindow{}, readErr
			}
		}
		return TextWindow{
			Content: strings.Join(lines, string([]byte{10})), Encoding: encoding,
			TotalLines: lineNumber, FirstLine: first, LastLine: first + len(lines) - 1,
		}, nil
	}
	window, err := scan("utf-8", true)
	if errors.Is(err, errInvalidUTF8) {
		fallback := DetectConsoleEncoding()
		if fallback == "utf-8" {
			fallback = "latin1"
		}
		return scan(fallback, false)
	}
	return window, err
}

// BytesWindow is one raw byte window in base64 read shape.
type BytesWindow struct {
	Size       int64  `json:"size"`
	Encoding   string `json:"encoding"`
	ByteOffset int    `json:"byteOffset"`
	ByteLength int    `json:"byteLength"`
	Content    string `json:"content"`
}

// ReadFileBytes reads a raw byte window and base64-encodes it.
func ReadFileBytes(abs string, offset int, length *int, max int) (BytesWindow, error) {
	f, err := os.Open(abs)
	if err != nil {
		return BytesWindow{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return BytesWindow{}, err
	}
	size := info.Size()
	want := size - int64(offset)
	if length != nil {
		want = int64(*length)
	}
	if want > int64(max) {
		want = int64(max)
	}
	if want > size-int64(offset) {
		want = size - int64(offset)
	}
	if want < 0 {
		want = 0
	}
	buf := make([]byte, want)
	bytesRead := 0
	if want > 0 {
		bytesRead, err = f.ReadAt(buf, int64(offset))
		if err != nil && !errors.Is(err, io.EOF) {
			return BytesWindow{}, err
		}
	}
	return BytesWindow{
		Size: size, Encoding: "base64", ByteOffset: offset, ByteLength: bytesRead,
		Content: base64.StdEncoding.EncodeToString(buf[:bytesRead]),
	}, nil
}

// DescribeFile describes a file for listings.
func DescribeFile(res Resolved) map[string]any {
	st := StatPath(res)
	out := map[string]any{"exists": st.Exists, "path": res.Rel, "root": res.RootName}
	if st.Exists {
		out["type"] = *st.Type
		out["size"] = *st.Size
		out["mode"] = *st.Mode
		out["modified"] = *st.Modified
		out["created"] = *st.Created
	}
	return out
}

// SearchHit is one content-search match.
type SearchHit struct {
	File   string
	Line   int
	Text   string
	Before []string
	After  []string
}

func splitLines(text string) []string {
	parts := []string{}
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == 13 {
			parts = append(parts, text[start:i])
			if i+1 < len(text) && text[i+1] == 10 {
				i++
			}
			start = i + 1
		} else if text[i] == 10 {
			parts = append(parts, text[start:i])
			start = i + 1
		}
	}
	return append(parts, text[start:])
}

// SearchTextFile searches one decoded file's lines.
func SearchTextFile(abs, rel string, re *regexp.Regexp, remaining, contextLines, maxFileBytes int) []SearchHit {
	info, err := os.Stat(abs)
	if err != nil || info.Size() > int64(maxFileBytes) {
		return nil
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	text := DecodeOutput(raw, DetectConsoleEncoding()).Text
	if strings.ContainsRune(text, 0) {
		return nil
	}
	lines := splitLines(text)
	var out []SearchHit
	for i, line := range lines {
		if len(out) >= remaining {
			break
		}
		if !re.MatchString(line) {
			continue
		}
		hit := SearchHit{File: rel, Line: i + 1, Text: truncateRunes(line, 500)}
		if contextLines > 0 {
			hit.Before = append([]string{}, lines[max(0, i-contextLines):i]...)
			end := min(len(lines), i+1+contextLines)
			hit.After = append([]string{}, lines[i+1:end]...)
		}
		out = append(out, hit)
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// UTF16Index returns the UTF-16 code-unit index of byte offset in s.
func UTF16Index(s string, byteOffset int) int {
	if byteOffset > len(s) {
		byteOffset = len(s)
	}
	units := 0
	for _, r := range s[:byteOffset] {
		units += utf16.RuneLen(r)
	}
	return units
}

// SortedEntryNames returns sorted names for deterministic tests.
func SortedEntryNames(entries []fs.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// ImageMime returns the mime type for image extensions, or "".
func ImageMime(path string) string {
	return imageTypes[strings.ToLower(filepath.Ext(path))]
}

// EncodeImage reads an image file as base64 when it fits the cap.
func EncodeImage(abs string, maxBytes int) (string, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.Size() > int64(maxBytes) {
		return "", fmt.Errorf("image larger than %d KB", maxBytes/1024)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
