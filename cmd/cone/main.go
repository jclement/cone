// Command cone is the agent coordination board.
//
//	The Cone of Silence never actually worked either.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jclement/cone/internal/board"
	"github.com/jclement/cone/internal/human"
	"github.com/jclement/cone/internal/inbox"
	"github.com/jclement/cone/internal/install"
	"github.com/jclement/cone/internal/mcpsrv"
	"github.com/jclement/cone/internal/selfupdate"
	"github.com/jclement/cone/internal/tui"
	"github.com/jclement/cone/internal/watch"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

const usage = `cone — the agent coordination board

  One file per thing; the directory is the state. Claiming is an atomic rename, so
  any agent can take part with nothing but mv and cat.

TASKS
  cone new <title>            file a task into inbox
  cone ls [state|all]         list tasks (default: ready)
  cone show <id>              print a task
  cone ready <id>             inbox -> ready (triaged, claimable)
  cone claim <id> [-as name]  ready -> doing. ATOMIC: exactly one agent wins
  cone done <id> [--result …] doing -> done. An investigation needs a result
  cone block <id> [why]       doing -> blocked
  cone back <id>              release a claim, back to ready
  cone ask <id> [flags]       post a question to the human service and block on the answer
  cone note <id> <text>       record a finding without changing state
  cone set <id> <key> <val>   worktree | agent | branch | repo | priority | kind | question
  cone stale [-h hours]       claims older than N hours (reports only)
  cone reap [--dry-run]       release claims held by agents herdr has lost

MESSAGES
  cone post <topic> <text>    leave something for other agents
  cone read [-n 10]           recent messages

FIND
  cone search <query>         full-text over tasks and messages
  cone reindex                rebuild the index from disk
  cone doctor                 why did nothing happen? Checks the board and the heartbeat

RUN
  cone tui                    interactive board
  cone watch                  the heartbeat: wake an idle orchestrator when work appears
  cone sync                   pull tasks from configured inboxes
  cone mcp                    stdio MCP server (for claude mcp add)
  cone install                install skills + the platform scheduler
  cone update [--check]       verified in-place upgrade to the latest release
  cone version

  Board root: $CONE_HOME, else ~/cone
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cone: "+err.Error())
		os.Exit(1)
	}
}

func open() (*board.Board, error) { return board.Open(os.Getenv("CONE_HOME")) }

// who identifies the acting agent. Herdr exports HERDR_AGENT into every pane it owns, which
// makes claims self-labelling with no configuration.
func who(override string) string {
	if override != "" {
		return override
	}
	for _, k := range []string{"CONE_AGENT", "HERDR_AGENT"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	h, _ := os.Hostname()
	return h
}

func run(args []string) error {
	if len(args) == 0 {
		return runTUI()
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version", "--version":
		return cmdVersion()
	case "new":
		return cmdNew(rest)
	case "ls", "list":
		return cmdList(rest)
	case "show", "cat":
		return cmdShow(rest)
	case "ready", "promote":
		return cmdTransition(rest, "ready")
	case "claim":
		return cmdClaim(rest)
	case "done", "complete":
		return cmdTransition(rest, "done")
	case "block":
		return cmdBlock(rest)
	case "back", "release":
		return cmdTransition(rest, "back")
	case "ask":
		return cmdAsk(rest)
	case "note":
		return cmdNote(rest)
	case "set":
		return cmdSet(rest)
	case "reap":
		return cmdReap(rest)
	case "stale":
		return cmdStale(rest)
	case "post":
		return cmdPost(rest)
	case "read":
		return cmdRead(rest)
	case "search":
		return cmdSearch(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "reindex":
		return cmdReindex()
	case "sync":
		return cmdSync()
	case "watch":
		return cmdWatch(rest)
	case "mcp":
		return cmdMCP()
	case "install":
		return install.Run(rest)
	case "update":
		return selfupdate.Run(rest, version)
	case "tui":
		return runTUI()
	default:
		return fmt.Errorf("unknown command %q (try: cone help)", cmd)
	}
}

// reorder pulls flags out of a free-text argument list so they can be written anywhere.
//
// Go's flag package stops parsing at the first positional. That is right for git-shaped
// commands and wrong for every command here that takes a sentence: `cone new "why is the
// solver slow" -kind investigate` filed a task actually titled "why is the solver slow -kind
// investigate", with the kind silently ignored — the same trap that made `cone search foo -n 2`
// an FTS5 syntax error. The FlagSet is consulted for what is a flag and whether it takes a
// value, so a word that merely starts with a dash is never eaten.
func reorder(fs *flag.FlagSet, args []string) []string {
	known, isBool := map[string]bool{}, map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		known[f.Name] = true
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			isBool[f.Name] = true
		}
	})

	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // the explicit end of flags; everything after is text
			rest = append(rest, args[i+1:]...)
			break
		}

		if !strings.HasPrefix(a, "-") || len(a) < 2 {
			rest = append(rest, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if k, _, ok := strings.Cut(name, "="); ok {
			if known[k] {
				flags = append(flags, a)
			} else {
				rest = append(rest, a)
			}
			continue
		}
		if !known[name] {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		if !isBool[name] && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	// The separator is always emitted: with the flags hoisted to the front, a positional that
	// itself begins with a dash ("-1 was the culprit") would otherwise be re-parsed as one.
	return append(append(flags, "--"), rest...)
}

// newUsage is what `cone new --help` prints. Answering that guess matters more here than in a
// normal CLI: reorder deliberately treats an unknown dash word as title text, so until this
// existed `cone new --help` filed a task actually titled "help", silently. The board is the
// coordination layer — every agent on this machine files, most are guessing at the flags the
// first time, and one that accumulates "help" tasks is one people stop trusting.
const newUsage = `usage: cone new [flags] <title>

  File a task into inbox. The title is free text and the flags may sit on either side of it.
  Use -- when the title itself has to begin with a dash.

`

// helpOrUnknownFlag inspects the leading-dash arguments of a board-writing command before
// reorder folds the unrecognised ones into the title: it reports a request for help, or the
// first argument that is neither a known flag nor deliberate text.
//
// It walks args exactly as reorder does — same known/bool lookup, same "the word after a
// non-bool flag is its value" step — because a gate that disagreed with the parser would
// reject arguments the parser was about to accept. Everything after -- is title text and is
// not inspected, which is what keeps a title legitimately able to begin with a dash.
//
// `new` and `ask` use this. The permissiveness is right for the commands that merely read or
// annotate ("-1 was the culprit" is a finding, not a flag); it is wrong for a command whose
// mistyped argument becomes a new row on the board — or, for `ask`, a question posted to a
// human's phone about a task id spelled "--help".
func helpOrUnknownFlag(fs *flag.FlagSet, args []string) (help bool, err error) {
	known, isBool := map[string]bool{}, map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		known[f.Name] = true
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			isBool[f.Name] = true
		}
	})

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return false, nil
		}
		if !strings.HasPrefix(a, "-") || len(a) < 2 {
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case name == "h" || name == "help":
			return true, nil
		case !known[name]:
			return false, fmt.Errorf("unknown flag %q — see cone %s --help, or put it after -- to take it literally", a, fs.Name())
		case !hasValue && !isBool[name] && i+1 < len(args):
			i++
		}
	}
	return false, nil
}

