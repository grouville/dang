package dang

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vito/dang/v2/pkg/hm"
)

func callbackTestFunction(t testing.TB) (ValueScope, Value) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.dang"), []byte("pub increment(value: Int!): Int! { value + 1 }"), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := RunDir(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	fn, found, err := env.Lookup(context.Background(), "increment")
	if err != nil || !found {
		t.Fatalf("function lookup: %t: %v", found, err)
	}
	return env, fn
}

func TestCallbackCallerConcurrentArguments(t *testing.T) {
	env, fn := callbackTestFunction(t)
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Go(func() {
			value, err := callFunc(t.Context(), fn, IntValue{Val: i})
			if err != nil || value.(IntValue).Val != i+1 {
				t.Errorf("call %d: %v: %v", i, value, err)
			}
		})
	}
	wg.Wait()
	if env.Has("value") {
		t.Fatal("argument leaked into the closure")
	}
}

type mutableCallback struct{ previous ValueScope }

func (*mutableCallback) Type() hm.Type            { return IntType }
func (*mutableCallback) String() string           { return "test callback" }
func (*mutableCallback) ParameterNames() []string { return []string{"value"} }
func (*mutableCallback) IsAutoCallable() bool     { return false }
func (fn *mutableCallback) Call(ctx context.Context, scope ValueScope, args map[string]Value) (Value, error) {
	if scope == fn.previous || scope.Has("stored") {
		panic("mutable custom caller was shared")
	}
	fn.previous = scope
	scope.Bind("stored", args["value"], PublicVisibility)
	scope.EnterSelf(args["value"])
	return args["value"], nil
}

func TestCallbackCallerKeepsCustomCallableFresh(t *testing.T) {
	fn := &mutableCallback{}
	for i := range 3 {
		v, err := callFunc(t.Context(), fn, IntValue{Val: i})
		if err != nil || v.(IntValue).Val != i {
			t.Fatalf("custom callable: %v: %v", v, err)
		}
	}
}

func TestCallbackCallerPreservesCapturedSelf(t *testing.T) {
	dir := t.TempDir()
	source := `type Counter {
  value: Int! = 0
  sum: Counter! {
    [1,2,3].each { item => self.value = self.value + item }
    self
  }
}
pub result: Int! = Counter().sum.value`
	if err := os.WriteFile(filepath.Join(dir, "main.dang"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := RunDir(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	value, found, err := env.Lookup(t.Context(), "result")
	if err != nil || !found || value.(IntValue).Val != 6 {
		t.Fatalf("captured self: %v: %v", value, err)
	}
}

func BenchmarkCallbackCaller(b *testing.B) {
	_, fn := callbackTestFunction(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := callFunc(context.Background(), fn, IntValue{Val: 1}); err != nil {
			b.Fatal(err)
		}
	}
}
