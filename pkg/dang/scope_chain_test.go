package dang

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vito/dang/v2/pkg/hm"
)

// prepareScopeChainProgram parses, infers and evaluates every form of src but
// the last, returning what's needed to (re-)evaluate the last one.
func prepareScopeChainProgram(tb testing.TB, src string) (context.Context, ValueScope, Node) {
	tb.Helper()

	parsed, err := ParseWithRecovery("scope_chain_test", []byte(src))
	require.NoError(tb, err, "parse")
	file, ok := parsed.(*FileBlock)
	require.True(tb, ok, "unexpected parse result %T", parsed)

	ctx := context.Background()
	typeScope, valueScope := BuildScopesFromImports("", nil)
	_, err = InferFormsWithPhases(ctx, file.Forms, typeScope, hm.NewSimpleFresher())
	require.NoError(tb, err, "infer")

	ctx = WithEvalContext(ctx, NewEvalContext("scope_chain_test", src))
	last := len(file.Forms) - 1
	for _, node := range file.Forms[:last] {
		_, err := EvalNode(ctx, valueScope, node)
		require.NoError(tb, err)
	}
	return ctx, valueScope, file.Forms[last]
}

// Self-recursive methods in their three shapes: a naked self-call, an
// explicit `self.` call, and one that writes a field on every level.
const scopeChainTypes = `
type Rec {
  n: Int! = 0

  naked(k: Int!): Rec! { if (k <= 0) { self } else { naked(k - 1) } }
  explicit(k: Int!): Rec! { if (k <= 0) { self } else { self.explicit(k - 1) } }
}

type Bumper {
  value: Int! = 0

  up(k: Int!): Bumper! { if (k <= 0) { self } else { value += 1; up(k - 1) } }
  setUp(k: Int!): Bumper! {
    if (k <= 0) { self } else { self.value = self.value + 1; self.setUp(k - 1) }
  }
}
`

var scopeChainCases = []struct {
	name string
	call string
}{
	{"naked", "Rec(0).naked(%d)"},
	{"explicit", "Rec(0).explicit(%d)"},
	{"bare-write", "Bumper(0).up(%d)"},
	{"self-write", "Bumper(0).setUp(%d)"},
}

// parentDepth counts the Parent links below an Object.
func parentDepth(o *Object) int {
	depth := 0
	for p := o.Parent; p != nil; p = p.Parent {
		depth++
	}
	return depth
}

// Each method call forks `self` into a fresh copy-on-write generation. A
// self-recursive method forks the fork it was called on, so without
// compaction every level stacked another Parent layer onto the receiver and
// every lookup of a name living on the instance (the method itself, fields,
// builtins) walked the whole stack: quadratic time and linear Go stack.
func TestSelfRecursionKeepsReceiverChainBounded(t *testing.T) {
	const depth = 1000
	for _, c := range scopeChainCases {
		t.Run(c.name, func(t *testing.T) {
			ctx, scope, node := prepareScopeChainProgram(t, scopeChainTypes+fmt.Sprintf(c.call, depth))
			val, err := EvalNode(ctx, scope, node)
			require.NoError(t, err)
			obj, ok := val.(*Object)
			require.True(t, ok, "expected an object, got %T", val)
			require.LessOrEqual(t, parentDepth(obj), 2,
				"receiver chain grew with recursion depth")
		})
	}
}

// A compacted fork resolves names exactly as a fork stacked on its receiver
// would, parents onto the first non-generation layer, and keeps writes local.
func TestForkReceiverCompactsGenerations(t *testing.T) {
	ctx := context.Background()
	root := NewObject(NewType("Root", ObjectKind))
	root.Bind("x", IntValue{Val: 1}, PublicVisibility)
	root.Bind("w", IntValue{Val: 0}, PublicVisibility)

	gen1 := forkReceiver(root)
	gen1.Update("y", IntValue{Val: 10})
	gen2 := deriveGeneration(gen1)
	gen2.Bind("x", IntValue{Val: 2}, PrivateVisibility)
	forced := 0
	gen2.BindLazy("z", func(context.Context) (Value, error) {
		forced++
		return IntValue{Val: 3}, nil
	}, PublicVisibility)
	// A pending further up is shadowed by a nearer value.
	gen1.(*Object).BindLazy("w", func(context.Context) (Value, error) {
		t.Fatal("shadowed initializer forced")
		return nil, nil
	}, PublicVisibility)
	gen2.Bind("w", IntValue{Val: 4}, PublicVisibility)

	fork := forkReceiver(gen2).(*Object)
	require.Same(t, root, fork.Parent, "fork should parent onto the instance")
	require.True(t, fork.IsForked)

	for name, want := range map[string]int{"x": 2, "y": 10, "z": 3, "w": 4} {
		val, found, err := fork.Lookup(ctx, name)
		require.NoError(t, err)
		require.True(t, found, name)
		require.Equal(t, IntValue{Val: want}, val, name)
	}
	require.Equal(t, PrivateVisibility, fork.Visibility("x"))

	// The initializer is shared with gen2, so it still runs once.
	val, _, err := gen2.Lookup(ctx, "z")
	require.NoError(t, err)
	require.Equal(t, IntValue{Val: 3}, val)
	require.Equal(t, 1, forced)

	fork.Update("x", IntValue{Val: 5})
	got, _, _ := gen2.Lookup(ctx, "x")
	require.Equal(t, IntValue{Val: 2}, got, "fork write leaked into its receiver")
	got, _ = root.LookupLocal("x")
	require.Equal(t, IntValue{Val: 1}, got, "fork write leaked into the instance")
}

// BenchmarkSelfRecursion times self-recursive methods at two depths, so a
// cost that grows faster than the depth shows up as a ratio well above 4.
func BenchmarkSelfRecursion(b *testing.B) {
	for _, c := range scopeChainCases {
		for _, depth := range []int{1000, 4000} {
			b.Run(fmt.Sprintf("%s/depth=%d", c.name, depth), func(b *testing.B) {
				ctx, scope, node := prepareScopeChainProgram(b, scopeChainTypes+fmt.Sprintf(c.call, depth))
				b.ResetTimer()
				for b.Loop() {
					if _, err := EvalNode(ctx, scope, node); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
