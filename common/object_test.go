package common

import (
	"testing"
	"time"

	"github.com/grafana/sobek"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MapJavascriptObjectToGo copies a JavaScript object into a Go struct by
// reflection. It is exported from `common` but has no caller left anywhere in
// this repository — see the report note. These tests pin its contract, because
// every conversion it gets wrong lands silently in a config value rather than
// as an error.

// jsValue evaluates a JavaScript expression and returns the resulting value.
func jsValue(t *testing.T, src string) sobek.Value {
	t.Helper()

	rt := sobek.New()
	v, err := rt.RunString("(" + src + ")")
	require.NoError(t, err)
	return v
}

// ---------------------------------------------------------------------------
// Destination validation
// ---------------------------------------------------------------------------

func TestMapJavascriptObjectToGo_RejectsBadDestinations(t *testing.T) {
	t.Parallel()

	src := jsValue(t, `{ Name: "x" }`)

	type target struct{ Name string }

	t.Run("a non-pointer destination", func(t *testing.T) {
		t.Parallel()
		err := MapJavascriptObjectToGo(src, target{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-nil pointer")
	})

	t.Run("a nil pointer destination", func(t *testing.T) {
		t.Parallel()
		var dest *target
		err := MapJavascriptObjectToGo(src, dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-nil pointer")
	})

	t.Run("a pointer to a non-struct", func(t *testing.T) {
		t.Parallel()
		var dest string
		err := MapJavascriptObjectToGo(src, &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must point to a struct")
	})
}

// An absent source must leave the destination untouched rather than zero it.
func TestMapJavascriptObjectToGo_AbsentSourceLeavesDestinationAlone(t *testing.T) {
	t.Parallel()

	type target struct{ Name string }

	for name, src := range map[string]sobek.Value{
		"nil":       nil,
		"undefined": sobek.Undefined(),
		"null":      sobek.Null(),
	} {
		t.Run(name, func(t *testing.T) {
			dest := target{Name: "preset"}
			require.NoError(t, MapJavascriptObjectToGo(src, &dest))
			assert.Equal(t, "preset", dest.Name, "an absent source must not clear the field")
		})
	}
}

// ---------------------------------------------------------------------------
// Field naming and skipping
// ---------------------------------------------------------------------------

// The mapper reads a field's json tag when there is one, and its Go name when
// there is not. Getting this wrong silently drops config the operator wrote.
func TestMapJavascriptObjectToGo_FieldNaming(t *testing.T) {
	t.Parallel()

	type target struct {
		Tagged     string `json:"tagged,omitempty"`
		Untagged   string
		unexported string //nolint:unused // present to prove it is skipped
	}

	dest := target{}
	require.NoError(t, MapJavascriptObjectToGo(
		jsValue(t, `{ tagged: "from tag", Tagged: "from go name", Untagged: "from go name", unexported: "nope" }`),
		&dest))

	assert.Equal(t, "from tag", dest.Tagged, "a json tag must win over the Go field name")
	assert.Equal(t, "from go name", dest.Untagged)
	assert.Equal(t, "", dest.unexported, "an unexported field must be skipped, not panic")
}

// A key the JavaScript object does not carry must leave the Go field as it was.
func TestMapJavascriptObjectToGo_MissingKeysAreSkipped(t *testing.T) {
	t.Parallel()

	type target struct {
		Present string `json:"present"`
		Absent  string `json:"absent"`
	}

	t.Run("a key the source omits", func(t *testing.T) {
		t.Parallel()

		dest := target{Present: "old", Absent: "keep me"}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ present: "new" }`), &dest))

		assert.Equal(t, "new", dest.Present)
		assert.Equal(t, "keep me", dest.Absent, "an omitted key must not clear the field")
	})

	t.Run("a key the source sets to undefined", func(t *testing.T) {
		t.Parallel()

		dest := target{Present: "old", Absent: "keep me"}
		require.NoError(t, MapJavascriptObjectToGo(
			jsValue(t, `{ present: "new", absent: undefined }`), &dest))

		assert.Equal(t, "new", dest.Present)
		assert.Equal(t, "keep me", dest.Absent,
			"an explicit undefined must not overwrite the field with an empty string")
	})
}

// ---------------------------------------------------------------------------
// Duration
// ---------------------------------------------------------------------------

// Duration is the field type that most often carries an operator's intent. A
// string is parsed as a Go duration; a bare number means milliseconds. Mixing
// those two up changes a timeout by three orders of magnitude.
func TestMapJavascriptObjectToGo_Duration(t *testing.T) {
	t.Parallel()

	type target struct {
		Timeout Duration `json:"timeout"`
	}

	for _, tc := range []struct {
		name string
		src  string
		want Duration
	}{
		{"a seconds string", `{ timeout: "5s" }`, Duration(5 * time.Second)},
		{"a minutes string", `{ timeout: "1m" }`, Duration(time.Minute)},
		{"a compound string", `{ timeout: "1h30m" }`, Duration(90 * time.Minute)},
		{"a number is milliseconds", `{ timeout: 250 }`, Duration(250 * time.Millisecond)},
		{"zero", `{ timeout: 0 }`, Duration(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dest := target{}
			require.NoError(t, MapJavascriptObjectToGo(jsValue(t, tc.src), &dest))
			assert.Equal(t, tc.want, dest.Timeout)
		})
	}

	t.Run("an unparsable string is an error", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ timeout: "five seconds" }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration format")
		assert.Contains(t, err.Error(), "timeout", "the error must name the field that failed")
	})
}

// ---------------------------------------------------------------------------
// RateLimitPeriod
// ---------------------------------------------------------------------------

// RateLimitPeriod accepts several spellings per period. A wrong mapping here
// multiplies or divides an operator's budget without any error.
func TestMapJavascriptObjectToGo_RateLimitPeriod(t *testing.T) {
	t.Parallel()

	type target struct {
		Period RateLimitPeriod `json:"period"`
	}

	for _, tc := range []struct {
		src  string
		want RateLimitPeriod
	}{
		{`{ period: "second" }`, RateLimitPeriodSecond},
		{`{ period: "1s" }`, RateLimitPeriodSecond},
		{`{ period: "  MINUTE  " }`, RateLimitPeriodMinute},
		{`{ period: "60s" }`, RateLimitPeriodMinute},
		{`{ period: "hour" }`, RateLimitPeriodHour},
		{`{ period: "3600s" }`, RateLimitPeriodHour},
		{`{ period: "day" }`, RateLimitPeriodDay},
		{`{ period: "24h" }`, RateLimitPeriodDay},
		{`{ period: "week" }`, RateLimitPeriodWeek},
		{`{ period: "168h" }`, RateLimitPeriodWeek},
		{`{ period: "month" }`, RateLimitPeriodMonth},
		{`{ period: "30d" }`, RateLimitPeriodMonth},
		{`{ period: "year" }`, RateLimitPeriodYear},
		{`{ period: "8760h" }`, RateLimitPeriodYear},
	} {
		t.Run(tc.src, func(t *testing.T) {
			t.Parallel()

			dest := target{}
			require.NoError(t, MapJavascriptObjectToGo(jsValue(t, tc.src), &dest))
			assert.Equal(t, tc.want, dest.Period)
		})
	}

	for _, tc := range []struct {
		name    string
		src     string
		wantMsg string
	}{
		{"an unknown spelling", `{ period: "fortnight" }`, "invalid rate limit period: fortnight"},
		{"a number outside the enum", `{ period: 99 }`, "invalid rate limit period: 99"},
		{"a boolean", `{ period: true }`, "invalid rate limit period type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dest := target{}
			err := MapJavascriptObjectToGo(jsValue(t, tc.src), &dest)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// The numeric form must accept every value the enum defines and reject the rest.
func TestMapJavascriptObjectToGo_RateLimitPeriodNumeric(t *testing.T) {
	t.Parallel()

	type target struct {
		Period RateLimitPeriod `json:"period"`
	}

	for _, want := range []RateLimitPeriod{
		RateLimitPeriodSecond, RateLimitPeriodMinute, RateLimitPeriodHour,
		RateLimitPeriodDay, RateLimitPeriodWeek, RateLimitPeriodMonth, RateLimitPeriodYear,
	} {
		t.Run(want.String(), func(t *testing.T) {
			t.Parallel()

			rt := sobek.New()
			obj := rt.NewObject()
			require.NoError(t, obj.Set("period", int64(want)))

			dest := target{}
			require.NoError(t, MapJavascriptObjectToGo(obj, &dest))
			assert.Equal(t, want, dest.Period)
		})
	}
}

// ---------------------------------------------------------------------------
// Scalar kinds
// ---------------------------------------------------------------------------

func TestMapJavascriptObjectToGo_Scalars(t *testing.T) {
	t.Parallel()

	type target struct {
		Flag    bool    `json:"flag"`
		Signed  int     `json:"signed"`
		Small   int8    `json:"small"`
		Wide    int64   `json:"wide"`
		Count   uint    `json:"count"`
		Byte    uint8   `json:"byte"`
		Ratio   float64 `json:"ratio"`
		Ratio32 float32 `json:"ratio32"`
		Name    string  `json:"name"`
	}

	dest := target{}
	require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{
		flag: true, signed: -7, small: 12, wide: 9007199254740991,
		count: 42, byte: 255, ratio: 0.25, ratio32: 1.5, name: "erpc"
	}`), &dest))

	assert.Equal(t, target{
		Flag: true, Signed: -7, Small: 12, Wide: 9007199254740991,
		Count: 42, Byte: 255, Ratio: 0.25, Ratio32: 1.5, Name: "erpc",
	}, dest)
}

// JavaScript coerces freely. The mapper follows those rules, so a test pins
// which coercions an operator actually gets.
func TestMapJavascriptObjectToGo_ScalarCoercion(t *testing.T) {
	t.Parallel()

	type target struct {
		Flag   bool   `json:"flag"`
		Signed int    `json:"signed"`
		Name   string `json:"name"`
	}

	dest := target{}
	require.NoError(t, MapJavascriptObjectToGo(
		jsValue(t, `{ flag: "non-empty", signed: "17", name: 99 }`), &dest))

	assert.True(t, dest.Flag, "a non-empty string is truthy in JavaScript")
	assert.Equal(t, 17, dest.Signed, "a numeric string coerces to a number")
	assert.Equal(t, "99", dest.Name, "a number coerces to its text")
}

// ---------------------------------------------------------------------------
// Composite kinds
// ---------------------------------------------------------------------------

func TestMapJavascriptObjectToGo_NestedStruct(t *testing.T) {
	t.Parallel()

	type inner struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	type outer struct {
		Name     string `json:"name"`
		Endpoint inner  `json:"endpoint"`
	}

	dest := outer{}
	require.NoError(t, MapJavascriptObjectToGo(
		jsValue(t, `{ name: "rpc-alpha", endpoint: { host: "localhost", port: 8545 } }`), &dest))

	assert.Equal(t, outer{Name: "rpc-alpha", Endpoint: inner{Host: "localhost", Port: 8545}}, dest)
}

// A sobek.Value field is meant to be stored verbatim. Today it panics.
//
// object.go guards the pass-through inside `case reflect.Struct` and compares
// the field type against `reflect.TypeOf(sobek.Value(nil))`. sobek.Value is an
// interface, so that expression is nil and the comparison never holds — and the
// field's kind is Interface, not Struct, so the guard is never even reached.
// The field falls through to `case reflect.Interface`, which reflect-Sets the
// EXPORTED Go value into a sobek.Value field and panics.
//
// This test pins the panic. Fix object.go and it will fail, which is the point.
func TestMapJavascriptObjectToGo_SobekValueFieldPanics(t *testing.T) {
	t.Parallel()

	type target struct {
		Raw sobek.Value `json:"raw"`
	}

	dest := target{}
	assert.PanicsWithValue(t,
		"reflect.Set: value of type map[string]interface {} is not assignable to type sobek.Value",
		func() { _ = MapJavascriptObjectToGo(jsValue(t, `{ raw: { anything: 1 } }`), &dest) },
		"a sobek.Value field panics instead of being stored verbatim")
}

func TestMapJavascriptObjectToGo_Pointer(t *testing.T) {
	t.Parallel()

	type target struct {
		Count *int    `json:"count"`
		Name  *string `json:"name"`
	}

	t.Run("a value allocates the pointer", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ count: 5, name: "x" }`), &dest))

		require.NotNil(t, dest.Count)
		assert.Equal(t, 5, *dest.Count)
		require.NotNil(t, dest.Name)
		assert.Equal(t, "x", *dest.Name)
	})

	t.Run("null leaves the pointer nil", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ count: null }`), &dest))
		assert.Nil(t, dest.Count, "an explicit null must not allocate a zero value")
	})

	t.Run("a pointee that fails to convert is an error", func(t *testing.T) {
		t.Parallel()

		type durationPtr struct {
			Timeout *Duration `json:"timeout"`
		}

		dest := durationPtr{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ timeout: "not a duration" }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration format")
		assert.Nil(t, dest.Timeout, "a failed conversion must not leave a half-built pointer")
	})
}

