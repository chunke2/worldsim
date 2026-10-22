package characteragent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const maxPrivateFileBytes = 64 << 20

func privateHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func validDigest(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 32 && id == strings.ToLower(id)
}

// Paths are fixed by trusted callers, never taken from private payloads.
func privatePath(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || relative == "." || strings.HasPrefix(filepath.Clean(relative), "..") {
		return "", errors.New("invalid private data path")
	}
	current := root
	for _, part := range strings.Split(filepath.Clean(relative), string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return "", errors.New("cannot inspect private data path")
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("private data paths must not be symbolic links")
		}
	}
	return current, nil
}

func readPrivateFile(root, relative string) ([]byte, error) {
	path, err := privatePath(root, relative)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPrivateFileBytes+1))
	if err != nil || len(b) > maxPrivateFileBytes {
		return nil, errors.New("private file cannot be read within size limit")
	}
	return b, nil
}

// Validate duplicate keys at every nesting level before decoding into maps.
func checkUniqueJSON(b []byte) error {
	if !utf8.Valid(b) {
		return errors.New("invalid JSON encoding")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var walk func(int) error
	bad := errors.New("invalid, duplicate-key or excessively nested JSON")
	walk = func(depth int) error {
		if depth > 64 {
			return bad
		}
		token, err := d.Token()
		if err != nil {
			return bad
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					token, err := d.Token()
					key, ok := token.(string)
					if err != nil || !ok || keys[key] {
						return bad
					}
					keys[key] = true
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				if token, err := d.Token(); err != nil || token != json.Delim('}') {
					return bad
				}
			case '[':
				for d.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
				if token, err := d.Token(); err != nil || token != json.Delim(']') {
					return bad
				}
			default:
				return bad
			}
		}
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return bad
	}
	return nil
}
