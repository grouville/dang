package dang

import (
	"fmt"
	"strings"
	"testing"
)

// Filtering test-search results exercises one block per match and another for
// each selected name. Parsing and type checking are outside the measured loop.
func BenchmarkFilterBlocks(b *testing.B) {
	for _, count := range []int{1, 128} {
		b.Run(fmt.Sprintf("matches=%d", count), func(b *testing.B) {
			items := make([]string, count)
			for i := range items {
				items[i] = fmt.Sprintf("%q", fmt.Sprintf("file%02d:TestCase%d", i%16, i))
			}
			source := "let matches = [" + strings.Join(items, ",") + "]\n" +
				`matches.filter { name => name.hasPrefix("file00:") }.map { name => name.trimPrefix("file00:") }.length`
			ctx, scope, node := prepareScopeChainProgram(b, source)
			want := (count + 15) / 16
			b.ReportAllocs()
			for b.Loop() {
				value, err := EvalNode(ctx, scope, node)
				if err != nil {
					b.Fatal(err)
				}
				if got := value.(IntValue).Val; got != want {
					b.Fatalf("selected %d names, want %d", got, want)
				}
			}
		})
	}
}
