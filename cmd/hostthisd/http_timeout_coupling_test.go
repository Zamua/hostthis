package main

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// The retry-budget arithmetic in internal/storage assumes this outer response
// deadline: attempts x shale-read-budget, plus backoff, must fit inside the
// http.Server WriteTimeout with margin. internal/storage cannot import main,
// so it hardcodes the number; without this pin, changing the real timeout
// leaves that assertion measuring a value the daemon does not use, and still
// passing.
const retryArithmeticAssumesWriteTimeout = 30 * time.Second

func TestHTTPWriteTimeoutMatchesRetryArithmetic(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	m := regexp.MustCompile(`WriteTimeout:\s*(\d+)\s*\*\s*time\.Second`).FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find the http.Server WriteTimeout in main.go; if it moved, " +
			"this coupling test must move with it or the retry arithmetic loses its anchor")
	}
	var secs time.Duration
	for _, c := range m[1] {
		secs = secs*10 + time.Duration(c-'0')
	}
	if got := secs * time.Second; got != retryArithmeticAssumesWriteTimeout {
		t.Fatalf("http.Server WriteTimeout is %v but internal/storage's retry arithmetic "+
			"assumes %v (see outerWriteTimeout in retry_acquiring_test.go). Change both together: "+
			"the read retry must fit inside the response deadline with margin, and a mismatch means "+
			"that pin is asserting against a number the daemon no longer uses.",
			got, retryArithmeticAssumesWriteTimeout)
	}
}
