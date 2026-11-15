package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/szmelc-inc/silkrouter/internal/config"
)

// CompleteKind says how a flag value or positional argument completes.
type CompleteKind int

// Completion kinds.
const (
	CompleteNone    CompleteKind = iota // free text, nothing to offer
	CompleteFile                        // file names
	CompleteDir                         // directory names
	CompleteChoices                     // a fixed word list (Words)
	CompleteDynamic                     // `prog __complete <Topic>` at TAB time
	CompleteCommand                     // a program name, then its own args
)

// Completion describes what completes for a flag value or positional args.
type Completion struct {
	Kind  CompleteKind
	Words []string // CompleteChoices
	Topic string   // CompleteDynamic
	// Repeat makes choices/dynamic values complete for every positional
	// argument instead of only the first ("config get KEY" vs "help A B").
	Repeat bool
}

// Convenience constructors for Command.Args.
var (
	ArgsFiles   = Completion{Kind: CompleteFile}
	ArgsDirs    = Completion{Kind: CompleteDir}
	ArgsCommand = Completion{Kind: CompleteCommand}
)

// ArgsChoices completes positional arguments from a fixed list.
func ArgsChoices(words ...string) Completion {
	return Completion{Kind: CompleteChoices, Words: words}
}

// ArgsDynamic completes positional arguments by running
// `prog __complete <topic>` (see RegisterCompleter).
func ArgsDynamic(topic string) Completion {
	return Completion{Kind: CompleteDynamic, Topic: topic}
}

// Flag is the metadata of one flag (all its names share a single value).
type Flag struct {
	// Names lists every spelling without dashes: single letters are short
	// flags ("d"), longer ones long flags ("max-ms"). Each works as -name
	// and --name.
	Names []string
	Usage string
	// Meta is the value placeholder in help ("MS", "FILE"); empty for bools.
	Meta string
	// Default is the default value as shown in help ("" hides it).
	Default string
	IsBool  bool
	// Key is the config key whose value is this flag's default.
	Key      string
	Complete Completion
	Hidden   bool
	// Global marks the flags every command accepts (--no-color, -q, -v).
	Global bool
}

// Short returns the single-letter names.
func (f *Flag) Short() []string { return f.filter(true) }

// Long returns the multi-letter names.
func (f *Flag) Long() []string { return f.filter(false) }

func (f *Flag) filter(short bool) []string {
	var out []string
	for _, n := range f.Names {
		if (len(n) == 1) == short {
			out = append(out, n)
		}
	}
	return out
}

// Spellings returns every accepted spelling (-x, --x, -long, --long).
func (f *Flag) Spellings() []string {
	var out []string
	for _, n := range f.Names {
		out = append(out, "-"+n, "--"+n)
	}
	return out
}

// TakesValue reports whether the flag consumes an argument.
func (f *Flag) TakesValue() bool { return !f.IsBool }

// Placeholder sets the value placeholder shown in help.
func (f *Flag) Placeholder(meta string) *Flag { f.Meta = meta; return f }

// ConfigKey records that the flag's default comes from a config key; help
// shows it so users know where to change the default.
func (f *Flag) ConfigKey(key string) *Flag { f.Key = key; return f }

// Files makes the value complete as a file name.
func (f *Flag) Files() *Flag { f.Complete = Completion{Kind: CompleteFile}; return f }

// Dirs makes the value complete as a directory name.
func (f *Flag) Dirs() *Flag { f.Complete = Completion{Kind: CompleteDir}; return f }

// Choices makes the value complete from a fixed list.
func (f *Flag) Choices(words ...string) *Flag {
	f.Complete = Completion{Kind: CompleteChoices, Words: words}
	return f
}

// Dynamic makes the value complete via `prog __complete <topic>`.
func (f *Flag) Dynamic(topic string) *Flag {
	f.Complete = Completion{Kind: CompleteDynamic, Topic: topic}
	return f
}

// Hide keeps the flag working but out of help and completion.
func (f *Flag) Hide() *Flag { f.Hidden = true; return f }

// NoDefault hides the default value in help.
func (f *Flag) NoDefault() *Flag { f.Default = ""; return f }

// FlagSet wraps a standard flag.FlagSet with help/completion metadata and
// multi-name flags ("d,max-ms").
type FlagSet struct {
	// Std is the underlying standard-library flag set.
	Std *flag.FlagSet
	// Config is the loaded configuration; use it for flag defaults.
	Config *config.Config
	flags  []*Flag
}

// NewFlagSet returns an empty flag set named name. cfg may be nil (defaults
// are used then).
func NewFlagSet(name string, cfg *config.Config) *FlagSet {
	if cfg == nil {
		cfg = config.Default()
	}
	std := flag.NewFlagSet(name, flag.ContinueOnError)
	std.SetOutput(io.Discard)
	std.Usage = func() {}
	return &FlagSet{Std: std, Config: cfg}
}

// Flags returns the declared flags in declaration order.
func (fs *FlagSet) Flags() []*Flag { return fs.flags }