func runTUI() error {
	upd := selfupdate.Start(version)
	b, err := open()
	if err != nil {
		return err
	}
	return tui.Run(b, upd)
}

// updateNotice is how long `cone version` will wait on the background check. GitHub answers
// in a fraction of this from a warm network; anything slower is dropped, because a version
// command that sits there is worse than one that says nothing about updates.
const updateNotice = 2 * time.Second

func cmdVersion() error {
	upd := selfupdate.Start(version)
	fmt.Printf("cone %s (%s, built %s)\n", version, commit, buildDate)
	if rel := upd.Wait(updateNotice); rel != nil {
		fmt.Printf("%s available — run: cone update\n", rel.Tag)
	}
	return nil
}

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	repo := fs.String("repo", "", "repo this belongs to")
	kind := fs.String("kind", "investigate", "investigate|implement|review|chore")
	prio := fs.String("priority", "normal", "low|normal|high")
	auto := fs.Bool("auto", false, "pre-authorise starting without asking")
	ready := fs.Bool("ready", false, "skip triage and file straight into ready")
	switch help, err := helpOrUnknownFlag(fs, args); {
	case err != nil:
		return err
	case help:
		fs.SetOutput(os.Stdout)
		fmt.Print(newUsage)
		fs.PrintDefaults()
		return nil
	}
	fs.Parse(reorder(fs, args))
	if fs.NArg() == 0 {
		return errors.New("usage: cone new [flags] <title>")
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.New(board.Task{
		Title: strings.Join(fs.Args(), " "), Repo: *repo,
		Kind: *kind, Priority: *prio, Auto: *auto,
	})
	if err != nil {
		return err
	}
	if *ready {
		if t, err = b.Promote(t.ID); err != nil {
			return err
		}
	}
	fmt.Println(t.Path)
	return nil
}

