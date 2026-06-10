// Tiny helper that pipes stdin -> render.Markdown -> stdout.
// Used by smoke + visual-check tooling; not shipped in the runtime image.
package main

import (
	"fmt"
	"github.com/Zamua/hostthis/internal/render"
	"io"
	"os"
)

func main() {
	src, _ := io.ReadAll(os.Stdin)
	out, err := render.Markdown(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(out)
}