func TestMapJavascriptObjectToGo_Slice(t *testing.T) {
	t.Parallel()

	type item struct {
		Id string `json:"id"`
	}
	type target struct {
		Names []string `json:"names"`
		Items []item   `json:"items"`
	}

	t.Run("an array of scalars and of objects", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{
			names: ["a", "b"], items: [{ id: "one" }, { id: "two" }]
		}`), &dest))

		assert.Equal(t, []string{"a", "b"}, dest.Names)
		assert.Equal(t, []item{{Id: "one"}, {Id: "two"}}, dest.Items)
	})

	t.Run("an empty array gives an empty slice", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ names: [] }`), &dest))
		assert.NotNil(t, dest.Names)
		assert.Empty(t, dest.Names)
	})

	t.Run("null leaves the slice nil", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ names: null }`), &dest))
		assert.Nil(t, dest.Names)
	})

	t.Run("a non-array is an error", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ names: "not an array" }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected array")
	})

	t.Run("an element that fails to convert is an error", func(t *testing.T) {
		t.Parallel()

		type durations struct {
			Timeouts []Duration `json:"timeouts"`
		}

		dest := durations{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ timeouts: ["1s", "not a duration"] }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration format")
	})
}

func TestMapJavascriptObjectToGo_Map(t *testing.T) {
	t.Parallel()

	type target struct {
		Headers map[string]string `json:"headers"`
		Limits  map[string]int    `json:"limits"`
	}

	t.Run("string keys with scalar values", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{
			headers: { "x-api-key": "secret", "accept": "application/json" },
			limits: { rps: 100 }
		}`), &dest))

		assert.Equal(t, map[string]string{"x-api-key": "secret", "accept": "application/json"}, dest.Headers)
		assert.Equal(t, map[string]int{"rps": 100}, dest.Limits)
	})

	t.Run("an existing map is added to, not replaced", func(t *testing.T) {
		t.Parallel()

		dest := target{Headers: map[string]string{"kept": "yes"}}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ headers: { added: "also yes" } }`), &dest))

		assert.Equal(t, map[string]string{"kept": "yes", "added": "also yes"}, dest.Headers)
	})

	t.Run("null leaves the map nil", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ headers: null }`), &dest))
		assert.Nil(t, dest.Headers)
	})

	t.Run("an entry set to undefined is skipped", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(
			jsValue(t, `{ headers: { kept: "yes", dropped: undefined } }`), &dest))

		assert.Equal(t, map[string]string{"kept": "yes"}, dest.Headers,
			"an undefined entry must not become an empty string")
	})

	t.Run("a non-object is an error", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ headers: "not an object" }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected object")
	})

	t.Run("a non-string key type is an error", func(t *testing.T) {
		t.Parallel()

		type intKeyed struct {
			Limits map[int]string `json:"limits"`
		}

		dest := intKeyed{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ limits: { "1": "a" } }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported map key type")
	})

	t.Run("a value that fails to convert is an error", func(t *testing.T) {
		t.Parallel()

		type durationMap struct {
			Timeouts map[string]Duration `json:"timeouts"`
		}

		dest := durationMap{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ timeouts: { slow: "not a duration" } }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to set map value for key slow")
	})
}