// Visible returns the non-hidden flags.
func (fs *FlagSet) Visible() []*Flag {
	var out []*Flag
	for _, f := range fs.flags {
		if !f.Hidden {
			out = append(out, f)
		}
	}
	return out
}

// Lookup returns the flag that has name (no dashes), or nil.
func (fs *FlagSet) Lookup(name string) *Flag {
	for _, f := range fs.flags {
		for _, n := range f.Names {
			if n == name {
				return f
			}
		}
	}
	return nil
}

// Has reports whether any of names is already defined.
func (fs *FlagSet) Has(names string) bool {
	for _, n := range splitNames(names) {
		if fs.Std.Lookup(n) != nil {
			return true
		}
	}
	return false
}

// Changed reports whether the flag with name was given on the command line
// (under any of its names).
func (fs *FlagSet) Changed(name string) bool {
	f := fs.Lookup(name)
	names := []string{name}
	if f != nil {
		names = f.Names
	}
	changed := false
	fs.Std.Visit(func(sf *flag.Flag) {
		for _, n := range names {
			if sf.Name == n {
				changed = true
			}
		}
	})
	return changed
}

func splitNames(names string) []string {
	var out []string
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimLeft(strings.TrimSpace(n), "-")
		if n != "" {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		panic("cli: flag without a name")
	}
	return out
}

func (fs *FlagSet) add(names string, usage string, def string, isBool bool, v flag.Value) *Flag {
	f := &Flag{Names: splitNames(names), Usage: usage, Default: def, IsBool: isBool}
	sort.SliceStable(f.Names, func(i, j int) bool {
		return len(f.Names[i]) == 1 && len(f.Names[j]) != 1
	})
	for _, n := range f.Names {
		fs.Std.Var(v, n, usage)
	}
	fs.flags = append(fs.flags, f)
	return f
}

// String defines a string flag under every name in names ("f,cache").
func (fs *FlagSet) String(p *string, names, value, usage string) *Flag {
	*p = value
	return fs.add(names, usage, value, false, (*stringValue)(p)).Placeholder("VALUE")
}

// Bool defines a boolean flag.
func (fs *FlagSet) Bool(p *bool, names string, value bool, usage string) *Flag {
	*p = value
	def := ""
	if value {
		def = "true"
	}
	return fs.add(names, usage, def, true, (*boolValue)(p)).Placeholder("")
}

// Int defines an integer flag.
func (fs *FlagSet) Int(p *int, names string, value int, usage string) *Flag {
	*p = value
	return fs.add(names, usage, strconv.Itoa(value), false, (*intValue)(p)).Placeholder("N")
}

// Float defines a float flag.
func (fs *FlagSet) Float(p *float64, names string, value float64, usage string) *Flag {
	*p = value
	return fs.add(names, usage, strconv.FormatFloat(value, 'g', -1, 64), false, (*floatValue)(p)).Placeholder("X")
}

// Duration defines a duration flag: a bare number means seconds ("8",
// "0.5"), otherwise a Go duration ("8s", "500ms", "1m").
func (fs *FlagSet) Duration(p *config.Duration, names string, value config.Duration, usage string) *Flag {
	*p = value
	return fs.add(names, usage, value.String(), false, p).Placeholder("DUR")
}

// Var defines a flag with a custom flag.Value. isBool must match whether v
// implements IsBoolFlag() bool returning true.
func (fs *FlagSet) Var(v flag.Value, names string, isBool bool, usage string) *Flag {
	return fs.add(names, usage, v.String(), isBool, v)
}

type stringValue string

func (s *stringValue) Set(v string) error { *s = stringValue(v); return nil }
func (s *stringValue) String() string     { return string(*s) }

type boolValue bool

func (b *boolValue) Set(v string) error {
	x, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("invalid boolean %q", v)
	}
	*b = boolValue(x)
	return nil
}
func (b *boolValue) String() string   { return strconv.FormatBool(bool(*b)) }
func (b *boolValue) IsBoolFlag() bool { return true }

type intValue int

func (i *intValue) Set(v string) error {
	x, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("invalid integer %q", v)
	}
	*i = intValue(x)
	return nil
}
func (i *intValue) String() string { return strconv.Itoa(int(*i)) }

type floatValue float64

func (f *floatValue) Set(v string) error {
	x, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("invalid number %q", v)
	}
	*f = floatValue(x)
	return nil
}
func (f *floatValue) String() string { return strconv.FormatFloat(float64(*f), 'g', -1, 64) }

// parse parses args. With interspersed set, flags may follow positional
// arguments; "--" always ends flag parsing. It returns the positionals.
func (fs *FlagSet) parse(args []string, interspersed bool) ([]string, error) {
	if !interspersed {
		if err := fs.Std.Parse(args); err != nil {
			return nil, err
		}
		return fs.Std.Args(), nil
	}
	var pos []string
	for {
		if err := fs.Std.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Std.Args()
		used := len(args) - len(rest)
		if used > 0 && args[used-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}