func cmdList(args []string) error {
	b, err := open()
	if err != nil {
		return err
	}
	want := board.States
	if len(args) > 0 && args[0] != "all" {
		// A misspelt state listed an empty directory and printed "nothing here", which is
		// indistinguishable from a genuinely empty queue — the worst possible answer to
		// "what is waiting?".
		st := board.State(args[0])
		if !validState(st) {
			return fmt.Errorf("no such state %q (%s, or all)", args[0], strings.Join(stateNames(), ", "))
		}
		want = []board.State{st}
	} else if len(args) == 0 {
		want = []board.State{board.Ready}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	total := 0
	for _, s := range want {
		tasks, err := b.List(s)
		if err != nil || len(tasks) == 0 {
			continue
		}
		fmt.Fprintf(tw, "\n%s (%d)\t\t\t\n", strings.ToUpper(string(s)), len(tasks))
		for _, t := range tasks {
			age := ""
			if !t.ClaimedAt.IsZero() {
				age = shortDur(time.Since(t.ClaimedAt))
			}
			auto := ""
			if t.Auto {
				// Rendered only when true, so it reads as the exception it is.
				auto = "AUTO"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Priority, t.Repo, t.Kind, auto, t.ClaimedBy, age, t.Title)
			total++
		}
	}
	if total == 0 {
		if len(want) == 1 {
			fmt.Printf("%s is empty\n", want[0])
		} else {
			fmt.Println("the board is empty")
		}
		return nil
	}
	return tw.Flush()
}

func validState(s board.State) bool {
	for _, x := range board.States {
		if x == s {
			return true
		}
	}
	return false
}

// stateNames renders states for a message; with no arguments it names every state.
func stateNames(states ...board.State) []string {
	if len(states) == 0 {
		states = board.States
	}
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

func cmdShow(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cone show <id>")
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Find(args[0])
	if err != nil {
		return err
	}
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return err
	}
	fmt.Print(string(data))
	return nil
}

// claimNotice pairs the one board-specific instruction with the invariant every caller states.
const claimNotice = `Copy this task's body into your worker's brief verbatim — a brief that only cites a task id gets compacted into nothing.

` + board.ClaimNotice

func cmdClaim(args []string) error {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	as := fs.String("as", "", "claim as this agent name")
	fs.Parse(reorder(fs, args))
	if fs.NArg() == 0 {
		return errors.New("usage: cone claim [-as name] <id>")
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Claim(fs.Arg(0), who(*as))
	if err != nil {
		// Losing a race is ordinary, not exceptional — say so plainly so an agent picks
		// something else instead of retrying.
		if errors.Is(err, board.ErrLostRace) {
			return fmt.Errorf("%v — pick another task", err)
		}
		return err
	}
	fmt.Println(t.Path)
	// stderr, so `cone claim` stays pipeable — the path on stdout is the contract.
	fmt.Fprintln(os.Stderr, "\n"+claimNotice)
	return nil
}

func cmdTransition(args []string, to string) error {
	fs := flag.NewFlagSet(to, flag.ExitOnError)
	result := fs.String("result", "", "what you found (done; required for an investigation)")
	resultFile := fs.String("result-file", "", "read the result from a file, or - for stdin")
	fs.Parse(reorder(fs, args))
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: cone %s <id>", to)
	}
	id := fs.Arg(0)

	if *resultFile != "" {
		var data []byte
		var err error
		if *resultFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*resultFile)
		}
		if err != nil {
			return err
		}
		*result = string(data)
	}

	b, err := open()
	if err != nil {
		return err
	}
	var t *board.Task
	switch to {
	case "ready":
		t, err = b.Promote(id)
	case "done":
		t, err = b.CompleteWith(id, *result)
	case "back":
		t, err = b.Release(id)
	}
	if err != nil {
		return err
	}
	fmt.Println(t.Path)
	return nil
}

