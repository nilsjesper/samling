// Command samling shuffles an Instapaper backlog: it keeps a local mirror of
// your unread list, picks articles at random, opens them in your browser, and
// archives what you've read on the next sync.
package main

import (
	"bufio"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/nilsjesper/samling/internal/browse"
	"github.com/nilsjesper/samling/internal/config"
	"github.com/nilsjesper/samling/internal/instapaper"
	"github.com/nilsjesper/samling/internal/library"
	"github.com/nilsjesper/samling/internal/oauth1"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "login":
		err = cmdLogin(args)
	case "sync":
		err = cmdSync(args)
	case "pick":
		err = cmdPick(args, true)
	case "list":
		err = cmdPick(args, false)
	case "status":
		err = cmdStatus(args)
	case "undo":
		err = cmdUndo(args)
	case "folders":
		err = cmdFolders(args)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return
	default:
		fmt.Fprintf(os.Stderr, "samling: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "samling: "+err.Error())
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `samling - shuffle your way through an Instapaper backlog

Usage:
  samling <command> [flags]

Commands:
  login     Exchange your Instapaper username and password for an access token
  sync      Archive what you've read, then refresh the local unread mirror
  pick      Open N random unread articles and mark them read locally
  list      Same selection as pick, printed instead of opened
  status    Show unread / pending / archived counts
  undo      Put the most recent pick back (only works before the next sync)
  folders   List your Instapaper folder ids

Pick and list flags:
  -n N               How many articles (default 1)
  --folder NAME      Only articles in this folder (as stored locally)
  --domain HOST      Only articles from this host, e.g. nytimes.com
  --older-than SPAN  Only articles saved at least SPAN ago, e.g. 90d, 2w, 36h
  --newer-than SPAN  Only articles saved within the last SPAN
  --starred          Only starred articles
  --unstarred        Only unstarred articles
  --seed N           Reproducible shuffle
  --no-open          Print instead of opening (implied by 'list')

Sync flags:
  --folder NAME      Instapaper folder to mirror: unread (default), starred,
                     archive, or a numeric id from 'samling folders'
  --dry-run          Report what would happen without changing anything
  --concurrency N    Parallel archive requests (default 4)

Setup:
  1. Request an API consumer key at
     https://www.instapaper.com/developers/applications/create
  2. Put it in ~/.samling/config.json as {"consumer_key":"...","consumer_secret":"..."}
     or set INSTAPAPER_CONSUMER_KEY / INSTAPAPER_CONSUMER_SECRET
  3. samling login && samling sync && samling pick -n 3

State lives in ~/.samling (override with SAMLING_HOME).
Picking is entirely offline; only 'sync', 'login' and 'folders' touch the network.
`)
}

// --- login --------------------------------------------------------------------

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	username := fs.String("username", "", "Instapaper username or email (prompted if omitted)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		return err
	}

	user := *username
	if user == "" {
		if user, err = prompt("Instapaper username or email: "); err != nil {
			return err
		}
	}
	password, err := promptPassword("Password (leave blank if your account has none): ")
	if err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	client := instapaper.New(oauth1.Credentials{
		ConsumerKey:    cfg.ConsumerKey,
		ConsumerSecret: cfg.ConsumerSecret,
	})

	token, secret, err := client.AccessToken(ctx, user, password)
	if err != nil {
		return err
	}
	client.Creds.Token, client.Creds.TokenSecret = token, secret

	account, err := client.VerifyCredentials(ctx)
	if err != nil {
		return fmt.Errorf("got a token but could not verify it: %w", err)
	}

	if err := config.SaveToken(config.Token{
		Token:       token,
		TokenSecret: secret,
		UserID:      account.UserID,
		Username:    account.Username,
	}); err != nil {
		return err
	}

	fmt.Printf("Logged in as %s (user %d).\nToken saved to %s\nNext: samling sync\n",
		account.Username, account.UserID, config.Path("token.json"))
	return nil
}

// --- pick / list ----------------------------------------------------------------

