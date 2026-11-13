package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Key describes one configuration key.
type Key struct {
	Name    string   `json:"name"`    // dotted, e.g. "route.max_ms"
	Table   string   `json:"table"`   // TOML table, e.g. "route"
	Field   string   `json:"field"`   // last component, e.g. "max_ms"
	Type    string   `json:"type"`    // string|bool|int|float|duration|list
	Help    string   `json:"help"`    // one-line description
	Choices []string `json:"choices"` // allowed values (empty = any)
	Min     *float64 `json:"min,omitempty"`
	Env     []string `json:"env"`     // SILKROUTER_* first, then legacy names
	Default string   `json:"default"` // formatted default value

	index []int
}

// Table is one TOML table (section), in declaration order. Tables may be
// empty: they are declared so later features have a stable home.
type Table struct {
	Name string `json:"name"`
	Help string `json:"help"`
	Keys []Key  `json:"keys"`
}

var (
	schemaOnce sync.Once
	tables     []Table
	keys       []Key
	keyIndex   map[string]int
)

var (
	durationType = reflect.TypeOf(Duration(0))
	setterType   = reflect.TypeOf((*interface{ Set(string) error })(nil)).Elem()
)

func schema() {
	schemaOnce.Do(func() {
		keyIndex = map[string]int{}
		def := reflect.ValueOf(Default()).Elem()
		walk(def.Type(), "", "", nil, def)
		for _, t := range tables {
			for _, k := range t.Keys {
				keyIndex[k.Name] = len(keys)
				keys = append(keys, k)
			}
		}
	})
}

