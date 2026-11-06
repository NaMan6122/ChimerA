package main

import (
	"fmt"
	"strings"
)

// A preset is a named task scaffold: the entire turn-one prompt a beginner
// would otherwise have to write themselves. A preset replaces the task
// (`-preset flashcards -root ~/papers`), and bare positional text refines it,
// appended as "Constraints: …". Each scaffold is a self-contained prompt with
// an explicit output convention — where the result goes, what structure it
// takes, and what to do when a tool call fails — and `-preset help` prints
// them verbatim, so the prompt doubles as documentation.

// preset is one entry in the -preset table.
type preset struct {
	name     string // the -preset argument
	summary  string // one-line description, shown by -preset help
	scaffold string // the literal task prompt sent to the model
}

// render returns the task string for a run: the scaffold, plus the user's
// refinement as a constraints line when one was given.
func (p preset) render(extra string) string {
	if extra = strings.TrimSpace(extra); extra == "" {
		return p.scaffold
	}
	return p.scaffold + "\n\nConstraints: " + extra
}

// presets is the table behind -preset, in the order -preset help shows it.
// The scaffolds are written to be read: a beginner learns what the agent will
// do by reading the prompt it will be given.
var presets = []preset{
	{
		name:    "summarize",
		summary: "read every text file under -root, one structured summary per file",
		scaffold: `Read every text file under the workspace root and produce one structured summary per file.

Discover the files first: list_dir the root, then list_dir each subdirectory you find the same way, skipping .git, node_modules and other dependency, build or log directories. Read each text file in full with read_file, continuing with startLine when a read reports truncation. Skip files that are not plain text, noting them in one line.

For every file you read, write its summary with write_file to:

    notes/<file>.md

where <file> is the source path with "/" replaced by "-" — internal/api/server.go becomes notes/internal-api-server.go.md. write_file creates the notes directory itself.

Every summary uses exactly this structure:

    # Summary: <source path>
    ## Overview
    what the file is, in one or two sentences
    ## Key points
    3-7 bullets, each grounded in something the file actually says
    ## Details
    names, values and relationships a reader would otherwise have to re-derive
    ## Open questions
    what the file leaves unanswered, or "none"

Tool failures are information, not a stop sign: if a list_dir or read_file fails, record the failure under Open questions for that file and continue with the rest. When every file is covered, the final answer is a short index: one line per summary written, plus what you skipped and why.`,
	},
	{
		name:    "flashcards",
		summary: "Anki-style Q/A decks from the material, one deck per file",
		scaffold: `Turn the material in this workspace into Anki-style flashcards, one deck per source file.

Discover the files first: list_dir the root, then list_dir each subdirectory you find the same way, skipping .git, node_modules and other dependency, build or log directories. Read each text file in full with read_file, continuing with startLine when a read reports truncation.

For every file, write its deck with write_file to:

    notes/<file>.flashcards.md

where <file> is the source path with "/" replaced by "-" — internal/api/server.go becomes notes/internal-api-server.go.flashcards.md. Each deck is Markdown:

    # Flashcards: <source path>

    **Q.** <one self-contained question>
    **A.** <the answer, in one or two sentences>

Write 5-15 cards per file, one fact per card: definitions, invariants, flags, measured numbers, cause and effect. Each card must make sense on its own, and answers must quote identifiers and paths exactly as the source spells them. No trick questions and no "it depends" answers — split those into separate cards.

Tool failures are information, not a stop sign: if a file cannot be read, write its deck with a single card — Q. Why is this file not covered? A. <the failure> — and continue with the rest. The final answer reports how many cards were written per file.`,
	},
	{
		name:    "eli12",
		summary: "explain each file for a bright 12-year-old",
		scaffold: `Explain every text file under the workspace root for a bright 12-year-old, one page per file.

Discover the files first: list_dir the root, then list_dir each subdirectory you find the same way, skipping .git, node_modules and other dependency, build or log directories. Read each text file in full with read_file, continuing with startLine when a read reports truncation.

For every file, write its page with write_file to:

    notes/<file>.eli12.md

where <file> is the source path with "/" replaced by "-" — internal/api/server.go becomes notes/internal-api-server.go.eli12.md. Every page uses exactly this structure:

    # <source path>, explained for a 12-year-old
    ## The big idea
    two or three sentences, no jargon
    ## How it works
    a short walkthrough built on one everyday analogy, carried all the way through
    ## Words that survived
    every technical term still used above, each defined in one line

Keep pages under about 400 words. Prefer analogies over definitions and concrete over abstract; write up to a smart kid who is new to this field, never down to a child. If a file genuinely resists simplification, say in one sentence what it does and why.

Tool failures are information, not a stop sign: if a file cannot be read, write its page anyway — the big idea becomes "I could not read this one because <the failure>" — and continue with the rest. The final answer lists the pages written.`,
	},
	{
		name:    "cite-check",
		summary: "list claims lacking a citation, with file:line, reported to stdout",
		scaffold: `Audit the text files under the workspace root for claims that lack a citation. Write no files: the report is the final answer, printed to stdout.

Discover the files first: list_dir the root, then list_dir each subdirectory you find the same way, skipping .git, node_modules and other dependency, build or log directories. Read each text file in full with read_file, continuing with startLine when a read reports truncation.

A claim is a factual statement a reader could act on: a measured number, a compatibility or support statement, a reference to a paper, specification or standard, or a "faster / safer / more reliable than" comparison. For each claim, look for evidence inside this workspace — the file itself, a sibling file, a spec, a measurement. Classify the claim:

    cited — the workspace contains the supporting evidence
    weak  — hedged or second-hand ("reportedly", "it is said") with no source
    bare  — nothing in the workspace supports it

The report is one line per finding, sorted by file and then line:

    <file>:<line> <bare|weak> "<short quote>" — <what evidence would satisfy it>

preceded by a one-line header with the totals (cited / weak / bare).

Tool failures are information, not a stop sign: if a file cannot be read, open the report with "<file>: unreadable (<error>)" and continue with the rest. If nothing is bare or weak, say so in one line instead of an empty list.`,
	},
	{
		name:    "meeting-notes",
		summary: "raw transcript -> decisions, actions, owners",
		scaffold: `Turn the raw meeting transcript in this workspace into minutes.

Find the transcript first: list_dir the root and its subdirectories, skipping .git, node_modules and other dependency, build or log directories, and pick the text file(s) that read as a transcript — speaker-labelled lines, timestamps, or an unedited conversation dump. If several qualify, produce one set of minutes per file. Read the whole transcript with read_file, continuing with startLine until you have seen every line.

For each transcript, write the minutes with write_file to:

    notes/<file>.minutes.md

where <file> is the transcript's path with "/" replaced by "-" — 2026-05-04-sync.txt becomes notes/2026-05-04-sync.txt.minutes.md. Every set of minutes uses exactly this structure:

    # Minutes: <transcript path>
    ## Attendees
    everyone who speaks; "unclear" when the transcript does not say
    ## Decisions
    one bullet per decision: what was decided, and the speaker's own words that settled it
    ## Actions
    a table: | action | owner | due | source line | — "unstated" wherever the transcript omits an owner or date
    ## Open threads
    questions raised and left unanswered

Quote decisions in the speaker's own words; never paraphrase into something stronger than was said, and never invent an owner or a date.

Tool failures are information, not a stop sign: if a transcript read fails partway, write minutes for what you did read and add a "## Coverage" section saying where you stopped and why. The final answer gives the minutes path plus counts: decisions, actions, open threads.`,
	},
}

