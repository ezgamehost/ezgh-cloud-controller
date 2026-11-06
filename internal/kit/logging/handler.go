package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// handler writes contract JSON lines. It lays lines out itself rather than wrapping
// slog.JSONHandler, so context fields stay at the top level inside groups and redaction sees
// every value.
type handler struct {
	out     *output
	level   slog.Leveler
	service string
	now     func() time.Time // tests; nil is the record's time
	pre     []groupAttr      // WithAttrs, with the groups open when they were added
	groups  []string
}

type output struct {
	mu sync.Mutex
	w  io.Writer
}

type groupAttr struct {
	groups []string
	attr   slog.Attr
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.pre = slices.Clip(h.pre)
	for _, a := range as {
		c.pre = append(c.pre, groupAttr{h.groups, a})
	}
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(slices.Clip(h.groups), name)
	return &c
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var attrs []field
	for _, p := range h.pre {
		attrs = putAttr(attrs, p.groups, p.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs = putAttr(attrs, h.groups, a)
		return true
	})
	t := r.Time
	if h.now != nil {
		t = h.now()
	}
	var buf bytes.Buffer
	encodeLine(&buf, buildLine(t, levelName(r.Level), r.Message, h.service, contextOf(ctx), attrs))
	h.out.mu.Lock()
	defer h.out.mu.Unlock()
	_, err := h.out.w.Write(buf.Bytes())
	return err
}

// putAttr adds a under groups: a top-level field, or a key inside its group's object (made on
// first use; slog omits empty groups and empty keys, and inlines groups with an empty key).
func putAttr(attrs []field, groups []string, a slog.Attr) []field {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup && a.Key == "" {
		for _, g := range a.Value.Group() {
			attrs = putAttr(attrs, groups, g)
		}
		return attrs
	}
	if a.Key == "" || (a.Value.Kind() == slog.KindGroup && len(a.Value.Group()) == 0) {
		return attrs
	}
	if len(groups) == 0 {
		return append(attrs, field{a.Key, jsonValue(a.Value)})
	}
	var m map[string]any
	for i := len(attrs) - 1; i >= 0; i-- {
		if attrs[i].key == groups[0] {
			m, _ = attrs[i].value.(map[string]any)
			break
		}
	}
	if m == nil {
		m = map[string]any{}
		attrs = append(attrs, field{groups[0], m})
	}
	for _, g := range groups[1:] {
		next, ok := m[g].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[g] = next
		}
		m = next
	}
	putInto(m, a)
	return attrs
}

func putInto(m map[string]any, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup && a.Key == "" {
		for _, g := range a.Value.Group() {
			putInto(m, g)
		}
		return
	}
	if a.Key != "" {
		m[a.Key] = jsonValue(a.Value)
	}
}

// jsonValue is a slog value as encoding/json writes it: durations in milliseconds, times in the
// line's format, groups as objects.
func jsonValue(v slog.Value) any {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return float64(v.Duration()) / float64(time.Millisecond)
	case slog.KindTime:
		return v.Time().UTC().Format(timeLayout)
	case slog.KindGroup:
		m := map[string]any{}
		for _, a := range v.Group() {
			putInto(m, a)
		}
		return m
	}
	return anyValue(v.Any())
}

// anyValue: an error is its message (which includes its wrapped chain); anything else is what
// encoding/json makes of it, read back as maps and slices so redaction can walk it. When
// encoding/json can't write it (a func or channel field, a cycle), it is walked with reflect into
// the same shape instead, never printed with fmt, which would write a secret field unredacted.
func anyValue(x any) any {
	switch t := x.(type) {
	case nil:
		return nil
	case error:
		return t.Error()
	}
	b, err := json.Marshal(x)
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var out any
		if dec.Decode(&out) == nil {
			return out
		}
	}
	return walk(reflect.ValueOf(x), map[uintptr]bool{}, 0)
}

// maxDepth bounds walk; deeper values are written as "[too deep]".
const maxDepth = 32

var (
	errorType     = reflect.TypeFor[error]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	// Requests and responses carry credentials in fields redaction can't recognise by name (the
	// URL's query, a header's raw value in Host or RequestURI), so they are never walked.
	opaque = map[reflect.Type]bool{
		reflect.TypeFor[http.Request]():  true,
		reflect.TypeFor[http.Response](): true,
	}
)

// walk is v as the maps, slices and scalars encoding/json decodes into: exported struct fields
// under their JSON names, funcs, channels and unsafe pointers left out of structs (and written as
// a placeholder elsewhere), and a pointer already being walked written as "[cycle]". Only
// scalars encoding/json refuses (NaN, ±Inf, complex numbers) use their fmt form.
func walk(v reflect.Value, onPath map[uintptr]bool, depth int) any {
	if !v.IsValid() {
		return nil
	}
	if depth > maxDepth {
		return "[too deep]"
	}
	t := v.Type()
	if opaque[t] || (t.Kind() == reflect.Pointer && opaque[t.Elem()]) {
		return "[unencodable " + t.String() + "]"
	}
	if v.Kind() != reflect.Pointer && v.Kind() != reflect.Interface || !v.IsNil() {
		if v.CanInterface() && t.Implements(errorType) {
			return v.Interface().(error).Error()
		}
		if v.CanInterface() && t.Implements(marshalerType) {
			if b, err := json.Marshal(v.Interface()); err == nil {
				var out any
				dec := json.NewDecoder(bytes.NewReader(b))
				dec.UseNumber()
				if dec.Decode(&out) == nil {
					return out
				}
			}
			return "[unencodable " + t.String() + "]"
		}
	}
	switch v.Kind() {
	case reflect.Bool:
		return v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Sprint(f)
		}
		return f
	case reflect.Complex64, reflect.Complex128:
		return fmt.Sprint(v.Complex())
	case reflect.String:
		return v.String()
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		if v.Kind() == reflect.Interface {
			return walk(v.Elem(), onPath, depth+1)
		}
		p := v.Pointer()
		if onPath[p] {
			return "[cycle]"
		}
		onPath[p] = true
		defer delete(onPath, p)
		return walk(v.Elem(), onPath, depth+1)
	case reflect.Struct:
		m := map[string]any{}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() || unwritable(f.Type) {
				continue
			}
			name := f.Name
			if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == "-" {
				continue
			} else if tag != "" {
				name = tag
			}
			m[name] = walk(v.Field(i), onPath, depth+1)
		}
		return m
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		p := v.Pointer()
		if onPath[p] {
			return "[cycle]"
		}
		onPath[p] = true
		defer delete(onPath, p)
		m := make(map[string]any, v.Len())
		for it := v.MapRange(); it.Next(); {
			k := it.Key()
			key := fmt.Sprint(k.Interface())
			if k.Kind() == reflect.String {
				key = k.String()
			}
			m[key] = walk(it.Value(), onPath, depth+1)
		}
		return m
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice {
			if v.IsNil() {
				return nil
			}
			if t.Elem().Kind() == reflect.Uint8 {
				return base64.StdEncoding.EncodeToString(v.Bytes())
			}
			p := v.Pointer()
			if onPath[p] && v.Len() > 0 {
				return "[cycle]"
			}
			onPath[p] = true
			defer delete(onPath, p)
		}
		out := make([]any, v.Len())
		for i := range out {
			out[i] = walk(v.Index(i), onPath, depth+1)
		}
		return out
	}
	return "[unencodable " + t.String() + "]" // func, chan, unsafe.Pointer
}

// unwritable types are left out of a walked struct, as encoding/json would refuse them.
func unwritable(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return true
	}
	return false
}