// cmdNote appends a finding to a task without changing its state. A worker that learns
// something at hour one should not have to survive to hour six for it to be findable.
func cmdNote(args []string) error {
	fs := flag.NewFlagSet("note", flag.ExitOnError)
	heading := fs.String("heading", "Note", "section heading")
	fs.Parse(reorder(fs, args))
	if fs.NArg() < 1 {
		return errors.New("usage: cone note <id> <text>   (or: … <id> - to read stdin)")
	}
	text := strings.Join(fs.Args()[1:], " ")
	if text == "-" || text == "" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(data)
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Note(fs.Arg(0), *heading, text)
	if err != nil {
		return err
	}
	fmt.Println(t.Path)
	return nil
}

// cmdSet closes the task -> worker -> worktree triangle, so any vertex finds the others and an
// abandoned checkout is distinguishable from debris.
func cmdSet(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: cone set <id> <worktree|agent|branch|repo|priority|kind|question> <value>")
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Set(args[0], args[1], strings.Join(args[2:], " "))
	if err != nil {
		return err
	}
	fmt.Println(t.Path)
	return nil
}

// cmdReap releases claims held by agents herdr no longer knows about. The worker cap counts
// tasks in doing/, so dead claims do not merely clutter — four of them stop the heartbeat.
func cmdReap(args []string) error {
	fs := flag.NewFlagSet("reap", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "say what would be released; change nothing")
	herdrBin := fs.String("herdr", os.Getenv("CONE_HERDR"), "path to the herdr binary")
	fs.Parse(reorder(fs, args))
	b, err := open()
	if err != nil {
		return err
	}
	reaped, err := b.Reap(*herdrBin, *dry)
	if err != nil {
		return fmt.Errorf("%w — refusing to release anything while herdr cannot be asked "+
			"who is alive", err)
	}
	if len(reaped) == 0 {
		fmt.Println("no claims held by dead agents")
		return nil
	}
	for _, t := range reaped {
		verb := "released"
		if *dry {
			verb = "would release"
		}
		fmt.Printf("%s %s (worker %s is gone) → %s\n", verb, t.ID, t.Agent, t.State)
	}
	return nil
}

func cmdBlock(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: cone block <id> [why]")
	}
	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Block(args[0], strings.Join(args[1:], " "))
	if err != nil {
		return err
	}
	fmt.Println(t.Path)
	fmt.Fprintln(os.Stderr, "note: blocked/ is a filing cabinet, not a notification — "+
		"`cone ask` posts the actual question, and the heartbeat delivers the answer")
	return nil
}

// askUsage is what `cone ask --help` prints. The phone-answerable rules live here and in the
// board's AGENTS.md — guidance for the asker, deliberately not enforced in code.
const askUsage = `usage: cone ask <task-id> --title <t> (--body <md> | --body-file <path>) [flags]

  Post a question about a task to the configured human service, record the question id on the
  task, and move the task to blocked/. From there the heartbeat owns the answer: when the human
  answers, the task returns to ready/ with the answer noted verbatim, and the next lead is
  offered it. --wait blocks here until the question settles instead.

  The human answers from a phone and sees only the title, the body and the options. So: paste
  the verbatim diff or error, not a description of it; give every option its consequence, not
  just a label; say which option you recommend (--recommend, required for confirm and choice)
  and put the strongest argument against it in the body. Most questions are not questions —
  anything inferable from the ticket, the code, or the conventions, you decide.

  The service is declared in ~/.config/cone/human.json ($CONE_HUMAN overrides); no file means
  no human service on this host.

`

// optionList collects repeated --option flags, each KEY:LABEL[:DETAIL].
type optionList []human.Option

func (o *optionList) String() string {
	parts := make([]string, len(*o))
	for i, opt := range *o {
		parts[i] = opt.Key
	}
	return strings.Join(parts, ",")
}

