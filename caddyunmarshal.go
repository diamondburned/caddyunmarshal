package caddyunmarshal

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

// Unmarshal unmarshals the given Caddyfile dispenser into the given struct
// value.
func Unmarshal[T any](d *caddyfile.Dispenser, v *T) error {
	r, err := newReflectValue(v)
	if err != nil {
		return err
	}
	return unmarshal(dispenser{d, nil}, r)
}

// UnmarshalForHTTP unmarshals the given HTTP Caddyfile helper into the given
// struct value.
func UnmarshalForHTTP[T any](d *httpcaddyfile.Helper, v *T) error {
	r, err := newReflectValue(v)
	if err != nil {
		return err
	}
	return unmarshal(dispenser{d.Dispenser, d}, r)
}

type dispenser struct {
	*caddyfile.Dispenser
	http *httpcaddyfile.Helper
}

// TODO: UnmarshalForJSON

type reflectValue struct {
	v reflect.Value
	t reflect.Type
}

func newReflectValue(v any) (reflectValue, error) {
	rv := reflect.ValueOf(v)
	rv = reflect.Indirect(rv)
	if rv.Kind() != reflect.Struct {
		return reflectValue{}, fmt.Errorf("caddyunmarshal: expected struct value, got %T", v)
	}

	rt := rv.Type()
	return reflectValue{rv, rt}, nil
}

// unmarshal unmarshals a list of arguments or blocks. Note that for a typical
// block (e.g. "handle a b c"), the function assumes that the first argument,
// which is the directive name, has already been consumed.
func unmarshal(d dispenser, r reflectValue) error {
	info, err := extractFields(r)
	if err != nil {
		return fmt.Errorf("cannot extract fields: %w", err)
	}

	// If we expect a matcher, then the user MUST have called UnmarshalForHTTP,
	// because we need the httpcaddyfile.Helper instance.
	if info.matcher != nil {
		if d.http == nil {
			return fmt.Errorf("cannot unmarshal matcher: UnmarshalForHTTP was not called")
		}

		// Matchers must be of type caddy.ModuleMap.
		if !r.t.AssignableTo(TypeCaddyModuleMap) {
			return fmt.Errorf("cannot unmarshal matcher: expected caddy.ModuleMap, got %T", r.v.Interface())
		}

		moduleMap, ok, err := d.http.MatcherToken()
		if err != nil {
			return fmt.Errorf("cannot get module map: %w", err)
		}

		if ok {
			// We matched a matcher, so we can set the value.
			r.v.Set(reflect.ValueOf(moduleMap))
		}
	}

	var hadBlock bool

	var i int
loop:
	for {
		nesting := d.Nesting()
		switch {
		case d.NextArg():
			field, ok := info.otherFieldAt(i)
			if !ok {
				return d.WrapErr(fmt.Errorf("unexpected argument at [%d]: %s", i, d.Val()))
			}

			if err := unmarshalValue(d, field.value, d.Val()); err != nil {
				return fmt.Errorf("error at [%d]: %w", i, err)
			}

		case d.NextBlock(nesting):
			var value reflectValue
			if field, ok := info.otherFieldAt(i); ok {
				value = field.value
			} else {
				// Field not found, so check if we parsed a block already.
				// If not, then we can assume that we want this. Otherwise,
				// error out.
				if hadBlock {
					return d.WrapErr(fmt.Errorf("unexpected block at [%d]: %s", i, d.Val()))
				}
				// value is kept the same, meaning we'll unmarshal into the
				// current struct.
				value = r
				hadBlock = true
			}

			if err := unmarshalBlock(d, nesting, value); err != nil {
				return fmt.Errorf("error at [%d]: %w", i, err)
			}

		default:
			break loop
		}

		i++
	}

	// check if we still have fields to unmarshal
	if i < len(info.otherFields) {
		for j, field := range info.otherFields[i:] {
			if !field.optional() {
				return d.WrapErr(fmt.Errorf("missing required field [%d]", i+j))
			}
		}
	}

	return nil
}