func cmdPick(args []string, open bool) error {
	name := "pick"
	if !open {
		name = "list"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	n := fs.Int("n", 1, "how many articles")
	folder := fs.String("folder", "", "only articles in this folder")
	domain := fs.String("domain", "", "only articles from this host")
	olderThan := fs.String("older-than", "", "only articles saved at least this long ago (90d, 2w, 36h)")
	newerThan := fs.String("newer-than", "", "only articles saved within this long")
	starred := fs.Bool("starred", false, "only starred articles")
	unstarred := fs.Bool("unstarred", false, "only unstarred articles")
	seed := fs.Int64("seed", 0, "reproducible shuffle")
	noOpen := fs.Bool("no-open", false, "print instead of opening")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *starred && *unstarred {
		return errors.New("--starred and --unstarred are mutually exclusive")
	}
	if *n < 1 {
		return errors.New("-n must be at least 1")
	}

	filter := library.Filter{Folder: *folder, Domain: *domain}
	var err error
	if filter.OlderThan, err = parseSpan(*olderThan); err != nil {
		return err
	}
	if filter.NewerThan, err = parseSpan(*newerThan); err != nil {
		return err
	}
	switch {
	case *starred:
		v := true
		filter.Starred = &v
	case *unstarred:
		v := false
		filter.Starred = &v
	}

	path := library.DefaultPath()
	lib, err := library.Load(path)
	if err != nil {
		return err
	}
	if len(lib.Bookmarks) == 0 {
		return errors.New("the local library is empty: run `samling sync` first")
	}

	var seeded bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "seed" {
			seeded = true
		}
	})
	picks := lib.Pick(*n, filter, newRNG(*seed, seeded))
	if len(picks) == 0 {
		return errors.New("no unread articles match those filters")
	}

	shouldOpen := open && !*noOpen

	// Commit the state change before opening anything, so an interrupted run
	// can't serve the same article twice. `samling undo` reverses it.
	if shouldOpen {
		lib.MarkRead(picks, time.Now())
		if err := lib.Save(path); err != nil {
			return err
		}
	}

	for i, b := range picks {
		fmt.Printf("%2d. %s\n    %s\n", i+1, titleOf(b), b.URL)
		if meta := describe(b); meta != "" {
			fmt.Printf("    %s\n", meta)
		}
		if shouldOpen {
			if err := browse.Open(b.URL); err != nil {
				fmt.Fprintf(os.Stderr, "    warning: %v\n", err)
			}
		}
	}

	if shouldOpen {
		fmt.Printf("\n%s marked read locally; run `samling sync` to archive %s in Instapaper.\n",
			plural(len(picks), "article", "articles"), pronoun(len(picks)))
		fmt.Println("Changed your mind? `samling undo`")
	}
	return nil
}

// --- status ---------------------------------------------------------------------

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	top := fs.Int("domains", 5, "how many top domains to show (0 for none)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	lib, err := library.Load(library.DefaultPath())
	if err != nil {
		return err
	}
	s := lib.Stats(*top)

	fmt.Printf("%s unread\n", comma(s.Unread))
	fmt.Printf("%s pending archive\n", comma(s.Pending))
	fmt.Printf("%s archived\n", comma(s.Archived))

	if s.LastSync.IsZero() {
		fmt.Println("\nNever synced. Run `samling sync`.")
		return nil
	}
	fmt.Printf("\nLast synced %s ago\n", formatAge(time.Since(s.LastSync)))

	if s.Oldest != nil {
		fmt.Printf("Oldest unread is %s old: %s\n", formatAge(s.Oldest.Age()), titleOf(*s.Oldest))
	}
	if len(s.Domains) > 0 {
		fmt.Println("\nTop domains:")
		for _, d := range s.Domains {
			fmt.Printf("  %5s  %s\n", comma(d.Count), d.Host)
		}
	}
	return nil
}

// --- undo -----------------------------------------------------------------------