func walk(t reflect.Type, prefix, help string, index []int, def reflect.Value) {
	cur := -1
	if prefix != "" {
		tables = append(tables, Table{Name: prefix, Help: help})
		cur = len(tables) - 1
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := strings.Split(f.Tag.Get("toml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		full := name
		if prefix != "" {
			full = prefix + "." + name
		}
		idx := append(append([]int(nil), index...), i)
		if f.Type.Kind() == reflect.Struct && f.Type != durationType {
			walk(f.Type, full, f.Tag.Get("help"), idx, def)
			continue
		}
		if cur < 0 {
			panic("config: top-level key " + name + " must live in a section")
		}
		k := Key{
			Name:  full,
			Table: prefix,
			Field: name,
			Type:  typeName(f.Type),
			Help:  f.Tag.Get("help"),
			Env:   []string{EnvName(full)},
			index: idx,
		}
		if c := f.Tag.Get("choices"); c != "" {
			k.Choices = strings.Split(c, "|")
		}
		if m := f.Tag.Get("min"); m != "" {
			v, err := strconv.ParseFloat(m, 64)
			if err != nil {
				panic("config: bad min tag on " + full)
			}
			k.Min = &v
		}
		if e := f.Tag.Get("env"); e != "" {
			k.Env = append(k.Env, strings.Split(e, ",")...)
		}
		k.Default = format(def.FieldByIndex(idx))
		tables[cur].Keys = append(tables[cur].Keys, k)
	}
}

func typeName(t reflect.Type) string {
	if t == durationType {
		return "duration"
	}
	switch t.Kind() {
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "int"
	case reflect.Float32, reflect.Float64:
		return "float"
	case reflect.Slice:
		return "list"
	default:
		return "string"
	}
}

// EnvName is the SILKROUTER_* variable for a dotted key.
func EnvName(key string) string {
	r := strings.NewReplacer(".", "_", "-", "_")
	return "SILKROUTER_" + strings.ToUpper(r.Replace(key))
}

// Tables returns every table in declaration order.
func Tables() []Table { schema(); return tables }

// Keys returns every key in declaration order.
func Keys() []Key { schema(); return keys }

// KeyNames returns every dotted key name.
func KeyNames() []string {
	ks := Keys()
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.Name
	}
	return out
}

// Lookup finds a key by its dotted name.
func Lookup(name string) (Key, bool) {
	schema()
	i, ok := keyIndex[name]
	if !ok {
		return Key{}, false
	}
	return keys[i], true
}

// UnknownKeyError is returned for keys that do not exist.
type UnknownKeyError struct{ Key string }

func (e UnknownKeyError) Error() string { return fmt.Sprintf("unknown config key %q", e.Key) }

func (c *Config) field(k Key) reflect.Value {
	return reflect.ValueOf(c).Elem().FieldByIndex(k.index)
}

func (k Key) copy(dst, src *Config) { dst.field(k).Set(src.field(k)) }

// Value returns the typed Go value of key (Duration, int, string, ...).
func (c *Config) Value(key string) (any, error) {
	k, ok := Lookup(key)
	if !ok {
		return nil, UnknownKeyError{key}
	}
	return c.field(k).Interface(), nil
}

// Get returns key's value formatted as text (lists comma-separated).
func (c *Config) Get(key string) (string, error) {
	k, ok := Lookup(key)
	if !ok {
		return "", UnknownKeyError{key}
	}
	return format(c.field(k)), nil
}

// Set parses value into key, validating type, choices and minimum.
func (c *Config) Set(key, value string) error {
	k, ok := Lookup(key)
	if !ok {
		return UnknownKeyError{key}
	}
	v := reflect.New(c.field(k).Type()).Elem()
	if err := parseInto(v, value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	if err := checkValue(k, v); err != nil {
		return err
	}
	c.field(k).Set(v)
	return nil
}

// Reset sets key back to its built-in default.
func (c *Config) Reset(key string) error {
	k, ok := Lookup(key)
	if !ok {
		return UnknownKeyError{key}
	}
	k.copy(c, Default())
	return nil
}

// IsDefault reports whether key currently holds its built-in default.
func (c *Config) IsDefault(key string) bool {
	k, ok := Lookup(key)
	return ok && format(c.field(k)) == k.Default
}

func (c *Config) validate(k Key) error { return checkValue(k, c.field(k)) }

// Validate checks every key and returns all problems joined.
func (c *Config) Validate() error {
	var msgs []string
	for _, k := range Keys() {
		if err := c.validate(k); err != nil {
			msgs = append(msgs, err.Error())
		}
	}
	if len(msgs) > 0 {
		return fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	return nil
}

func checkValue(k Key, v reflect.Value) error {
	if len(k.Choices) > 0 {
		s := format(v)
		found := false
		for _, ch := range k.Choices {
			if s == ch {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: invalid value %q (want one of: %s)", k.Name, s, strings.Join(k.Choices, ", "))
		}
	}
	if k.Min != nil {
		var n float64
		switch {
		case v.Type() == durationType:
			n = time.Duration(v.Int()).Seconds()
		case v.CanInt():
			n = float64(v.Int())
		case v.CanUint():
			n = float64(v.Uint())
		case v.CanFloat():
			n = v.Float()
		}
		if n < *k.Min {
			return fmt.Errorf("%s: %s is below the minimum %s", k.Name, format(v), strconv.FormatFloat(*k.Min, 'f', -1, 64))
		}
	}
	return nil
}

func parseInto(v reflect.Value, s string) error {
	if v.CanAddr() && v.Addr().Type().Implements(setterType) {
		return v.Addr().Interface().(interface{ Set(string) error }).Set(s)
	}
	s = strings.TrimSpace(s)
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := parseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid integer %q", s)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid unsigned integer %q", s)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return fmt.Errorf("invalid number %q", s)
		}
		v.SetFloat(f)
	case reflect.Slice:
		if v.Type().Elem().Kind() != reflect.String {
			return fmt.Errorf("unsupported list type %s", v.Type())
		}
		var items []string
		for _, part := range strings.Split(s, ",") {
			if part = strings.TrimSpace(part); part != "" {
				items = append(items, part)
			}
		}
		v.Set(reflect.ValueOf(items).Convert(v.Type()))
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "1", "t", "true", "y", "yes", "on":
		return true, nil
	case "0", "f", "false", "n", "no", "off", "":
		return false, nil
	}
	return false, fmt.Errorf("invalid boolean %q", s)
}

func format(v reflect.Value) string {
	if s, ok := v.Interface().(fmt.Stringer); ok {
		return s.String()
	}
	switch v.Kind() {
	case reflect.Slice:
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = fmt.Sprint(v.Index(i).Interface())
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(v.Interface())
	}
}

// Map returns the config as nested maps (table -> key -> typed value), for
// JSON output.
func (c *Config) Map() map[string]any {
	out := map[string]any{}
	for _, t := range Tables() {
		m := out
		for _, part := range strings.Split(t.Name, ".") {
			sub, ok := m[part].(map[string]any)
			if !ok {
				sub = map[string]any{}
				m[part] = sub
			}
			m = sub
		}
		for _, k := range t.Keys {
			m[k.Field] = c.field(k).Interface()
		}
	}
	return out
}

// Flat returns key -> formatted value, sorted by key when iterated via
// KeyNames order.
func (c *Config) Flat() map[string]string {
	out := make(map[string]string, len(Keys()))
	for _, k := range Keys() {
		out[k.Name] = format(c.field(k))
	}
	return out
}

// SortedKeys returns key names sorted alphabetically.
func SortedKeys() []string {
	out := KeyNames()
	sort.Strings(out)
	return out
}