func unmarshalBlock(d dispenser, nesting int, r reflectValue) error {
	// We expect either a struct or a map[K]V for each struct field value.
	// If it's anything else, then it doesn't match a block.
	var isMap bool
	var info structInfo
	switch r.v.Kind() {
	case reflect.Struct:
		i, err := extractFields(r)
		if err != nil {
			return fmt.Errorf("cannot extract fields: %w", err)
		}
		info = i
	case reflect.Map:
		isMap = true
	default:
		return fmt.Errorf("expected struct or map, got %T", r.v.Interface())
	}

	parse := func() error {
		name := d.Val()
		var value reflectValue

		if isMap {
			// If it's a map, then we need to create a new value for the
			// map key, and then unmarshal into that.
			key := reflect.New(r.t.Key()).Elem()
			if err := unmarshalValue(d, reflectValue{key, key.Type()}, name); err != nil {
				return fmt.Errorf("error unmarshaling map key %q: %w", name, err)
			}

			// Create a new value for the map value.
			val := reflect.New(r.t.Elem()).Elem()
			value = reflectValue{val, val.Type()}

			// At the end, set the map value.
			defer func() { r.v.SetMapIndex(key, val) }()
		} else {
			field, ok := info.blockFieldNamed(name)
			if !ok {
				// Fields are optional, so we can just skip over them.
				// I think this is the right skip? TODO: check.
				d.NextSegment()
				return nil
			}
			value = field.value
		}

		// If this field is a boolean, then we are immediately done and don't
		// expect any more fields.
		if value.v.Kind() == reflect.Bool {
			if d.CountRemainingArgs() > 0 {
				return d.WrapErr(fmt.Errorf("unexpected argument at %q: %s", name, d.Val()))
			}

			value.v.SetBool(true)
			return nil
		}

		// Otherwise, delegate this list of values to the unmarshal function.
		if err := unmarshal(d, value); err != nil {
			return fmt.Errorf("error at %q: %w", name, err)
		}

		return nil
	}

	// Note that if we're in this function, then we've already entered the
	// child. We shall iterate over the fields within it.
	for ok := true; ok; ok = d.NextBlock(nesting) {
		if err := parse(); err != nil {
			return nil
		}
	}

	return nil
}

// Explicitly supported value types:
var (
	TypeCaddyModuleMap      = reflect.TypeOf(caddy.ModuleMap{}) // matcher only
	TypeCaddyAddress        = reflect.TypeOf(httpcaddyfile.Address{})
	TypeCaddyNetworkAddress = reflect.TypeOf(caddy.NetworkAddress{})
	TypeCaddyDuration       = reflect.TypeOf(caddy.Duration(0))
	TypeDuration            = reflect.TypeOf(time.Duration(0))
)

func unmarshalValue(d dispenser, r reflectValue, raw string) error {
	// Does this type implement caddyfile.Unmarshaler? If so, we can allow some
	// overriding.
	if unmarshaler, ok := r.v.Addr().Interface().(caddyfile.Unmarshaler); ok {
		return unmarshaler.UnmarshalCaddyfile(d.Dispenser)
	}

	// Handle primitive types.
	switch r.v.Kind() {
	case reflect.String:
		r.v.SetString(raw)
		return nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(raw, 10, r.t.Bits())
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse int: %w", err))
		}

		r.v.SetInt(i)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(raw, 10, r.t.Bits())
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse uint: %w", err))
		}

		r.v.SetUint(u)
		return nil

	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(raw, r.t.Bits())
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse float: %w", err))
		}

		r.v.SetFloat(f)
		return nil

	case reflect.Bool:
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse boolean value %q: %w", raw, err))
		}
		r.v.SetBool(v)
		return nil
	}

	switch {
	case r.t.AssignableTo(TypeCaddyAddress):
		addr, err := httpcaddyfile.ParseAddress(raw)
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse address: %w", err))
		}

		r.v.Set(reflect.ValueOf(addr))
		return nil

	case r.t.AssignableTo(TypeCaddyNetworkAddress):
		addr, err := caddy.ParseNetworkAddress(raw)
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse network address: %w", err))
		}

		r.v.Set(reflect.ValueOf(addr))
		return nil

	case r.t.AssignableTo(TypeCaddyDuration):
		dura, err := caddy.ParseDuration(raw)
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse duration: %w", err))
		}

		r.v.Set(reflect.ValueOf(dura))
		return nil

	case r.t.AssignableTo(TypeDuration):
		dura, err := time.ParseDuration(raw)
		if err != nil {
			return d.WrapErr(fmt.Errorf("cannot parse duration: %w", err))
		}

		r.v.Set(reflect.ValueOf(dura))
		return nil
	}

	return fmt.Errorf("cannot unmarshal value of unsupported type %T", r.v.Interface())
}

type fieldKind interface {
	fieldKind()
}

// blockFieldKind is a fieldKind that indicates that the field is a field within
// a block.
type blockFieldKind struct {
	name string // name of the field within our block
}

// blockKind is a fieldKind that indicates that the field is an entire block.
type blockKind struct {
	ix       int
	optional bool
}

// argumentKind is a fieldKind that indicates that the field is a value
// argument.
type argumentKind struct {
	ix       int
	optional bool
}