// A function field must be captured as a callable, so the config can carry a
// user-written policy.
func TestMapJavascriptObjectToGo_Func(t *testing.T) {
	t.Parallel()

	type target struct {
		Eval sobek.Callable `json:"eval"`
	}

	t.Run("a function is captured and stays callable", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		require.NoError(t, MapJavascriptObjectToGo(
			jsValue(t, `{ eval: function (a, b) { return a + b; } }`), &dest))

		require.NotNil(t, dest.Eval)
		rt := sobek.New()
		out, err := dest.Eval(sobek.Undefined(), rt.ToValue(2), rt.ToValue(3))
		require.NoError(t, err)
		assert.Equal(t, int64(5), out.ToInteger())
	})

	t.Run("a non-function object is an error", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ eval: { not: "a function" } }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "field is not a function")
	})

	t.Run("a scalar is an error", func(t *testing.T) {
		t.Parallel()

		dest := target{}
		err := MapJavascriptObjectToGo(jsValue(t, `{ eval: 42 }`), &dest)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "field is not a function")
	})
}

// An interface{} field takes whatever sobek exported, unchanged.
func TestMapJavascriptObjectToGo_Interface(t *testing.T) {
	t.Parallel()

	type target struct {
		Anything interface{} `json:"anything"`
	}

	dest := target{}
	require.NoError(t, MapJavascriptObjectToGo(jsValue(t, `{ anything: [1, "two", true] }`), &dest))
	assert.Equal(t, []interface{}{int64(1), "two", true}, dest.Anything)
}

// A destination kind the mapper does not handle must be reported, not silently
// left at its zero value.
func TestMapJavascriptObjectToGo_UnsupportedKind(t *testing.T) {
	t.Parallel()

	type target struct {
		Ch chan int `json:"ch"`
	}

	dest := target{}
	err := MapJavascriptObjectToGo(jsValue(t, `{ ch: 1 }`), &dest)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported kind: chan")
}
