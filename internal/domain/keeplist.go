package domain

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// KeepListName is the archive file naming base files a redeploy carries
// unchanged (docs/SPEC.md "Redeploy reuse"). It is never stored or served.
const KeepListName = ".hostthis-keep"

// MaxKeepListBytes bounds the keep list read: a line per file, each path
// within MaxSitePathLen.
const MaxKeepListBytes = MaxSiteFiles * (MaxSitePathLen + 72)

// ErrBadKeepList refuses a deploy whose keep list cannot be honoured exactly.
var ErrBadKeepList = errors.New("keep list does not match the site")

// KeepList is a parsed .hostthis-keep: the base version it was computed
// against and the digest of each path to carry.
type KeepList struct {
	Base  int
	Paths map[string]string
}

// ParseKeepList reads `base <n>` then `<digest>  <path>` lines, the format
// `sha256sum` prints. An escaped line (leading backslash) is refused, as is a
// repeated path or any other malformed line.
func ParseKeepList(r io.Reader) (KeepList, error) {
	sc := bufio.NewScanner(io.LimitReader(r, MaxKeepListBytes+1))
	sc.Buffer(make([]byte, 0, 4096), MaxSitePathLen+128)
	bad := func(format string, a ...any) (KeepList, error) {
		return KeepList{}, fmt.Errorf("%w: "+format, append([]any{ErrBadKeepList}, a...)...)
	}
	if !sc.Scan() {
		return bad("missing base line")
	}
	n, ok := strings.CutPrefix(sc.Text(), "base ")
	base, err := strconv.Atoi(n)
	if !ok || err != nil || base < 1 {
		return bad("first line must be base <version>")
	}
	kl := KeepList{Base: base, Paths: map[string]string{}}
	read := len(sc.Text()) + 1
	for sc.Scan() {
		line := sc.Text()
		read += len(line) + 1
		if read > MaxKeepListBytes {
			return bad("longer than %d bytes", MaxKeepListBytes)
		}
		if line == "" {
			continue
		}
		digest, path, ok := strings.Cut(line, "  ")
		if !ok || strings.HasPrefix(line, `\`) || !isHexDigest(digest) || path == "" {
			return bad("malformed line %q", line)
		}
		path = strings.TrimPrefix(path, "./")
		if _, dup := kl.Paths[path]; dup {
			return bad("%s listed twice", path)
		}
		kl.Paths[path] = digest
	}
	if err := sc.Err(); err != nil {
		return bad("%v", err)
	}
	return kl, nil
}

// KeepablePath reports whether a path can appear in a keep list unescaped.
func KeepablePath(p string) bool {
	return !strings.ContainsAny(p, "\\\n")
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
