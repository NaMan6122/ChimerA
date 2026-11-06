package main

// runSessions handles `chimera sessions <command>` — read-only access to the
// preserved transcripts. Sessions are PRESERVATION, not resumption: the
// provider side of an old conversation expires and is out of our control, so
// what we keep is a readable runbook of what happened.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chimera/chimera/internal/sessions"
	"github.com/chimera/chimera/internal/spin"
)

const sessionsUsage = `Usage: chimera sessions <command> [flags]

Commands:
  list                       index of recorded sessions, newest first
  search <query>             full-text scan of every transcript (Text,
                             Arguments, Result fields, case-insensitive),
                             newest session first
  show <id>                  print the session's runbook (Markdown) to stdout
  export <id> -o <file>      write the runbook to a file (--html for a
                             printable, self-contained HTML page)

Flags:
  -dir <path>   sessions directory (default: $CHIMERA_SESSIONS_DIR, else
                ~/.local/share/chimera/sessions)
  -tag <name>   list/search: only sessions carrying this tag (see
                chimera-spin -tag)

An id is the transcript filename without .jsonl — ` + "`list`" + ` prints it.
`

func runSessions(args []string) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Print(sessionsUsage)
			return
		}
	}
	if len(args) == 0 || args[0] == "help" {
		fmt.Print(sessionsUsage)
		return
	}

	// Flags are scanned manually: Go's flag package stops at the first
	// positional argument, and the documented forms put flags after the
	// subcommand (`list --tag research`, `export <id> --html -o f.html`).
	tail, err := parseTail(args)
	if err != nil {
		failSessions(err.Error())
	}
	if len(tail.positional) == 0 {
		fmt.Print(sessionsUsage)
		return
	}
	sub, rest := tail.positional[0], tail.positional[1:]

	switch sub {
	case "list":
		sessionsList(tail.dir, tail.tag)
	case "search":
		query := strings.Join(rest, " ")
		if strings.TrimSpace(query) == "" {
			failSessions(`search needs a query, e.g. chimera sessions search "webapi.Register"`)
		}
		sessionsSearch(tail.dir, query, tail.tag)
	case "show":
		if len(rest) == 0 {
			failSessions("show needs a session id (see `chimera sessions list`)")
		}
		md, err := sessions.RenderMarkdown(filepath.Join(tail.dir, rest[0]+".jsonl"))
		if err != nil {
			failSessions(err.Error())
		}
		fmt.Print(md)
	case "export":
		if len(rest) == 0 {
			failSessions("export needs a session id")
		}
		id := rest[0]
		out := tail.out
		if out == "" {
			if tail.html {
				out = id + ".html"
			} else {
				out = id + ".md"
			}
		}
		path := filepath.Join(tail.dir, id+".jsonl")
		var doc string
		if tail.html {
			doc, err = sessions.RenderHTML(path)
		} else {
			doc, err = sessions.RenderMarkdown(path)
		}
		if err != nil {
			failSessions(err.Error())
		}
		if err := os.WriteFile(out, []byte(doc), 0o644); err != nil {
			failSessions(err.Error())
		}
		fmt.Printf("wrote %s\n", out)
	default:
		failSessions(fmt.Sprintf("unknown sessions command %q", sub))
	}
}

// tailOpts is the result of scanning `chimera sessions` arguments: the
// positional arguments in order (subcommand first), plus any flags, wherever
// they appeared.
type tailOpts struct {
	positional []string
	dir        string
	tag        string
	out        string
	html       bool
}

// parseTail scans sessions arguments in one pass, keeping positionals in
// order and collecting flags around them. An unrecognized dashed argument is
// an error, not something to silently swallow.
func parseTail(args []string) (*tailOpts, error) {
	o := &tailOpts{dir: spin.DefaultSessionsDir()}
	value := func(i int, name string) (string, error) {
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s needs a value (e.g. %s <value>)", name, name)
		}
		return args[i+1], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-dir", "--dir":
			v, err := value(i, "-dir")
			if err != nil {
				return nil, err
			}
			o.dir, i = v, i+1
		case "-tag", "--tag":
			v, err := value(i, "-tag")
			if err != nil {
				return nil, err
			}
			o.tag, i = v, i+1
		case "-o", "--output":
			v, err := value(i, "-o")
			if err != nil {
				return nil, err
			}
			o.out, i = v, i+1
		case "-html", "--html":
			o.html = true
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return nil, fmt.Errorf("unknown flag %q", a)
			}
			o.positional = append(o.positional, a)
		}
	}
	return o, nil
}

func sessionsList(dir, tag string) {
	sums, err := sessions.List(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no sessions in %s (%v)\n", dir, err)
		return
	}
	if tag != "" {
		var tagged []*sessions.Summary
		for _, s := range sums {
			if sessions.HasTag(s.Tags, tag) {
				tagged = append(tagged, s)
			}
		}
		sums = tagged
	}
	if len(sums) == 0 {
		if tag != "" {
			fmt.Printf("no sessions tagged %q in %s\n(dir is set with -dir or $CHIMERA_SESSIONS_DIR)\n", tag, dir)
		} else {
			fmt.Printf("no sessions recorded yet in %s\n(dir is set with -dir or $CHIMERA_SESSIONS_DIR)\n", dir)
		}
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "STARTED\tID\tROUNDS\tTOOLS\tDURATION\tTASK\n")
	for _, s := range sums {
		task := s.Task
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n",
			s.Started.Format("2006-01-02 15:04"),
			s.ID, s.Rounds, s.ToolCalls,
			s.Duration.Round(time.Second),
			task)
	}
	tw.Flush()
}

// sessionsSearch prints one line per hit —
//
//	<id>  r<round>  <kind>  <trimmed matching line>
//
// — newest session first, capped at the package's SearchHitCap, with the
// uncapped total at the end.
func sessionsSearch(dir, query, tag string) {
	res, err := sessions.Search(dir, query, tag)
	if err != nil {
		failSessions(err.Error())
	}
	if res.Total == 0 {
		if tag != "" {
			fmt.Printf("no matches for %q in sessions tagged %q (%s)\n", query, tag, dir)
		} else {
			fmt.Printf("no matches for %q in %s\n", query, dir)
		}
		return
	}
	for _, h := range res.Hits {
		fmt.Printf("%s  r%d  %s  %s\n", h.Session, h.Round, h.Kind, truncateRunes(h.Line, 160))
	}
	if res.Total > len(res.Hits) {
		fmt.Printf("\n%d matches (showing first %d)\n", res.Total, len(res.Hits))
	} else {
		fmt.Printf("\n%d matches\n", res.Total)
	}
}

// truncateRunes shortens s to at most max runes, marking the cut.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " …"
}

func failSessions(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
