package domain

import (
	"math"
	"strconv"
	"strings"
)

// ByteRange is one satisfiable range of a body: Length bytes from Start.
type ByteRange struct {
	Start, Length int64
}

// RangeVerdict is what a Range header asks of a body of known size.
type RangeVerdict int

const (
	// RangeIgnored serves the whole body: no header, several ranges, another
	// unit, or a malformed spec.
	RangeIgnored RangeVerdict = iota
	// RangeSatisfiable serves the range (206).
	RangeSatisfiable
	// RangeUnsatisfiable answers 416.
	RangeUnsatisfiable
)

// ParseRange reads a Range header against a body of size bytes (size > 0),
// per docs/SPEC.md "Byte ranges". A number too large to parse counts as past
// any end, so it answers the same as any other number beyond size.
func ParseRange(header string, size int64) (ByteRange, RangeVerdict) {
	unit, spec, found := strings.Cut(header, "=")
	if !found || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return ByteRange{}, RangeIgnored
	}
	spec = strings.TrimSpace(spec)
	first, last, found := strings.Cut(spec, "-")
	if !found || strings.Contains(spec, ",") {
		return ByteRange{}, RangeIgnored
	}
	if first == "" {
		n, ok := rangeNumber(last)
		if !ok {
			return ByteRange{}, RangeIgnored
		}
		if n == 0 {
			return ByteRange{}, RangeUnsatisfiable
		}
		n = min(n, size)
		return ByteRange{Start: size - n, Length: n}, RangeSatisfiable
	}
	start, ok := rangeNumber(first)
	if !ok {
		return ByteRange{}, RangeIgnored
	}
	end := size - 1
	if last != "" {
		e, ok := rangeNumber(last)
		if !ok || e < start {
			return ByteRange{}, RangeIgnored
		}
		end = min(e, end)
	}
	if start >= size {
		return ByteRange{}, RangeUnsatisfiable
	}
	return ByteRange{Start: start, Length: end - start + 1}, RangeSatisfiable
}

// rangeNumber parses an unsigned decimal, saturating on overflow.
func rangeNumber(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return n, true
}
