package dang

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type methodHoistingScope struct {
	ValueScope
	pending int
}

func (s *methodHoistingScope) BindLazy(name string, init func(context.Context) (Value, error), visibility Visibility) {
	s.pending++
	s.ValueScope.BindLazy(name, init, visibility)
}

func TestCoercedMethodsAvoidVariableInitializers(t *testing.T) {
	directory := t.TempDir()
	source := `type Example {
  value: String!
  read(prefix: String! = "hello"): String! { prefix + ":" + value }
}`
	if err := os.WriteFile(filepath.Join(directory, "main.dang"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := RunDir(t.Context(), directory, false)
	if err != nil {
		t.Fatal(err)
	}
	constructor, _, err := env.Lookup(t.Context(), "Example")
	if err != nil {
		t.Fatal(err)
	}
	declaration := constructor.(*ConstructorFunction)
	object := NewObject(declaration.ObjectType)
	object.Bind("value", StringValue{Val: "current"}, PublicVisibility)
	scope := &methodHoistingScope{ValueScope: CreateOverlayValueScope(object, declaration.Closure)}
	scope.EnterSelf(object)
	if _, err := EvaluateFormsWithPhases(t.Context(), declaration.ObjectBodyForms, scope); err != nil {
		t.Fatal(err)
	}
	if scope.pending != 0 {
		t.Fatalf("typed methods must be installed without variable initializers; got %d", scope.pending)
	}
	value, _, err := object.Lookup(t.Context(), "read")
	if err != nil {
		t.Fatal(err)
	}
	result, err := (BoundMethod{Method: value.(FunctionValue), Receiver: object}).Call(t.Context(), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.(StringValue).Val != "hello:current" {
		t.Fatalf("method receiver or defaults changed: %v", result)
	}
}

func TestCoercedMethodPhasesPreserveForwardCallsAndEffects(t *testing.T) {
	directory := t.TempDir()
	source := `type Example {
  let value = later()
  later: String! { "ready" }
  read: String! { value }
}
let example = Example()
let result = example.read
`
	if err := os.WriteFile(filepath.Join(directory, "main.dang"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := RunDir(t.Context(), directory, false)
	if err != nil {
		t.Fatal(err)
	}
	result, found, err := env.Lookup(t.Context(), "result")
	if err != nil || !found {
		t.Fatalf("result lookup: %t: %v", found, err)
	}
	data, err := json.Marshal(result)
	if err != nil || string(data) != `"ready"` {
		t.Fatalf("forward call changed: %s: %v", data, err)
	}
}