// presetNames lists the table's names in display order.
func presetNames() []string {
	names := make([]string, 0, len(presets))
	for _, p := range presets {
		names = append(names, p.name)
	}
	return names
}

// lookupPreset finds one preset by exact name.
func lookupPreset(name string) (preset, bool) {
	for _, p := range presets {
		if p.name == name {
			return p, true
		}
	}
	return preset{}, false
}

// resolveTask is the CLI's entire task-resolution policy, split out of main so
// it can be tested without os.Exit:
//
//	-preset flashcards           -> the scaffold
//	-preset flashcards "text"    -> the scaffold + "Constraints: text"
//	-preset flashcards -task "x" -> error: the two task channels are exclusive
//	-preset help                 -> showHelp; main prints every scaffold
//	-task "x", or a bare "x"     -> the task itself, exactly as before presets
func resolveTask(preset, taskFlag, positional string) (task string, showHelp bool, err error) {
	switch {
	case preset == "":
		if taskFlag == "" {
			taskFlag = positional
		}
		return taskFlag, false, nil
	case taskFlag != "":
		return "", false, fmt.Errorf("-preset and -task are mutually exclusive; pass refinement as bare positional text instead")
	case preset == "help":
		return "", true, nil
	}
	p, ok := lookupPreset(preset)
	if !ok {
		return "", false, fmt.Errorf("unknown preset %q; known presets: %s (or -preset help)", preset, strings.Join(presetNames(), ", "))
	}
	return p.render(positional), false, nil
}

// presetHelp renders `-preset help`: every scaffold, verbatim, so a beginner
// can read exactly what the agent will be asked before running it.
func presetHelp() string {
	var b strings.Builder
	b.WriteString(`chimera-spin presets — named scaffolds for common tasks

usage:
  chimera-spin -preset <name> [-root DIR] ["refinement text"]
  chimera-spin -preset help    this list

A preset is the literal prompt sent to the model on turn one; refinement text
is appended as "Constraints: …". A preset and an explicit task are exclusive.

`)
	for _, p := range presets {
		fmt.Fprintf(&b, "%s — %s\n\n%s\n\n", p.name, p.summary, indent(p.scaffold))
	}
	return b.String()
}

// indent prefixes every non-empty line with four spaces, so a scaffold reads
// as quoted material inside the help text.
func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "    " + line
		}
	}
	return strings.Join(lines, "\n")
}
