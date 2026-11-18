package sqlite

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Everything a record holds is kept as JSON and read back into the generic
// form of core.DecodeJSONValue: int64 for an integer, float64 for any other
// number, so a value read after a restart equals the one a run held before.

func enc(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return b, nil
}

// dec decodes b into dst (a pointer) and converts every number held by an
// `any` to int64 or float64.
func dec(b []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	fixNumbers(reflect.ValueOf(dst))
	return nil
}

func numberValue(n json.Number) any {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return i
		}
	}
	f, err := n.Float64()
	if err != nil {
		return s
	}
	return f
}

var numberType = reflect.TypeOf(json.Number(""))

func fixNumbers(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			fixNumbers(v.Elem())
		}
	case reflect.Interface:
		if v.IsNil() {
			return
		}
		e := v.Elem()
		if e.Type() == numberType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(numberValue(e.Interface().(json.Number)))) //nolint:errcheck,forcetypeassert // type checked above
			}
			return
		}
		switch e.Kind() {
		case reflect.Map, reflect.Slice:
			fixNumbers(e)
		}
	case reflect.Map:
		if v.IsNil() || !refElem(v.Type().Elem()) {
			return
		}
		for it := v.MapRange(); it.Next(); {
			nv := reflect.New(v.Type().Elem()).Elem()
			nv.Set(it.Value())
			fixNumbers(nv)
			v.SetMapIndex(it.Key(), nv)
		}
	case reflect.Slice:
		if v.IsNil() || !refElem(v.Type().Elem()) {
			return
		}
		for i := 0; i < v.Len(); i++ {
			fixNumbers(v.Index(i))
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath == "" {
				fixNumbers(v.Field(i))
			}
		}
	}
}

// refElem reports an element type that can hold a nested number.
func refElem(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Interface, reflect.Map, reflect.Slice, reflect.Pointer, reflect.Struct:
		return t != reflect.TypeOf(time.Time{})
	}
	return false
}

// tn is a time as a column value: UTC nanoseconds, NULL for the zero time.
func tn(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixNano()
}

func tnp(t *time.Time) any {
	if t == nil {
		return nil
	}
	return tn(*t)
}

func ptrOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
