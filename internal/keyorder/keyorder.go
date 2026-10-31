// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package keyorder orders map keys the same way in every process, for
// deterministic map iteration. It is Go's internal/fmtsort with two changes:
// interface keys order their concrete types by name, not by the address of
// the type, and pointer and channel keys, which have only an address, are
// refused.
package keyorder

import (
	"cmp"
	"reflect"
	"slices"
)

// Unordered is the panic value for a key that has no fixed order.
type Unordered struct{ Type reflect.Type }

func (u Unordered) Error() string {
	return "deterministic run: map keys of type " + u.Type.String() + " have no fixed order"
}

// KeyValue holds a single key and value pair found in a map.
type KeyValue struct {
	Key, Value reflect.Value
}

// Sort returns the entries of m sorted by key. It panics with Unordered when
// a key contains a pointer or a channel.
func Sort(m reflect.Value) []KeyValue {
	sorted := make([]KeyValue, 0, m.Len())
	iter := m.MapRange()
	for iter.Next() {
		sorted = append(sorted, KeyValue{iter.Key(), iter.Value()})
	}
	slices.SortStableFunc(sorted, func(a, b KeyValue) int {
		return compare(a.Key, b.Key)
	})
	return sorted
}

func compare(aVal, bVal reflect.Value) int {
	aType, bType := aVal.Type(), bVal.Type()
	if aType != bType {
		// ponytail: two distinct types with one name compare equal and keep
		// map order; give them a tiebreak if a program ever has that.
		return cmp.Compare(aType.String(), bType.String())
	}
	switch aVal.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return cmp.Compare(aVal.Int(), bVal.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return cmp.Compare(aVal.Uint(), bVal.Uint())
	case reflect.String:
		return cmp.Compare(aVal.String(), bVal.String())
	case reflect.Float32, reflect.Float64:
		// ponytail: several NaN keys compare equal and keep map order.
		return cmp.Compare(aVal.Float(), bVal.Float())
	case reflect.Complex64, reflect.Complex128:
		a, b := aVal.Complex(), bVal.Complex()
		if c := cmp.Compare(real(a), real(b)); c != 0 {
			return c
		}
		return cmp.Compare(imag(a), imag(b))
	case reflect.Bool:
		a, b := aVal.Bool(), bVal.Bool()
		switch {
		case a == b:
			return 0
		case a:
			return 1
		default:
			return -1
		}
	case reflect.Struct:
		for i := 0; i < aVal.NumField(); i++ {
			if c := compare(aVal.Field(i), bVal.Field(i)); c != 0 {
				return c
			}
		}
		return 0
	case reflect.Array:
		for i := 0; i < aVal.Len(); i++ {
			if c := compare(aVal.Index(i), bVal.Index(i)); c != 0 {
				return c
			}
		}
		return 0
	case reflect.Interface:
		if aVal.IsNil() || bVal.IsNil() {
			return cmp.Compare(boolInt(!aVal.IsNil()), boolInt(!bVal.IsNil()))
		}
		return compare(aVal.Elem(), bVal.Elem())
	default:
		panic(Unordered{aType})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