func cmdUndo(args []string) error {
	fs := flag.NewFlagSet("undo", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := library.DefaultPath()
	lib, err := library.Load(path)
	if err != nil {
		return err
	}
	restored, err := lib.Undo()
	if err != nil {
		return err
	}
	if err := lib.Save(path); err != nil {
		return err
	}
	fmt.Printf("Put %s back:\n", plural(len(restored), "article", "articles"))
	for _, b := range restored {
		fmt.Printf("  %s\n", titleOf(b))
	}
	return nil
}

// --- folders ---------------------------------------------------------------------

func cmdFolders(args []string) error {
	fs := flag.NewFlagSet("folders", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	folders, err := client.Folders(ctx)
	if err != nil {
		return err
	}
	fmt.Println("unread    (built in)")
	fmt.Println("starred   (built in)")
	fmt.Println("archive   (built in)")
	for _, f := range folders {
		fmt.Printf("%-9s %s\n", f.FolderID, f.Title)
	}
	return nil
}

// --- helpers -----------------------------------------------------------------------

func newClient() (*instapaper.Client, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return nil, err
	}
	tok, err := config.LoadToken()
	if err != nil {
		return nil, err
	}
	return instapaper.New(oauth1.Credentials{
		ConsumerKey:    cfg.ConsumerKey,
		ConsumerSecret: cfg.ConsumerSecret,
		Token:          tok.Token,
		TokenSecret:    tok.TokenSecret,
	}), nil
}

// signalContext cancels on Ctrl-C so a long sync can stop cleanly and save
// whatever progress it has made.
func signalContext() (context.Context, func()) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func newRNG(seed int64, seeded bool) *rand.Rand {
	if seeded {
		// Two distinct streams from one user-supplied number.
		return rand.New(rand.NewPCG(uint64(seed), uint64(seed)^0x9e3779b97f4a7c15))
	}
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		// crypto/rand only fails catastrophically; fall back to the clock.
		now := uint64(time.Now().UnixNano())
		return rand.New(rand.NewPCG(now, now>>7))
	}
	return rand.New(rand.NewPCG(
		binary.LittleEndian.Uint64(b[:8]),
		binary.LittleEndian.Uint64(b[8:]),
	))
}

// parseSpan accepts Go durations plus the day, week and year units that make
// sense for a reading backlog: "90d", "2w", "1y", "36h".
func parseSpan(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	if n := len(s); n > 1 {
		mult := map[byte]time.Duration{
			'd': 24 * time.Hour,
			'w': 7 * 24 * time.Hour,
			'y': 365 * 24 * time.Hour,
		}[s[n-1]]
		if mult > 0 {
			if v, err := strconv.ParseFloat(s[:n-1], 64); err == nil {
				return time.Duration(v * float64(mult)), nil
			}
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (try 90d, 2w, or 36h)", s)
	}
	return d, nil
}

func prompt(msg string) (string, error) {
	fmt.Print(msg)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("reading input: %w", err)
	}
	return strings.TrimSpace(line), nil
}

func promptPassword(msg string) (string, error) {
	fd := int(syscall.Stdin)
	if !term.IsTerminal(fd) {
		// Piped input: read a line normally so scripts still work. Nothing was
		// echoed, so supply the newline the terminal path would have produced.
		s, err := prompt(msg)
		fmt.Println()
		return s, err
	}
	fmt.Print(msg)
	b, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	return string(b), nil
}

func titleOf(b library.Bookmark) string {
	if t := strings.TrimSpace(b.Title); t != "" {
		return t
	}
	if h := b.Host(); h != "" {
		return "(untitled — " + h + ")"
	}
	return "(untitled)"
}

func describe(b library.Bookmark) string {
	var parts []string
	if h := b.Host(); h != "" {
		parts = append(parts, h)
	}
	if b.Time > 0 {
		parts = append(parts, "saved "+formatAge(b.Age())+" ago")
	}
	if b.Starred {
		parts = append(parts, "starred")
	}
	if b.Progress > 0.01 {
		parts = append(parts, fmt.Sprintf("%.0f%% read", b.Progress*100))
	}
	return strings.Join(parts, " · ")
}

func formatAge(d time.Duration) string {
	switch days := int(d.Hours() / 24); {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute", "minutes")
	case days < 1:
		return plural(int(d.Hours()), "hour", "hours")
	case days < 14:
		return plural(days, "day", "days")
	case days < 60:
		return plural(days/7, "week", "weeks")
	case days < 730:
		return plural(days/30, "month", "months")
	default:
		return plural(days/365, "year", "years")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return comma(n) + " " + many
}

func pronoun(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// comma formats an integer with thousands separators.
func comma(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}
