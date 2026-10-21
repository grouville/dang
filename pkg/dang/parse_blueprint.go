package dang

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// A parse blueprint retains syntax before inference, never a module's schema,
// client or values. Each use constructs independent pointers and containers
// and supplies the current file path for source locations.
type parseBlueprint func(filename reflect.Value) reflect.Value

type syntaxCloner func(*syntaxCloneState) reflect.Value

type syntaxCloneState struct {
	filename reflect.Value
	pointers []reflect.Value
}

type syntaxPointer struct {
	typ     reflect.Type
	address uintptr
}

type syntaxBlueprintBuilder struct {
	weight    *int64
	budget    int64
	pointers  map[syntaxPointer]int
	sliceCaps map[syntaxPointer]int
	regions   []syntaxRegion
}

type syntaxRegion struct{ start, end uintptr }

type parseBlueprintKey struct {
	digest [32]byte
}

type blueprintEntry struct {
	create parseBlueprint
	weight int64
}

// SyntaxCache stores syntax before inference. Clients and evaluated values
// belong to each call. Construct a cache with NewSyntaxCache.
type SyntaxCache struct {
	mu      sync.Mutex
	entries *simplelru.LRU[parseBlueprintKey, blueprintEntry]
	weight  int64
	budget  int64
}

func newSyntaxCache(entries int, budget int64) *SyntaxCache {
	cache := &SyntaxCache{budget: budget}
	cache.entries, _ = simplelru.NewLRU[parseBlueprintKey, blueprintEntry](entries,
		func(_ parseBlueprintKey, entry blueprintEntry) { cache.weight -= entry.weight })
	return cache
}

func (cache *SyntaxCache) get(key parseBlueprintKey) (parseBlueprint, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries.Get(key)
	return entry.create, ok
}

func (cache *SyntaxCache) add(key parseBlueprintKey, create parseBlueprint, weight int64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	// Concurrent misses may build the same entry; the first one is enough.
	if cache.entries.Contains(key) || weight > cache.budget {
		return
	}
	cache.weight += weight
	cache.entries.Add(key, blueprintEntry{create: create, weight: weight})
	for cache.weight > cache.budget {
		cache.entries.RemoveOldest()
	}
}

// NewSyntaxCache creates a concurrent cache for syntax reused across RunDir calls.
// It retains at most 128 entries with a combined estimated weight of 64 MiB.
func NewSyntaxCache() *SyntaxCache { return newSyntaxCache(128, 64<<20) }

type syntaxCacheKey struct{}

// WithSyntaxCache lets RunDir reuse parsed source through cache. Each use gets
// fresh syntax nodes before type checking, imports and initialization. Share
// the cache across calls that repeatedly evaluate the same module sources.
func WithSyntaxCache(ctx context.Context, cache *SyntaxCache) context.Context {
	return context.WithValue(ctx, syntaxCacheKey{}, cache)
}

func parseFileWithSyntaxCache(ctx context.Context, filename string) (any, error) {
	cache, _ := ctx.Value(syntaxCacheKey{}).(*SyntaxCache)
	if cache == nil {
		return ParseFileWithRecovery(filename, GlobalStore("filePath", filename))
	}
	return parseFileBlueprintWithCache(filename, cache)
}

func parseFileBlueprintWithCache(filename string, cache *SyntaxCache) (any, error) {
	source, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	key := parseBlueprintKey{digest: sha256.Sum256(source)}
	if blueprint, ok := cache.get(key); ok {
		return blueprint(reflect.ValueOf(filename)).Interface().(*FileBlock), nil
	}
	parsed, err := ParseWithRecovery(filename, source, GlobalStore("filePath", filename))
	if err != nil {
		return nil, err
	}
	block, ok := parsed.(*FileBlock)
	if !ok {
		return nil, fmt.Errorf("parsed result for %s is not a FileBlock", filename)
	}
	weight := int64(128) // key and LRU entry
	blueprint, err := syntaxBlueprintWeighted(reflect.ValueOf(block), &weight, cache.budget)
	if err != nil {
		// New syntax fields must preserve parsing even before the blueprint
		// builder supports them. This result has not been inferred yet.
		return block, nil
	}
	cache.add(key, blueprint, weight)
	return block, nil
}

func syntaxBlueprint(value reflect.Value) (parseBlueprint, error) {
	weight := int64(0)
	return syntaxBlueprintWeighted(value, &weight, 64<<20)
}

func syntaxBlueprintWeighted(value reflect.Value, weight *int64, budget int64) (parseBlueprint, error) {
	builder := syntaxBlueprintBuilder{weight: weight, budget: budget, pointers: map[syntaxPointer]int{}, sliceCaps: map[syntaxPointer]int{}}
	create, err := builder.build(value)
	if err != nil {
		return nil, err
	}
	// Identical references share one clone. Other overlapping storage,
	// including pointers into another object, requires normal parsing.
	slices.SortFunc(builder.regions, func(a, b syntaxRegion) int { return cmp.Compare(a.start, b.start) })
	for i := 1; i < len(builder.regions); i++ {
		if builder.regions[i].start < builder.regions[i-1].end {
			return nil, fmt.Errorf("overlapping syntax storage")
		}
	}
	count := len(builder.pointers)
	return func(filename reflect.Value) reflect.Value {
		state := syntaxCloneState{filename: filename, pointers: make([]reflect.Value, count)}
		return create(&state)
	}, nil
}