func (o *optionList) Set(v string) error {
	parts := strings.SplitN(v, ":", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("an option is KEY:LABEL or KEY:LABEL:DETAIL (got %q)", v)
	}
	opt := human.Option{Key: parts[0], Label: parts[1]}
	if len(parts) == 3 {
		opt.Detail = parts[2]
	}
	*o = append(*o, opt)
	return nil
}

// cmdAsk is the one verb an agent blocked on a human needs. Durability first: the question id
// lands on the task before anything else is reported, so even a crashed --wait leaves a task
// the heartbeat's sweep can finish the job on.
func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	title := fs.String("title", "", "one line the human sees first (required)")
	bodyFlag := fs.String("body", "", "the question, markdown")
	bodyFile := fs.String("body-file", "", "read the body from a file, or - for stdin")
	kind := fs.String("kind", human.KindManual, "confirm|choice|ack|manual (confirm and choice require --recommend)")
	var options optionList
	fs.Var(&options, "option", "KEY:LABEL[:DETAIL], repeatable — the choices offered")
	recommend := fs.String("recommend", "", "the option key you would pick")
	urgency := fs.String("urgency", "normal", "low|normal|blocking")
	expires := fs.Int("expires-hours", 0, "let the question expire after this many hours")
	wait := fs.Bool("wait", false, "block until the question settles instead of leaving it to the heartbeat")
	fs.Usage = func() {
		fmt.Print(askUsage)
		fs.SetOutput(os.Stdout)
		fs.PrintDefaults()
	}
	switch help, err := helpOrUnknownFlag(fs, args); {
	case err != nil:
		return err
	case help:
		fs.Usage()
		return nil
	}
	fs.Parse(reorder(fs, args))
	if fs.NArg() == 0 {
		return errors.New("usage: cone ask <task-id> --title <t> (--body <md> | --body-file <path>)  (--help for the rest)")
	}

	body := *bodyFlag
	if *bodyFile != "" {
		var data []byte
		var err error
		if *bodyFile == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(*bodyFile)
		}
		if err != nil {
			return err
		}
		body = string(data)
	}

	b, err := open()
	if err != nil {
		return err
	}
	t, err := b.Find(fs.Arg(0))
	if err != nil {
		return err
	}
	svc, err := human.Configured()
	if err != nil {
		return err
	}
	if svc == nil {
		return fmt.Errorf("no human service on this host — declare one in %s (name, url, and a token source; see the README's \"The human loop\")", human.ConfigPath())
	}

	// context is what lets the human's phone show where the question came from, and what lets
	// anything downstream find its way back to the board.
	qctx := map[string]string{"task": t.ID, "board": b.Root}
	if t.Agent != "" {
		qctx["agent"] = t.Agent
	}
	if t.Repo != "" {
		qctx["repo"] = t.Repo
	}
	if t.Worktree != "" {
		qctx["worktree"] = t.Worktree
	}
	q := human.Question{
		Title: *title, BodyMD: body, Kind: *kind, Options: options,
		Recommend: *recommend, Context: qctx, Urgency: *urgency, ExpiresInHours: *expires,
	}
	ctx := context.Background()
	asked, err := svc.Ask(ctx, q)
	if err != nil {
		return err
	}

	// The question exists remotely from here on, so every failure below must still name it.
	if _, err := b.Set(t.ID, "question", asked.ID); err != nil {
		return fmt.Errorf("question %s was posted (%s) but could not be recorded: %w — run: cone set %s question %s",
			asked.ID, asked.URL, err, t.ID, asked.ID)
	}
	if _, err := b.Note(t.ID, "Asked", fmt.Sprintf("%q posted to %s — %s", q.Title, svc.Name, asked.URL)); err != nil {
		return fmt.Errorf("question %s is recorded but its note failed: %w", asked.ID, err)
	}
	if t.State != board.Blocked {
		if _, err := b.Block(t.ID, ""); err != nil {
			return fmt.Errorf("question %s is recorded but the task could not be blocked: %w", asked.ID, err)
		}
	}
	fmt.Printf("%s %s\n", asked.ID, asked.URL)
	if !*wait {
		fmt.Fprintln(os.Stderr, "the heartbeat owns the answer from here — the task returns to ready/ when the human answers")
		return nil
	}
	return awaitAnswer(ctx, b, svc, t.ID, asked.ID)
}

