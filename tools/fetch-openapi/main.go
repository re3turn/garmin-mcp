// Command fetch-openapi downloads the OpenAPI spec of the Garmin Connect
// Personal API.
//
// Garmin does not publish the spec as a file. The Swagger docs page bundles it
// into a JavaScript chunk as JSON.parse('...'), and the chunk name carries a
// hash that changes when Garmin rebuilds the site. This command finds the chunk from
// the page HTML, pulls the string literal out of it and writes it as indented
// JSON.
//
// Usage:
//
//	go run ./tools/fetch-openapi/ [-o api/openapi.json]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	siteURL  = "https://developer.garmin.com"
	pagePath = "/gc-developer-program/personal/swagger-docs/"
)

// chunkRe matches the chunk reference as written in the page, which may be a
// root-relative path, carry a path prefix or be an absolute URL.
var chunkRe = regexp.MustCompile(`[^"'\s=]*/component---src-pages-gc-developer-program-personal-swagger-docs-js-[0-9a-f]+\.js`)

// maxBody caps each download so that a misbehaving server cannot exhaust memory.
const maxBody = 64 << 20

func main() {
	out := flag.String("o", filepath.Join("api", "openapi.json"), "output file (- for stdout)")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "fetch-openapi:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pageURL := siteURL + pagePath
	page, err := get(ctx, pageURL)
	if err != nil {
		return err
	}
	chunk, err := chunkURL(pageURL, page)
	if err != nil {
		return err
	}
	js, err := get(ctx, chunk)
	if err != nil {
		return err
	}
	spec, err := extractSpec(string(js))
	if err != nil {
		return fmt.Errorf("%s: %w", chunk, err)
	}

	if out == "-" {
		_, err = os.Stdout.Write(spec)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, spec, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (from %s)\n", out, chunk)
	return nil
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("GET %s: body larger than %d bytes", url, maxBody)
	}
	return body, nil
}

// chunkURL finds the swagger docs chunk referenced from page and resolves it
// against pageURL.
func chunkURL(pageURL string, page []byte) (string, error) {
	ref := chunkRe.Find(page)
	if ref == nil {
		return "", errors.New("swagger docs chunk not found in page HTML")
	}
	base, err := neturl.Parse(pageURL)
	if err != nil {
		return "", err
	}
	u, err := base.Parse(string(ref))
	if err != nil {
		return "", fmt.Errorf("bad chunk reference %q: %w", ref, err)
	}
	return u.String(), nil
}

// specMarkers are the forms the minifier may give the JSON.parse call, one
// per quote style of the string literal.
var specMarkers = []string{`JSON.parse('{"openapi"`, `JSON.parse("{\"openapi\"`}

// extractSpec finds the JSON.parse('{"openapi"...') call in js and returns the
// spec as indented JSON with a trailing newline. Key order is kept as Garmin
// wrote it so that diffs between fetches stay small.
func extractSpec(js string) ([]byte, error) {
	n, start := 0, -1
	for _, m := range specMarkers {
		if c := strings.Count(js, m); c > 0 {
			n += c
			start = strings.Index(js, m) + len("JSON.parse(")
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("found %d embedded OpenAPI specs, want 1", n)
	}
	raw, err := unquoteJS(js[start:])
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(raw), "", "  "); err != nil {
		return nil, fmt.Errorf("embedded spec is not valid JSON: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// unquoteJS decodes the JavaScript string literal at the start of s, which
// must begin with a single or double quote, and ignores what follows it.
func unquoteJS(s string) (string, error) {
	if s == "" || (s[0] != '\'' && s[0] != '"') {
		return "", errors.New("not a string literal")
	}
	quote := s[0]
	var b strings.Builder
	for i := 1; i < len(s); {
		c := s[i]
		switch {
		case c == quote:
			return b.String(), nil
		case c == '\n' || c == '\r':
			return "", errors.New("unterminated string literal")
		case c != '\\':
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			break
		}
		e := s[i+1]
		i += 2
		switch e {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0':
			// \0 followed by a digit is a legacy octal escape, not NUL.
			if i < len(s) && s[i] >= '0' && s[i] <= '9' {
				return "", errors.New("legacy octal escape not supported")
			}
			b.WriteByte(0)
		case '\n':
			// Line continuation.
		case '\r':
			if i < len(s) && s[i] == '\n' {
				i++
			}
		case 'x':
			if i+2 > len(s) {
				return "", errors.New(`truncated \x escape`)
			}
			v, err := strconv.ParseUint(s[i:i+2], 16, 8)
			if err != nil {
				return "", fmt.Errorf(`bad \x escape: %w`, err)
			}
			b.WriteRune(rune(v))
			i += 2
		case 'u':
			r, n, err := decodeU(s[i:])
			if err != nil {
				return "", err
			}
			i += n
			// Join a UTF-16 surrogate pair written as two \u escapes.
			if utf16.IsSurrogate(r) && strings.HasPrefix(s[i:], `\u`) {
				if r2, n2, err := decodeU(s[i+2:]); err == nil {
					if p := utf16.DecodeRune(r, r2); p != utf8.RuneError {
						r = p
						i += 2 + n2
					}
				}
			}
			// A lone surrogate has no UTF-8 form; WriteRune would turn it into
			// U+FFFD and corrupt the spec without notice.
			if utf16.IsSurrogate(r) {
				return "", fmt.Errorf(`lone surrogate \u%04x`, r)
			}
			b.WriteRune(r)
		default:
			// \' \" \\ and any other character stand for themselves.
			b.WriteByte(e)
		}
	}
	return "", errors.New("unterminated string literal")
}

// decodeU decodes the part of a \u escape after the "u": either four hex
// digits or {hex}. It returns the code point and the bytes consumed.
func decodeU(s string) (rune, int, error) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return 0, 0, errors.New(`unterminated \u{} escape`)
		}
		v, err := strconv.ParseUint(s[1:end], 16, 32)
		if err != nil || v > utf8.MaxRune {
			return 0, 0, fmt.Errorf(`bad \u{} escape %q`, s[:end+1])
		}
		return rune(v), end + 1, nil
	}
	if len(s) < 4 {
		return 0, 0, errors.New(`truncated \u escape`)
	}
	v, err := strconv.ParseUint(s[:4], 16, 16)
	if err != nil {
		return 0, 0, fmt.Errorf(`bad \u escape: %w`, err)
	}
	return rune(v), 4, nil
}
