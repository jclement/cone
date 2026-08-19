// Command cone is the agent coordination board.
//
//	The Cone of Silence never actually worked either.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jclement/cone/internal/board"
	"github.com/jclement/cone/internal/inbox"
	"github.com/jclement/cone/internal/install"
	"github.com/jclement/cone/internal/mcpsrv"
	"github.com/jclement/cone/internal/tui"
	"github.com/jclement/cone/internal/watch"
)

var version = "dev"

const usage = `cone — the agent coordination board

  One file per thing; the directory is the state. Claiming is an atomic rename, so
  any agent can take part with nothing but mv and cat.

TASKS
  cone new <title>            file a task into inbox
  cone ls [state|all]         list tasks (default: ready)
  cone show <id>              print a task
  cone ready <id>             inbox -> ready (triaged, claimable)
  cone claim <id> [-as name]  ready -> doing. ATOMIC: exactly one agent wins
  cone done <id>              doing -> done
  cone block <id> [why]       doing -> blocked
  cone back <id>              release a claim, back to ready
  cone stale [-h hours]       claims older than N hours (reports only)

MESSAGES
  cone post <topic> <text>    leave something for other agents
  cone read [-n 10]           recent messages

FIND
  cone search <query>         full-text over tasks and messages
  cone reindex                rebuild the index from disk

RUN
  cone tui                    interactive board
  cone watch                  the heartbeat: wake an idle orchestrator when work appears
  cone sync                   pull tasks from configured inboxes
  cone mcp                    stdio MCP server (for claude mcp add)
  cone serve [-addr :7788]    HTTP MCP server (for remote agents)
  cone install                install skills + the platform scheduler
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
		fmt.Println("cone " + version)
		return nil
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
	case "stale":
		return cmdStale(rest)
	case "post":
		return cmdPost(rest)
	case "read":
		return cmdRead(rest)
	case "search":
		return cmdSearch(rest)
	case "reindex":
		return cmdReindex()
	case "sync":
		return cmdSync()
	case "watch":
		return cmdWatch(rest)
	case "mcp":
		return cmdMCP()
	case "serve":
		return cmdServe(rest)
	case "install":
		return install.Run(rest)
	case "tui":
		return runTUI()
	default:
		return fmt.Errorf("unknown command %q (try: cone help)", cmd)
	}
}

func runTUI() error {
	b, err := open()
	if err != nil {
		return err
	}
	return tui.Run(b)
}

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	repo := fs.String("repo", "", "repo this belongs to")
	kind := fs.String("kind", "investigate", "investigate|implement|review|chore")
	prio := fs.String("priority", "normal", "low|normal|high")
	auto := fs.Bool("auto", false, "pre-authorise starting without asking")
	ready := fs.Bool("ready", false, "skip triage and file straight into ready")
	fs.Parse(args)
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
		want = []board.State{board.State(args[0])}
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
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", t.ID, t.Priority, t.Repo, t.ClaimedBy, age)
			total++
		}
	}
	if total == 0 {
		fmt.Println("nothing here")
		return nil
	}
	return tw.Flush()
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

func cmdClaim(args []string) error {
	fs := flag.NewFlagSet("claim", flag.ExitOnError)
	as := fs.String("as", "", "claim as this agent name")
	fs.Parse(args)
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
	return nil
}

func cmdTransition(args []string, to string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cone %s <id>", to)
	}
	b, err := open()
	if err != nil {
		return err
	}
	var t *board.Task
	switch to {
	case "ready":
		t, err = b.Promote(args[0])
	case "done":
		t, err = b.Complete(args[0])
	case "back":
		t, err = b.Release(args[0])
	}
	if err != nil {
		return err
	}
	fmt.Println(t.Path)
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
		"post the actual question through the ask-the-human service")
	return nil
}

func cmdStale(args []string) error {
	fs := flag.NewFlagSet("stale", flag.ExitOnError)
	hours := fs.Int("h", 8, "claims older than this many hours")
	fs.Parse(args)
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
	fs.Parse(args)
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
	sources := inbox.FromEnv()
	if len(sources) == 0 {
		fmt.Println("no inboxes configured on this host (nothing to do)")
		return nil
	}
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = s.Name()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	filed, err := inbox.Sync(ctx, b, sources)
	for _, t := range filed {
		fmt.Printf("filed  %s  (from %s)\n", t.ID, t.Source)
	}
	if len(filed) == 0 && err == nil {
		fmt.Printf("nothing new from %s\n", strings.Join(names, ", "))
	}
	if err != nil {
		return err
	}
	if len(filed) > 0 {
		_, _ = b.Reindex()
	}
	return nil
}

func cmdWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	interval := fs.Duration("interval", 30*time.Second, "how often to check")
	max := fs.Int("max-workers", 4, "concurrent worker cap")
	verbose := fs.Bool("verbose", false, "log every decision")
	dry := fs.Bool("dry-run", false, "say what it would do; poke nothing")
	once := fs.Bool("once", false, "run one cycle and exit")
	fs.Parse(args)

	b, err := open()
	if err != nil {
		return err
	}
	w := watch.New(b, watch.Options{
		Interval: *interval, MaxWorkers: *max, Verbose: *verbose, DryRun: *dry,
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

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "listen address")
	token := fs.String("token", os.Getenv("CONE_TOKEN"), "bearer token required from clients")
	fs.Parse(args)
	b, err := open()
	if err != nil {
		return err
	}
	return mcpsrv.ServeHTTP(b, *addr, *token)
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
