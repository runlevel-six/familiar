// SPDX-License-Identifier: Apache-2.0

package redact

import "reflect"

// emptySlices returns a copy of v in which every nil slice, at any depth, is
// an empty one, so it encodes as [] rather than null. Callers (alert-gate.py,
// for one) iterate lists without expecting null. Nil maps and pointers are
// left alone: their null means "absent", not "none".
func emptySlices(v any) any {
	if v == nil {
		return nil
	}
	return fill(reflect.ValueOf(v)).Interface()
}

func fill(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(fill(v.Elem()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(fill(v.Elem()))
		return out
	case reflect.Slice:
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(fill(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			out.Index(i).Set(fill(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), fill(it.Value()))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v) // unexported fields (time.Time's, say) copied as they are
		for i := range v.NumField() {
			if out.Field(i).CanSet() {
				out.Field(i).Set(fill(v.Field(i)))
			}
		}
		return out
	}
	return v
}