func (builder *syntaxBlueprintBuilder) build(value reflect.Value) (syntaxCloner, error) {
	// Account for a closure, reflected scalar/type metadata and child storage.
	// This is a cache weight, not exact allocator heap accounting.
	*builder.weight += 128
	if *builder.weight > builder.budget {
		return nil, fmt.Errorf("parse blueprint exceeds cache budget")
	}
	typeOf := value.Type()
	if value.IsZero() {
		return func(state *syntaxCloneState) reflect.Value { return reflect.Zero(typeOf) }, nil
	}
	switch value.Kind() {
	case reflect.Pointer:
		key := syntaxPointer{typ: typeOf, address: value.Pointer()}
		if id, ok := builder.pointers[key]; ok {
			return func(state *syntaxCloneState) reflect.Value { return state.pointers[id] }, nil
		}
		id := len(builder.pointers)
		builder.pointers[key] = id
		if size := typeOf.Elem().Size(); size > 0 {
			builder.regions = append(builder.regions, syntaxRegion{key.address, key.address + size})
		}
		element, err := builder.build(value.Elem())
		if err != nil {
			return nil, err
		}
		return func(state *syntaxCloneState) reflect.Value {
			result := reflect.New(typeOf.Elem())
			state.pointers[id] = result
			result.Elem().Set(element(state))
			return result
		}, nil
	case reflect.Interface:
		element, err := builder.build(value.Elem())
		if err != nil {
			return nil, err
		}
		return func(state *syntaxCloneState) reflect.Value {
			result := reflect.New(typeOf).Elem()
			result.Set(element(state))
			return result
		}, nil
	case reflect.Struct:
		*builder.weight += int64(value.NumField()) * 8
		if *builder.weight > builder.budget {
			return nil, fmt.Errorf("parse blueprint exceeds cache budget")
		}
		fields := make([]syntaxCloner, value.NumField())
		for i := range fields {
			field := typeOf.Field(i)
			if field.PkgPath != "" {
				if !value.Field(i).IsZero() {
					return nil, fmt.Errorf("nonzero private parse field %s.%s", typeOf, field.Name)
				}
				continue
			}
			if typeOf == reflect.TypeFor[SourceLocation]() && field.Name == "Filename" {
				// Source paths belong to this use of the syntax, not its content.
				fields[i] = func(state *syntaxCloneState) reflect.Value { return state.filename }
				continue
			}
			var err error
			fields[i], err = builder.build(value.Field(i))
			if err != nil {
				return nil, err
			}
		}
		return func(state *syntaxCloneState) reflect.Value {
			result := reflect.New(typeOf).Elem()
			for i, field := range fields {
				if field != nil {
					result.Field(i).Set(field(state))
				}
			}
			return result
		}, nil
	case reflect.Slice:
		length, capacity := value.Len(), value.Cap()
		id := -1
		if capacity > 0 && typeOf.Elem().Size() > 0 {
			key := syntaxPointer{typ: typeOf, address: value.Pointer()}
			if found, ok := builder.pointers[key]; ok {
				if builder.sliceCaps[key] != capacity {
					return nil, fmt.Errorf("overlapping syntax slice capacities")
				}
				return func(state *syntaxCloneState) reflect.Value {
					return state.pointers[found].Slice3(0, length, capacity)
				}, nil
			}
			id = len(builder.pointers)
			builder.pointers[key] = id
			builder.sliceCaps[key] = capacity
			builder.regions = append(builder.regions, syntaxRegion{key.address, key.address + uintptr(capacity)*typeOf.Elem().Size()})
		}
		*builder.weight += int64(capacity) * 8
		if *builder.weight > builder.budget {
			return nil, fmt.Errorf("parse blueprint exceeds cache budget")
		}
		elements := make([]syntaxCloner, capacity)
		full := value.Slice(0, capacity)
		for i := range elements {
			var err error
			elements[i], err = builder.build(full.Index(i))
			if err != nil {
				return nil, err
			}
		}
		return func(state *syntaxCloneState) reflect.Value {
			result := reflect.MakeSlice(typeOf, capacity, capacity)
			if id >= 0 {
				state.pointers[id] = result
			}
			for i, element := range elements {
				result.Index(i).Set(element(state))
			}
			return result.Slice3(0, length, capacity)
		}, nil
	case reflect.Func:
		// Parser-installed operator evaluators are immutable functions; the
		// blueprint is built before inference or evaluation can bind clients.
		if typeOf != reflect.TypeFor[BinaryOperatorEvaluator]() {
			return nil, fmt.Errorf("unsupported parse function %s", typeOf)
		}
		function := reflect.New(typeOf).Elem()
		function.Set(value)
		return func(state *syntaxCloneState) reflect.Value { return function }, nil
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		// Copy scalar values into the blueprint, detached from parser storage.
		scalar := reflect.New(typeOf).Elem()
		scalar.Set(value)
		if value.Kind() == reflect.String {
			*builder.weight += int64(value.Len())
			if *builder.weight > builder.budget {
				return nil, fmt.Errorf("parse blueprint exceeds cache budget")
			}
			// Detach strings from any larger parser source buffer.
			scalar.SetString(strings.Clone(value.String()))
		}
		return func(state *syntaxCloneState) reflect.Value { return scalar }, nil
	default:
		return nil, fmt.Errorf("unsupported parse field %s", typeOf)
	}
}
