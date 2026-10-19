package dang

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type errorContextReadCounter struct {
	context.Context
	reads int
}

func (ctx *errorContextReadCounter) Value(key any) any {
	if _, ok := key.(evalContextKey); ok {
		ctx.reads++
	}
	return ctx.Context.Value(key)
}

func TestSuccessfulNodeDoesNotReadErrorContext(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprint(present), func(t *testing.T) {
			ctx := context.Background()
			if present {
				ctx = WithEvalContext(ctx, NewEvalContext("main.dang", "42"))
			}
			counted := &errorContextReadCounter{Context: ctx}
			for range 10 {
				value, err := EvalNode(counted, nil, &Int{Value: 42})
				if err != nil || value.(IntValue).Val != 42 {
					t.Fatalf("literal evaluation changed: %v: %v", value, err)
				}
			}
			if counted.reads != 0 {
				t.Fatalf("successful evaluation must not fetch error context: %d reads", counted.reads)
			}
		})
	}
}

type errorContextProbeNode struct {
	Node
	location *SourceLocation
	value    Value
	err      error
	observe  func(context.Context)
}

func (node *errorContextProbeNode) GetSourceLocation() *SourceLocation { return node.location }
func (node *errorContextProbeNode) Eval(ctx context.Context, _ ValueScope) (Value, error) {
	if node.observe != nil {
		node.observe(ctx)
	}
	return node.value, node.err
}

// This node exercises the unsupported-node error path rather than Evaluator.
type errorContextUnsupportedNode struct {
	Node
	location *SourceLocation
}

func (node *errorContextUnsupportedNode) GetSourceLocation() *SourceLocation { return node.location }

func TestNodeErrorContextAndExplicitNil(t *testing.T) {
	parent := NewEvalContext("parent.dang", "parent source")
	explicit := NewEvalContext("explicit.dang", "explicit source")
	ctx, cancel := context.WithCancelCause(WithEvalContext(t.Context(), parent))
	cause := errors.New("caller cancellation")
	cancel(cause)
	failure := errors.New("evaluation failed")
	location := &SourceLocation{Filename: "node.dang", Line: 2, Column: 3}
	for _, tc := range []struct {
		name     string
		call     func(context.Context, Node) (Value, error)
		observed *EvalContext
		wrapped  *EvalContext
	}{
		{"inherited", func(ctx context.Context, node Node) (Value, error) { return EvalNode(ctx, nil, node) }, parent, parent},
		{"explicit", func(ctx context.Context, node Node) (Value, error) {
			return EvalNodeWithContext(ctx, nil, node, explicit)
		}, explicit, explicit},
		{"explicit nil", func(ctx context.Context, node Node) (Value, error) { return EvalNodeWithContext(ctx, nil, node, nil) }, parent, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &errorContextProbeNode{location: location, err: failure, observe: func(ctx context.Context) {
				if GetEvalContext(ctx) != tc.observed || context.Cause(ctx) != cause {
					t.Fatal("evaluator context changed")
				}
			}}
			_, err := tc.call(ctx, node)
			if !errors.Is(err, failure) {
				t.Fatalf("lost evaluator error: %v", err)
			}
			if tc.wrapped == nil {
				if err != failure {
					t.Fatalf("explicit nil changed wrapping: %v", err)
				}
				return
			}
			var sourceErr *SourceError
			if !errors.As(err, &sourceErr) || sourceErr.Source != tc.wrapped.Source || sourceErr.Location != location {
				t.Fatalf("source context changed: %#v", err)
			}
		})
	}
	_, err := EvalNode(ctx, nil, &errorContextUnsupportedNode{location: location})
	var sourceErr *SourceError
	if !errors.As(err, &sourceErr) || sourceErr.Source != parent.Source || sourceErr.Location != location {
		t.Fatalf("unsupported-node source context changed: %#v", err)
	}
}

func TestNodeErrorContextPreservesSentinelsAndNilValues(t *testing.T) {
	ctx := WithEvalContext(t.Context(), NewEvalContext("main.dang", "source"))
	location := &SourceLocation{Filename: "main.dang", Line: 1, Column: 1}
	for _, sentinel := range []error{&RaisedError{}, &ReturnException{}, &BreakException{}, &ContinueException{}} {
		_, err := EvalNode(ctx, nil, &errorContextProbeNode{location: location, err: sentinel})
		if err != sentinel {
			t.Fatalf("control flow changed for %T: %T", sentinel, err)
		}
	}
	_, err := EvalNode(ctx, nil, &errorContextProbeNode{location: location})
	var sourceErr *SourceError
	if err == nil || errors.As(err, &sourceErr) {
		t.Fatalf("nil-value validation changed: %v", err)
	}
}

func BenchmarkSuccessfulNodeErrorContext(b *testing.B) {
	for _, depth := range []int{0, 128} {
		for _, present := range []bool{false, true} {
			b.Run(fmt.Sprintf("depth=%d/present=%t", depth, present), func(b *testing.B) {
				ctx := context.Background()
				if present {
					ctx = WithEvalContext(ctx, NewEvalContext("main.dang", "42"))
				}
				for i := range depth {
					ctx = context.WithValue(ctx, i, i)
				}
				node := &Int{Value: 42}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if _, err := EvalNode(ctx, nil, node); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
