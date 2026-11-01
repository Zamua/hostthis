package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/zstdenc"
	"github.com/klauspost/compress/zstd"
)

// Screener asks whether an upload may be hosted (docs/SPEC.md "Pre-publish
// screening"). internal/screen satisfies it.
type Screener interface {
	Screen(ctx context.Context, req domain.ScreenRequest) (domain.ScreenVerdict, error)
}

// ErrContentRefused matches every screening refusal.
var ErrContentRefused = errors.New("service: upload refused: it matched a content rule")

// ErrScreenUnavailable refuses an upload the screener could not judge.
var ErrScreenUnavailable = errors.New("service: content screening is unavailable; try again in a few minutes")

// RefusedError names the rule a refused upload matched, and the screener's
// explanation when it gave one.
type RefusedError struct{ Rule, Message string }

func (e *RefusedError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("upload refused: %s (%s)", e.Message, e.Rule)
	}
	return fmt.Sprintf("upload refused: it matched a content rule (%s)", e.Rule)
}

func (e *RefusedError) Is(target error) bool { return target == ErrContentRefused }

const screenTimeout = 20 * time.Second

// ScreenGate applies a Screener to uploads: exempt keys skip it, and any
// answer short of an allow refuses, so screening fails closed. A nil gate
// screens nothing.
type ScreenGate struct {
	Screener Screener
	// Exempt holds key fingerprints (SHA256:...) that skip screening.
	Exempt map[string]bool
	Logger *log.Logger
}

// applies reports whether owner's uploads are screened.
func (g *ScreenGate) applies(owner domain.Owner) bool {
	return g != nil && g.Screener != nil && !g.Exempt[owner.Fingerprint()]
}

func (g *ScreenGate) check(owner domain.Owner, req domain.ScreenRequest) error {
	if !g.applies(owner) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), screenTimeout)
	defer cancel()
	verdict, err := g.Screener.Screen(ctx, req)
	if err != nil {
		if g.Logger != nil {
			g.Logger.Printf("screen: %s upload refused, screener unavailable: %v", req.Shape, err)
		}
		return ErrScreenUnavailable
	}
	if !verdict.Allow {
		if g.Logger != nil {
			g.Logger.Printf("screen: %s upload refused by rule %q", req.Shape, verdict.Rule)
		}
		return &RefusedError{Rule: verdict.Rule, Message: printable(verdict.Message)}
	}
	return nil
}

// checkStaged screens a staged paste body, decoded from its at-rest form
// without moving the file's offset, which the blob write reads from.
func (g *ScreenGate) checkStaged(owner domain.Owner, staged stagedUpload, name string) error {
	if !g.applies(owner) {
		return nil
	}
	encoded := io.NewSectionReader(staged.File, int64(len(zstdenc.Magic)), int64(staged.CompressedSize))
	dec, err := zstd.NewReader(encoded, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return ErrScreenUnavailable
	}
	defer dec.Close()
	return g.check(owner, domain.ScreenRequest{
		Shape:     domain.ScreenDocument,
		Name:      name,
		Truncated: staged.RawSize > domain.ScreenDocumentMax,
		Body:      io.LimitReader(dec, domain.ScreenDocumentMax),
	})
}

// printable keeps a screener's message to one short line of printable text,
// since it reaches the uploader's terminal.
func printable(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return strings.TrimSpace(s)
}