// matcherKind is a fieldKind that indicates that the field is a matcher. It is
// always assumed that this argument is optional, but if it does appear, it must
// be the first argument. Because of this special case, it is its own field in
// structInfo.
type matcherKind struct{}

func (blockFieldKind) fieldKind() {}
func (blockKind) fieldKind()      {}
func (argumentKind) fieldKind()   {}
func (matcherKind) fieldKind()    {}

type fieldInfo struct {
	field reflect.StructField
	value reflectValue
	kind  fieldKind
}

func (field fieldInfo) optional() bool {
	switch kind := field.kind.(type) {
	case blockKind:
		return kind.optional
	case argumentKind:
		return kind.optional
	default:
		return true // block fields are always optional
	}
}

func (field fieldInfo) index() int {
	switch kind := field.kind.(type) {
	case blockKind:
		return kind.ix
	case argumentKind:
		return kind.ix
	default:
		return -1
	}
}

type structInfo struct {
	blockFields []fieldInfo // for blockFieldKinds
	otherFields []fieldInfo // for blockKinds and argumentKinds
	matcher     *fieldInfo
}

func (s structInfo) blockFieldNamed(name string) (fieldInfo, bool) {
	for _, field := range s.blockFields {
		if field.field.Name == name {
			return field, true
		}
	}
	return fieldInfo{}, false
}

func (s structInfo) otherFieldAt(ix int) (fieldInfo, bool) {
	if ix < 0 || ix >= len(s.otherFields) {
		return fieldInfo{}, false
	}
	return s.otherFields[ix], true
}

var blockIxRe = regexp.MustCompile(`^\{(\d+)\}$`)

// extractFields extracts all struct fields from the given struct value.
func extractFields(r reflectValue) (structInfo, error) {
	var info structInfo

	nfields := r.v.NumField()
	for i := 0; i < nfields; i++ {
		f := r.t.Field(i)
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("caddyfile")
		if tag == "" {
			// no tag, so default kind
			info.blockFields = append(info.blockFields, fieldInfo{
				f, reflectValue{r.v.Field(i), f.Type},
				blockFieldKind{f.Name},
			})
			continue
		}

		parts := strings.Split(tag, ",")
		name := parts[0]

		switch {
		case name == "-":
			// ignore this field
			continue

		case name == "$matcher":
			// matcher field
			info.matcher = &fieldInfo{
				f, reflectValue{r.v.Field(i), f.Type},
				matcherKind{},
			}
		case blockIxRe.MatchString(name):
			matches := blockIxRe.FindStringSubmatch(name)
			ix, err := strconv.Atoi(matches[1])
			if err != nil {
				return structInfo{}, fmt.Errorf(
					"caddyunmarshal: invalid block index %s: %w", name, err)
			}

			info.otherFields = append(info.blockFields, fieldInfo{
				f, reflectValue{r.v.Field(i), f.Type},
				blockKind{ix, hasOpt(parts[1:], "optional")},
			})
		case strings.HasPrefix(name, "$"):
			ix, err := strconv.Atoi(strings.TrimPrefix(name, "$"))
			if err != nil {
				return structInfo{}, fmt.Errorf(
					"caddyunmarshal: invalid argument index %s: %w", name, err)
			}

			info.otherFields = append(info.otherFields, fieldInfo{
				f, reflectValue{r.v.Field(i), f.Type},
				argumentKind{ix, hasOpt(parts[1:], "optional")},
			})
		default:
			info.blockFields = append(info.blockFields, fieldInfo{
				f, reflectValue{r.v.Field(i), f.Type},
				blockFieldKind{name},
			})
		}
	}

	sort.Slice(info.blockFields, func(i, j int) bool {
		return info.blockFields[i].index() < info.blockFields[j].index()
	})

	// validate that optional fields are at the end
	var foundOptional bool
	for i, field := range info.otherFields {
		optional := field.optional()

		if foundOptional && !optional {
			return structInfo{}, fmt.Errorf(
				"caddyunmarshal: illegal non-optional field %d follows optional field", i)
		}

		if !foundOptional {
			foundOptional = optional
		}
	}

	// validate that all field indices are unique
	usedIndices := make(map[int]struct{})
	for _, field := range info.otherFields {
		ix := field.index()

		_, used := usedIndices[ix]
		if used {
			return structInfo{}, fmt.Errorf(
				"caddyunmarshal: duplicate field index %d", ix)
		}

		usedIndices[ix] = struct{}{}
	}

	return info, nil
}

func hasOpt(parts []string, opt string) bool {
	for _, part := range parts {
		if part == opt {
			return true
		}
	}
	return false
}
