// Command cfgfile-fmt formats JWCC configuration files in canonical layout
// (see jwcc.Format), keeping comments, encoding and line endings.
//
//	cfgfile-fmt [flags] [file ...]
//
// Without files it formats standard input to standard output.
//
//	-w        write the result back to each file (atomically)
//	-l        list files whose formatting differs
//	-d        print a line diff for files that differ
//	-check    like -l, and exit with status 1 if any file differs
//	-strict   print standard JSON without comments instead
//	-tabs     indent with tabs instead of two spaces
//	-trailing multiline (default), keep or remove trailing commas
//
// Programs embedding the package can offer the same through
// cfgfile.Format.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/djylb/cfgfile"
	"github.com/djylb/cfgfile/jwcc"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cfgfile-fmt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	write := fs.Bool("w", false, "write the result back to each file")
	list := fs.Bool("l", false, "list files whose formatting differs")
	diff := fs.Bool("d", false, "print a diff for files that differ")
	check := fs.Bool("check", false, "list differing files and exit 1 if any")
	strict := fs.Bool("strict", false, "print standard JSON without comments")
	tabs := fs.Bool("tabs", false, "indent with tabs")
	trailing := fs.String("trailing", "multiline", "trailing commas: multiline, keep or remove")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o := &jwcc.FormatOptions{}
	if *tabs {
		o.Indent = "\t"
	}
	switch *trailing {
	case "multiline":
	case "keep":
		o.Trailing = jwcc.TrailingKeep
	case "remove":
		o.Trailing = jwcc.TrailingRemove
	default:
		// The exit status reports the error even if stderr is gone.
		_, _ = fmt.Fprintf(stderr, "cfgfile-fmt: bad -trailing %q\n", *trailing)
		return 2
	}
	indent := "  "
	if *tabs {
		indent = "\t"
	}
	process := func(name string, raw []byte) (bool, error) {
		if *strict {
			out, err := cfgfile.ToJSON(raw, indent)
			if err == nil {
				_, err = stdout.Write(out)
			}
			return false, err
		}
		out, err := cfgfile.Format(raw, o)
		if err != nil {
			return false, err
		}
		differs := !bytes.Equal(raw, out)
		switch {
		case *write:
			if differs && name != "" {
				err = cfgfile.WriteFile(name, out, 0o644)
			} else if name == "" {
				_, err = stdout.Write(out)
			}
		case *list || *check:
			if differs {
				_, err = fmt.Fprintln(stdout, displayName(name))
			}
		case *diff:
			if differs {
				_, err = io.WriteString(stdout, lineDiff(displayName(name), string(raw), string(out)))
			}
		default:
			_, err = stdout.Write(out)
		}
		return differs, err
	}
	status, anyDiff := 0, false
	report := func(name string, err error) {
		// The exit status reports the error even if stderr is gone.
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", displayName(name), err)
		status = 1
	}
	if fs.NArg() == 0 {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			report("", err)
			return 1
		}
		d, err := process("", raw)
		if err != nil {
			report("", err)
		}
		anyDiff = d
	}
	for _, name := range fs.Args() {
		raw, err := os.ReadFile(name)
		if err != nil {
			report(name, err)
			continue
		}
		d, err := process(name, raw)
		if err != nil {
			report(name, err)
		}
		anyDiff = anyDiff || d
	}
	if *check && anyDiff && status == 0 {
		status = 1
	}
	return status
}

func displayName(name string) string {
	if name == "" {
		return "<stdin>"
	}
	return name
}

// lineDiff prints the lines removed (-) and added (+) between a and b,
// with a longest-common-subsequence match. Very long inputs are only
// reported as different.
func lineDiff(name, a, b string) string {
	x, y := strings.SplitAfter(a, "\n"), strings.SplitAfter(b, "\n")
	var out strings.Builder
	out.WriteString("--- " + name + "\n+++ " + name + " (formatted)\n")
	if len(x)*len(y) > 4<<20 {
		out.WriteString("files differ (too long to diff)\n")
		return out.String()
	}
	lcs := make([][]int32, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(y)+1)
	}
	for i, v := range slices.Backward(x) {
		for j := len(y) - 1; j >= 0; j-- {
			if v == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	line := func(prefix, s string) {
		out.WriteString(prefix + strings.TrimRight(s, "\r\n") + "\n")
	}
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			line("+", y[j])
			j++
		default:
			line("-", x[i])
			i++
		}
	}
	return out.String()
}
