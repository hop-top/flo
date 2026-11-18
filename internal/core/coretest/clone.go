package coretest

import "reflect"

// clone returns a deep copy of v: maps, slices, pointers and interface values
// are copied, so a caller that mutates what a fake returned (or what it
// passed in) cannot change the fake's stored state. Go types are kept as they
// are (an int64 stays an int64); real stores read JSON back, and a test that
// cares about that normalizes with core.NormalizeJSON.
func clone[T any](v T) T {
	out := deepCopy(reflect.ValueOf(&v).Elem())
	r, _ := out.Interface().(T) // nil interface value stays the zero T
	return r
}

func deepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		n := reflect.New(v.Type().Elem())
		n.Elem().Set(deepCopy(v.Elem()))
		return n
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopy(v.Elem()))
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(deepCopy(it.Key()), deepCopy(it.Value()))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		if v.Type().Elem().Kind() == reflect.Uint8 {
			reflect.Copy(out, v)
			return out
		}
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(deepCopy(v.Index(i)))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			out.Field(i).Set(deepCopy(v.Field(i)))
		}
		return out
	}
	return v
}
