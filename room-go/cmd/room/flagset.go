package main

import (
	"flag"
	"fmt"
	"os"
)

// flagSet is a thin wrapper over the stdlib flag package for per-subcommand
// flags. It keeps the subcommand definitions terse and centralizes parse-error
// handling (usage errors map to exit code 2, per SPEC §2).
type flagSet struct {
	fs *flag.FlagSet
}

func newFlagSet(name string) *flagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return &flagSet{fs: fs}
}

func (f *flagSet) string(name, def, usage string) *string         { return f.fs.String(name, def, usage) }
func (f *flagSet) bool(name string, def bool, usage string) *bool { return f.fs.Bool(name, def, usage) }
func (f *flagSet) int(name string, def int, usage string) *int    { return f.fs.Int(name, def, usage) }

// parse parses args, returning a non-zero exit code on usage error.
func (f *flagSet) parse(args []string) int {
	if err := f.fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "room %s: %v\n", f.fs.Name(), err)
		return 2
	}
	return 0
}

// parseInterspersed parses flags that may appear *after* positional arguments
// (the stdlib flag package stops at the first positional). It returns the
// collected positionals and a non-zero exit code on usage error. This lets
// `room remote <host> --stop` work with the host in front of the flags.
func (f *flagSet) parseInterspersed(args []string) ([]string, int) {
	var positionals []string
	rest := args
	for len(rest) > 0 {
		if err := f.fs.Parse(rest); err != nil {
			fmt.Fprintf(os.Stderr, "room %s: %v\n", f.fs.Name(), err)
			return nil, 2
		}
		r := f.fs.Args()
		if len(r) == 0 {
			break
		}
		positionals = append(positionals, r[0])
		rest = r[1:]
	}
	return positionals, 0
}

func (f *flagSet) args() []string { return f.fs.Args() }