// awaitAnswer long-polls until the question settles. Interrupting it loses nothing: the task
// is blocked with its question recorded, and the watch sweep produces the same end state.
func awaitAnswer(ctx context.Context, b *board.Board, svc *human.Service, taskID, questionID string) error {
	fmt.Fprintln(os.Stderr, "waiting — ctrl-c is safe; the heartbeat will deliver the answer instead")
	failures := 0
	for {
		ans, err := svc.Question(ctx, questionID, 90*time.Second)
		if err != nil {
			failures++
			if failures >= 5 {
				return fmt.Errorf("cannot reach %s (%v) — giving up on waiting; the task stays blocked and the heartbeat will deliver the answer", svc.Name, err)
			}
			time.Sleep(10 * time.Second)
			continue
		}
		failures = 0
		switch ans.Status {
		case human.StatusOpen:
			// A conforming server held the request for the full wait; one that answered
			// immediately would otherwise make this a tight loop.
			time.Sleep(2 * time.Second)
		case human.StatusAnswered:
			t, err := human.Answered(b, taskID, questionID, ans)
			if err != nil {
				return fmt.Errorf("answered, but the answer could not be applied: %w", err)
			}
			if ans.Value != "" {
				fmt.Println(ans.Value)
			}
			if ans.Note != "" {
				fmt.Println(ans.Note)
			}
			fmt.Fprintf(os.Stderr, "answer noted on %s\n", t.Path)
			return nil
		default:
			// Expired or cancelled. Loudly noted, and the task deliberately STAYS blocked:
			// the asker is right here to be told, and must not read silence as consent. The
			// watch sweep will re-offer it for triage on its next pass.
			if _, err := b.Note(taskID, "No answer", human.UnansweredNote(questionID, ans.Status)); err != nil {
				return fmt.Errorf("question %s %s, and noting that failed: %w", questionID, ans.Status, err)
			}
			return fmt.Errorf("question %s %s without an answer — expiry is not consent; the task stays blocked", questionID, ans.Status)
		}
	}
}

func cmdStale(args []string) error {
	fs := flag.NewFlagSet("stale", flag.ExitOnError)
	hours := fs.Int("h", 8, "claims older than this many hours")
	fs.Parse(reorder(fs, args))
	b, err := open()
	if err != nil {
		return err
	}
	tasks, err := b.Stale(time.Duration(*hours) * time.Hour)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		fmt.Println("no stale claims")
		return nil
	}
	for _, t := range tasks {
		fmt.Printf("  %-44s %-14s claimed %s ago\n", t.ID, t.ClaimedBy, shortDur(time.Since(t.ClaimedAt)))
	}
	fmt.Fprintln(os.Stderr, "\nReports only. Whether a claim is abandoned or merely slow is a "+
		"judgement call — 'cone back <id>' releases one.")
	return nil
}

func cmdPost(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: cone post <topic> <text>")
	}
	b, err := open()
	if err != nil {
		return err
	}
	m, err := b.Post(who(""), args[0], strings.Join(args[1:], " "))
	if err != nil {
		return err
	}
	fmt.Println(m.Path)
	return nil
}

func cmdRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	n := fs.Int("n", 10, "how many messages")
	fs.Parse(reorder(fs, args))
	b, err := open()
	if err != nil {
		return err
	}
	msgs, err := b.Read(*n)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println("no messages")
		return nil
	}
	for _, m := range msgs {
		fmt.Printf("\n─── %s · %s · %s\n", m.Posted.Format("2006-01-02 15:04"), m.From, m.Topic)
		for _, line := range strings.Split(m.Body, "\n") {
			fmt.Println("    " + line)
		}
	}
	return nil
}

