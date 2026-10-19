package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/nilsjesper/samling/internal/library"
)

// --- survivors ---------------------------------------------------------------------

func cmdSurvivors(args []string) error {
	return runSurvivors(args, os.Stdout, flag.ExitOnError)
}

// runSurvivors is cmdSurvivors with its output and flag error handling
// injectable, so tests can read what it prints.
func runSurvivors(args []string, w io.Writer, onErr flag.ErrorHandling) error {
	fs := flag.NewFlagSet("survivors", onErr)
	if onErr == flag.ContinueOnError {
		fs.SetOutput(io.Discard)
	}
	minKept := fs.Int("min", 1, "only articles kept at least this many times")
	// The limit is a bare number, as with pick and list, and may come before
	// or after the flags.
	leading := -1
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if v, err := strconv.Atoi(args[0]); err == nil {
			leading, args = v, args[1:]
		}
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	limit := 0 // all of them
	if leading >= 0 || len(fs.Args()) > 0 {
		var err error
		if limit, err = resolvePickCount(leading, 0, fs.Args()); err != nil {
			return err
		}
	}
	if *minKept < 1 {
		return fmt.Errorf("--min must be at least 1, got %d", *minKept)
	}

	lib, err := library.Load(library.DefaultPath())
	if err != nil {
		return err
	}
	survivors := lib.Survivors(*minKept)
	if len(survivors) == 0 {
		if *minKept > 1 {
			fmt.Fprintf(w, "Nothing kept %d times or more yet.\n", *minKept)
		} else {
			fmt.Fprintln(w, "No survivors yet. Re-save a picked article you want to keep, then sync,\nand it shows up here.")
		}
		return nil
	}
	if limit > 0 && len(survivors) > limit {
		survivors = survivors[:limit]
	}

	// Pad the count so titles line up when ×9 and ×10 share a list.
	width := 0
	for _, b := range survivors {
		width = max(width, len([]rune(keptTimes(b.Kept()))))
	}
	for _, b := range survivors {
		kept := keptTimes(b.Kept())
		line := kept + strings.Repeat(" ", width-len([]rune(kept))) + "  " + titleOf(b)
		var meta []string
		if h := b.Host(); h != "" {
			meta = append(meta, h)
		}
		if b.Time > 0 {
			meta = append(meta, formatAge(b.Age())+" old")
		}
		if len(meta) > 0 {
			line += " · " + strings.Join(meta, " · ")
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

// --- the --kept filter -------------------------------------------------------------

// keptFlag is pick and list's --kept K. It acts as a boolean flag so that a
// bare --kept means "kept at least once", which the flag package only allows
// for flags that report IsBoolFlag. The catch is that the flag package then
// never consumes a following "--kept 2" value; joinKeptValue fixes that up
// before parsing.
type keptFlag int

func (k *keptFlag) String() string {
	if k == nil {
		return "0"
	}
	return strconv.Itoa(int(*k))
}

func (k *keptFlag) Set(s string) error {
	if s == "true" { // what the flag package passes for a bare --kept
		*k = 1
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 {
		return errors.New("want a count of at least 1")
	}
	*k = keptFlag(v)
	return nil
}

func (k *keptFlag) IsBoolFlag() bool { return true }

// joinKeptValue rewrites "--kept 2" as "--kept=2", so the number is read as
// the flag's value rather than as the pick count. A number right after --kept
// always belongs to it: write "pick 3 --kept" or "pick --kept=1 3" to give a
// bare --kept and a count.
func joinKeptValue(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...)
		}
		if (a == "--kept" || a == "-kept") && i+1 < len(args) {
			if _, err := strconv.Atoi(args[i+1]); err == nil {
				out = append(out, a+"="+args[i+1])
				i++
				continue
			}
		}
		out = append(out, a)
	}
	return out
}
