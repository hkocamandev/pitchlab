package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// repoRoot resolves the checked-in output relative to this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// cmd/tsgen -> repository root
	return filepath.Join(wd, "..", "..")
}

func TestGeneratedTypesAreUpToDate(t *testing.T) {
	path := filepath.Join(repoRoot(t), DefaultOutput)

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// This is the whole point of generating rather than hand-writing. A field
	// renamed in a response struct compiles cleanly in Go and in TypeScript
	// and fails in a browser; here it fails in CI instead.
	if got := Generate(entries()); got != string(current) {
		t.Fatalf("%s is out of date -- run: make ts-types", DefaultOutput)
	}
}

func TestEveryRegisteredTypeIsAStruct(t *testing.T) {
	for _, e := range entries() {
		if k := reflect.TypeOf(e.value).Kind(); k != reflect.Struct {
			t.Errorf("%s is a %s; only structs produce an interface", e.name, k)
		}
	}
}

func TestRegisteredNamesAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, e := range entries() {
		name := tsName(e.name)
		// Two Go types can share a base name while describing different
		// shapes -- the REST pitch context and the event pitch context do.
		// Letting them collide would silently merge two contracts into one
		// declaration and the second would win.
		if prev, ok := seen[name]; ok {
			t.Errorf("%s and %s both generate %q", prev, e.name, name)
		}
		seen[name] = e.name
	}
}

func TestWireTypesAreMappedNotFollowed(t *testing.T) {
	names := map[reflect.Type]string{}

	// Both marshal to a string. A generator that followed their struct shapes
	// would describe a [16]byte array and a wall-clock struct -- neither of
	// which any client has ever received.
	if got := tsType(reflect.TypeOf(uuid.UUID{}), names); got != "string" {
		t.Errorf("uuid.UUID -> %s, want string", got)
	}
	if got := tsType(reflect.TypeOf(time.Time{}), names); got != "string" {
		t.Errorf("time.Time -> %s, want string", got)
	}
}

func TestPointersBecomeNullable(t *testing.T) {
	names := map[reflect.Type]string{}
	var f *float64

	// A pointer field is null when unset, and the dashboard has to draw "no
	// reading" differently from zero. Dropping the null would let a missing
	// spin rate render as 0 rpm.
	if got := tsType(reflect.TypeOf(f), names); got != "number | null" {
		t.Errorf("*float64 -> %s, want number | null", got)
	}
}

func TestEmbeddedStructsAreFlattened(t *testing.T) {
	type inner struct {
		A string `json:"a"`
	}
	type outer struct {
		inner
		B int `json:"b"`
	}

	fields := fieldsOf(reflect.TypeOf(outer{}), map[reflect.Type]string{})
	if len(fields) != 2 || fields[0].name != "a" || fields[1].name != "b" {
		// encoding/json inlines an embedded struct's fields. A generated type
		// that nested them would describe a response that never existed.
		t.Fatalf("embedded fields were not flattened: %+v", fields)
	}
}

func TestUnregisteredStructsAreNotReferenced(t *testing.T) {
	type unregistered struct {
		A string `json:"a"`
	}
	type holder struct {
		Thing unregistered `json:"thing"`
	}

	out := Generate([]entry{{"HolderDTO", holder{}}})

	// A reference to a type the output does not declare would not compile.
	// "unknown" is worse to use and better to ship: it makes someone look.
	if !strings.Contains(out, "thing: unknown;") {
		t.Fatalf("an unregistered struct was referenced by name:\n%s", out)
	}
}

func TestMapsAndSlicesRender(t *testing.T) {
	names := map[reflect.Type]string{}

	if got := tsType(reflect.TypeOf(map[string]int64{}), names); got != "Record<string, number>" {
		t.Errorf("map -> %s", got)
	}
	if got := tsType(reflect.TypeOf([]string{}), names); got != "string[]" {
		t.Errorf("slice -> %s", got)
	}
	// []byte is base64 on the wire, not an array of numbers.
	if got := tsType(reflect.TypeOf([]byte{}), names); got != "string" {
		t.Errorf("[]byte -> %s, want string", got)
	}
}
