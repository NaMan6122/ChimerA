package main

// runSessions handles `chimera sessions <command>` — read-only access to the
// preserved transcripts. Sessions are PRESERVATION, not resumption: the
// provider side of an old conversation expires and is out of our control, so
// what we keep is a readable runbook of what happened.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/chimera/chimera/internal/sessions"
	"github.com/chimera/chimera/internal/spin"
)

const sessionsUsage = `Usage: chimera sessions <command> [flags]

Commands:
  list                  index of recorded sessions, newest first
  show <id>             print the session's runbook (Markdown) to stdout
  export <id> -o <file> write the runbook to a Markdown file

Flags (all commands):
  -dir <path>   sessions directory (default: $CHIMERA_SESSIONS_DIR, else
                ~/.local/share/chimera/sessions)

An id is the transcript filename without .jsonl — ` + "`list`" + ` prints it.
`

func runSessions(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(sessionsUsage)
		return
	}


	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	dir := fs.String("dir", spin.DefaultSessionsDir(), "sessions directory")
	if err := fs.Parse(args[1:]); err != nil {
		fmt.Fprint(os.Stderr, sessionsUsage)
		os.Exit(2)
	}

	switch args[0] {
	case "list":
		sessionsList(*dir)
	case "show":
		if fs.NArg() == 0 {
			failSessions("show needs a session id (see `chimera sessions list`)")
		}
		md, err := sessions.RenderMarkdown(filepath.Join(*dir, fs.Arg(0)+".jsonl"))
		if err != nil {
			failSessions(err.Error())
		}
		fmt.Print(md)
	case "export":
		if fs.NArg() == 0 {
			failSessions("export needs a session id")
		}
		out := fs.Arg(0) + ".md"
		// -o may appear before or after the id; scan the remaining flags.
		rest := fs.Args()
		if len(rest) > 1 && (rest[0] == "-o" || rest[0] == "--output") {
			out = rest[1]
		}
		md, err := sessions.RenderMarkdown(filepath.Join(*dir, rest[0]+".jsonl"))
		if err != nil {
			failSessions(err.Error())
		}
		if err := os.WriteFile(out, []byte(md), 0o644); err != nil {
			failSessions(err.Error())
		}
		fmt.Printf("wrote %s\n", out)
	default:
		failSessions(fmt.Sprintf("unknown sessions command %q", args[0]))
	}
}

func sessionsList(dir string) {
	sums, err := sessions.List(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no sessions in %s (%v)\n", dir, err)
		return
	}
	if len(sums) == 0 {
		fmt.Printf("no sessions recorded yet in %s\n(dir is set with -dir or $CHIMERA_SESSIONS_DIR)\n", dir)
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

func failSessions(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
