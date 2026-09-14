package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The TypeScript types the dashboard uses are generated from the Go response
// structs rather than written by hand or derived from an OpenAPI document.
//
// A hand-written copy drifts: a field renamed in Go compiles fine on both
// sides and fails at runtime in a browser. An OpenAPI document is a second
// hand-maintained artifact with the same problem one level removed -- it can
// disagree with the code it claims to describe.
//
// Generating from the structs themselves makes drift impossible: the Go types
// are the only source, and a test fails the build when the checked-in output
// no longer matches them.

// entry names one exported type.
type entry struct {
	name  string
	value any
}

// tsType maps a Go type onto its JSON representation in TypeScript.
//
// It maps what the encoder actually produces, not what the Go type is named.
// uuid.UUID and time.Time are both strings on the wire; a generator that
// followed their struct shapes would describe something no client ever sees.
func tsType(t reflect.Type, names map[reflect.Type]string) string {
	switch t {
	case reflect.TypeOf(json.RawMessage{}):
		// Raw JSON is embedded verbatim, not base64-encoded like an ordinary
		// []byte. Typing it as a string would describe something the client
		// never receives.
		return "unknown"

	case reflect.TypeOf(uuid.UUID{}):
		return "string"
	case reflect.TypeOf(time.Time{}):
		// ISO 8601. Typed as a string because that is what arrives; parsing it
		// is the caller's decision, not something to hide behind a type alias.
		return "string"
	}

	switch t.Kind() {
	case reflect.Pointer:
		// A pointer field is null in JSON when unset, and null is not the same
		// as absent: the dashboard has to render "no reading" differently from
		// zero, which is the whole reason these fields are pointers in Go.
		return tsType(t.Elem(), names) + " | null"

	case reflect.String:
		return "string"

	case reflect.Bool:
		return "boolean"

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"

	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			// []byte is base64 on the wire.
			return "string"
		}
		return tsType(t.Elem(), names) + "[]"

	case reflect.Map:
		return fmt.Sprintf("Record<%s, %s>",
			tsType(t.Key(), names), tsType(t.Elem(), names))

	case reflect.Struct:
		// Only types the registry names are referenced. Anything else would
		// produce a dangling reference in the output, which is worse than an
		// honest "unknown" the compiler will make someone look at.
		if name, ok := names[t]; ok {
			return name
		}
		return "unknown"

	case reflect.Interface:
		return "unknown"
	}
	return "unknown"
}

// tsName strips the DTO suffix. The suffix distinguishes response shapes from
// database rows inside the Go package; in TypeScript there is nothing to
// distinguish them from.
func tsName(goName string) string {
	return strings.TrimSuffix(goName, "DTO")
}

// field describes one property.
type field struct {
	name     string
	tsType   string
	optional bool
	comment  string
}

// fieldsOf flattens a struct into its JSON properties.
//
// Embedded structs are flattened because that is what encoding/json does with
// them: AthleteListItem has AthleteDTO's fields inline on the wire, and a
// generated type that nested them would describe a response that never
// existed.
func fieldsOf(t reflect.Type, names map[reflect.Type]string) []field {
	var out []field

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// An unexported field is invisible to the encoder -- except when it is
		// an embedded struct, whose exported fields encoding/json still
		// inlines. Skipping those would drop real properties from the output.
		if f.PkgPath != "" && !f.Anonymous {
			continue
		}

		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}

		parts := strings.Split(tag, ",")
		name := parts[0]
		omitempty := false
		for _, p := range parts[1:] {
			if p == "omitempty" {
				omitempty = true
			}
		}

		if f.Anonymous && name == "" {
			inner := f.Type
			for inner.Kind() == reflect.Pointer {
				inner = inner.Elem()
			}
			if inner.Kind() == reflect.Struct {
				out = append(out, fieldsOf(inner, names)...)
				continue
			}
		}

		if name == "" {
			name = f.Name
		}

		out = append(out, field{
			name:     name,
			tsType:   tsType(f.Type, names),
			optional: omitempty,
		})
	}

	return out
}

// Generate renders the TypeScript module.
func Generate(entries []entry) string {
	// The registry decides what each type is called in TypeScript. Two Go
	// types can share a base name -- the REST pitch context and the event
	// pitch context do -- and they are different shapes, so the name has to
	// come from the registry rather than from reflection.
	names := map[reflect.Type]string{}
	for _, e := range entries {
		names[reflect.TypeOf(e.value)] = tsName(e.name)
	}

	var b strings.Builder
	b.WriteString(`// Code generated from the Go response types. DO NOT EDIT.
//
// Regenerate with: make ts-types
//
// These mirror the structs in internal/api/dto.go. They are generated rather
// than written by hand so that a field renamed on the server cannot compile
// cleanly on both sides and fail in a browser.

`)

	for _, e := range entries {
		t := reflect.TypeOf(e.value)
		fields := fieldsOf(t, names)

		fmt.Fprintf(&b, "export interface %s {\n", tsName(e.name))
		for _, f := range fields {
			opt := ""
			if f.optional {
				opt = "?"
			}
			if f.comment != "" {
				fmt.Fprintf(&b, "  /** %s */\n", f.comment)
			}
			fmt.Fprintf(&b, "  %s%s: %s;\n", f.name, opt, f.tsType)
		}
		b.WriteString("}\n\n")
	}

	return b.String()
}

// sortedNames is used by the staleness test's failure message.
func sortedNames(entries []entry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, tsName(e.name))
	}
	sort.Strings(names)
	return names
}