func cmdSearch(args []string) error {
	// The query is free text, so Go's flag package is the wrong shape here: it stops
	// parsing at the first positional, which silently swallows a trailing `-n 5` into the
	// query and produces an FTS5 syntax error that looks like the tool is broken.
	// Pull known flags out from anywhere; everything else is the query.
	limit := 20
	var words []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-n", "--limit":
			if i+1 < len(args) {
				if v, err := strconv.Atoi(args[i+1]); err == nil {
					limit, i = v, i+1
					continue
				}
			}
		default:
			if v, ok := strings.CutPrefix(args[i], "-n="); ok {
				if n, err := strconv.Atoi(v); err == nil {
					limit = n
					continue
				}
			}
			words = append(words, args[i])
		}
	}
	if len(words) == 0 {
		return errors.New("usage: cone search <query> [-n 20]")
	}
	b, err := open()
	if err != nil {
		return err
	}
	hits, err := b.Search(strings.Join(words, " "), limit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, h := range hits {
		loc := h.State
		if h.Kind == "message" {
			loc = "message"
		}
		fmt.Printf("\n%-44s %s\n  %s\n  %s\n", h.ID, loc, h.Title, strings.ReplaceAll(h.Snippet, "\n", " "))
	}
	return nil
}

// cmdDoctor answers "why did nothing happen?". Exit code 1 when something is broken, so it is
// usable from a shell — a health check nobody can script is a health check nobody runs.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	herdrBin := fs.String("herdr", os.Getenv("CONE_HERDR"), "path to the herdr binary")
	quiet := fs.Bool("quiet", false, "only show problems")
	fs.Parse(reorder(fs, args))

	b, err := open()
	if err != nil {
		return err
	}
	fmt.Printf("board: %s\n\n", b.Root)

	findings := append(b.Doctor(*herdrBin), install.Doctor(b.Root)...)
	findings = append(findings, human.Doctor(b)...)
	worst := board.OK
	area := ""
	for _, f := range findings {
		if f.Severity > worst {
			worst = f.Severity
		}
		if *quiet && f.Severity == board.OK {
			continue
		}
		if f.Area != area {
			fmt.Printf("%s\n", f.Area)
			area = f.Area
		}
		fmt.Printf("  %s %s\n", f.Severity.Mark(), f.Message)
		if f.Fix != "" {
			fmt.Printf("      → %s\n", f.Fix)
		}
	}
	switch worst {
	case board.OK:
		fmt.Println("\nthe board is healthy")
	case board.Warn:
		fmt.Println("\nnothing broken, but see the warnings above")
	default:
		return errors.New("the board has problems — see above")
	}
	return nil
}

func cmdReindex() error {
	b, err := open()
	if err != nil {
		return err
	}
	n, err := b.Reindex()
	if err != nil {
		return err
	}
	fmt.Printf("indexed %d documents into %s\n", n, b.IndexPath())
	return nil
}

func cmdSync() error {
	b, err := open()
	if err != nil {
		return err
	}
	filed, names, err := inbox.SyncConfigured(context.Background(), b)
	if err != nil && len(names) == 0 {
		return err
	}
	if len(names) == 0 {
		fmt.Printf("no inboxes configured (declare them in %s)\n", inbox.ConfigPath())
		return nil
	}
	for _, t := range filed {
		fmt.Printf("filed  %s  (from %s)\n", t.ID, t.Source)
	}
	if len(filed) == 0 && err == nil {
		fmt.Printf("nothing new from %s\n", strings.Join(names, ", "))
	}
	return err
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := fs.Duration("interval", 30*time.Second, "how often to check")
	max := fs.Int("max-workers", 4, "concurrent worker cap")
	verbose := fs.Bool("verbose", false, "log every decision")
	dry := fs.Bool("dry-run", false, "say what it would do; poke nothing")
	once := fs.Bool("once", false, "run one cycle and exit")
	// The installed unit passes an absolute path: launchd's PATH does not include Homebrew,
	// so a bare "herdr" resolves to nothing and the heartbeat fails silently forever.
	herdrBin := fs.String("herdr", os.Getenv("CONE_HERDR"), "path to the herdr binary")
	noReap := fs.Bool("no-reap", false, "leave claims held by agents herdr no longer knows about")
	fs.Parse(reorder(fs, args))

	b, err := open()
	if err != nil {
		return err
	}
	w := watch.New(b, watch.Options{
		Interval: *interval, MaxWorkers: *max, Verbose: *verbose, DryRun: *dry,
		HerdrBin: *herdrBin, NoReap: *noReap,
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *once {
		w.Tick(ctx)
		return nil
	}
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func cmdMCP() error {
	b, err := open()
	if err != nil {
		return err
	}
	return mcpsrv.ServeStdio(b)
}

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

var _ = filepath.Join // retained: install paths are built here in later revisions
