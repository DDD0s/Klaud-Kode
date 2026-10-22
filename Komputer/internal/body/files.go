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
	".venv": true, "venv": true, ".next": true, "dist": true, ".cache": true,
	".idea": true, ".vs": true, "bin": true, "obj": true,
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
			if !visit(child, rel, entry.IsDir()) {
				return false
			}
			if entry.IsDir() && !ignoredDirs[entry.Name()] {
				if !walk(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	_ = walk(root, 0)
}

// GlobPattern compiles the small glob subset search_files accepts.
func GlobPattern(pattern string) *regexp.Regexp {
	var re strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+2 < len(pattern) && pattern[i+1] == '*' && pattern[i+2] == '/' {
				re.WriteString("(?:.*/)?")
				i += 2
			} else if i+1 < len(pattern) && pattern[i+1] == '*' {
				re.WriteString(".*")
				i++
			} else {
				re.WriteString("[^/]*")
			}
		case '?':
			re.WriteString("[^/]")
		default:
			re.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	flags := "^"
	if IsWindows {
		flags = "(?i)^"
	}
	return regexp.MustCompile(flags + re.String() + "$")
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

// StatPath stats one resolved path, following links.
func StatPath(res Resolved) FileInfoResult {
	out := FileInfoResult{Path: res.Rel, Root: res.RootName}
	info, typ, err := statOne(res.Abs)
	if err != nil {
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

// ReadBytesWindow reads one byte window of a file.
func ReadBytesWindow(abs string, offset, length, max int) ([]byte, int64, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	start := min(int64(offset), info.Size())
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, 0, err
	}
	want := length
	if want > max {
		want = max
	}
	if want < 0 {
		want = 0
	}
	buf, err := io.ReadAll(io.LimitReader(f, int64(want)))
	return buf, info.Size(), err
}

// TextWindow is a decoded line window of one file.
type TextWindow struct {
	Content    string
	Encoding   string
	Truncated  bool
	TotalLines int
	StartLine  int
	EndLine    int
}

func validUTF8Prefix(s string) string {
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ReadTextWindow streams a file by lines, retaining only the requested
// window. Content is the window's lines joined with newlines, decoded like
// command output.
func ReadTextWindow(abs string, startLine, maxLines, maxBytes int, cfg Config) (TextWindow, error) {
	f, err := os.Open(abs)
	if err != nil {
		return TextWindow{}, err
	}
	defer f.Close()
	enc := "utf-8"
	if IsWindows && !cfg.ConsoleUTF8 {
		enc = DetectConsoleEncoding()
	}
	first := max(startLine, 1)
	last := first + maxLines - 1
	reader := bufio.NewReaderSize(f, 65536)
	var kept []string
	keptBytes, truncated, total := 0, false, 0
	for {
		chunk, readErr := reader.ReadBytes(10)
		total++
		if total >= first && total <= last && !truncated {
			line := chunk
			if n := len(line); n > 0 && line[n-1] == 10 {
				line = line[:n-1]
			}
			if n := len(line); n > 0 && line[n-1] == 13 {
				line = line[:n-1]
			}
			text := DecodeOutput(line, enc).Text
			if keptBytes+len(text)+1 > maxBytes {
				if len(kept) == 0 {
					remaining := maxBytes - keptBytes
					if remaining < 0 {
						remaining = 0
					}
					if remaining < len(text) {
						text = validUTF8Prefix(text[:remaining])
					}
					kept = append(kept, text)
				}
				truncated = true
			} else {
				keptBytes += len(text) + 1
				kept = append(kept, text)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return TextWindow{}, readErr
		}
	}
	return TextWindow{
		Content: strings.Join(kept, string([]byte{10})), Encoding: enc,
		Truncated: truncated, TotalLines: total,
		StartLine: first, EndLine: first - 1 + len(kept),
	}, nil
}

// FileDescription describes a file for stat_path and listings.
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

// SearchText streams matching lines from one file. max counts matches.
func SearchText(abs, rel string, re *regexp.Regexp, maxResults int, cfg Config) []map[string]any {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer f.Close()
	enc := "utf-8"
	if IsWindows && !cfg.ConsoleUTF8 {
		enc = DetectConsoleEncoding()
	}
	reader := bufio.NewReaderSize(f, 65536)
	// Binary sniff over the first chunk, through the buffered reader.
	peek, _ := reader.Peek(4096)
	if !isTextBytes(peek) {
		return nil
	}
	var out []map[string]any
	lineNo := 0
	for len(out) < maxResults {
		chunk, readErr := reader.ReadBytes(10)
		lineNo++
		line := chunk
		if n := len(line); n > 0 && line[n-1] == 10 {
			line = line[:n-1]
		}
		if n := len(line); n > 0 && line[n-1] == 13 {
			line = line[:n-1]
		}
		text := DecodeOutput(line, enc).Text
		if re.MatchString(text) {
			runes := []rune(strings.TrimSpace(text))
			if len(runes) > 400 {
				runes = runes[:400]
			}
			out = append(out, map[string]any{"path": rel, "line": lineNo, "text": string(runes)})
		}
		if readErr != nil {
			break
		}
	}
	return out
}

// UTF16Index returns the UTF-16 code-unit index of byte offset in s.
func UTF16Index(s string, byteOffset int) int {
	units := 0
	for _, r := range s[:byteOffset] {
		units += utf16.RuneLen(r)
	}
	return units
}

// SortedEntryNames returns sorted names for one directory listing. It
// exists for tests that need deterministic output.
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
