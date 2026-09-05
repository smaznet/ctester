package app

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/aria/x-tester/internal/activity"
	"github.com/aria/x-tester/internal/balancer"
	"github.com/aria/x-tester/internal/config"
	"github.com/aria/x-tester/internal/listen"
	"github.com/aria/x-tester/internal/logbuf"
	"github.com/aria/x-tester/internal/metrics"
	"github.com/aria/x-tester/internal/probe"
	"github.com/aria/x-tester/internal/stats"
	"github.com/aria/x-tester/internal/store"
	"github.com/aria/x-tester/internal/sub"
	"github.com/aria/x-tester/internal/tui"
	"github.com/aria/x-tester/internal/xray"
	"github.com/fsnotify/fsnotify"
	"github.com/mattn/go-isatty"
)

type App struct {
	configPath string
	log        *log.Logger
	logs       *logbuf.Ring
	met        *metrics.Collector
	act        *activity.Tracker
	db         *store.DB

	mu     sync.Mutex
	cfg    *config.Config
	bal    *balancer.Sticky
	main   *xray.Instance
	probe  *xray.Instance
	listen *listen.Server
	stats  *stats.Server
	work   string

	mounted map[string]*xray.NodeLocal
	nodes   map[string]sub.Node
	standby map[string]probe.Result // healthy, not mounted — fast replace pool
	probing bool
}

func Run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	logs := logbuf.New(200)
	headless := !isatty.IsTerminal(os.Stdout.Fd()) && !isatty.IsCygwinTerminal(os.Stdout.Fd())
	var logOut io.Writer = logs
	if headless {
		// journald / systemd: mirror ring into stdout
		logOut = io.MultiWriter(logs, os.Stdout)
	}
	logger := log.New(logOut, "", 0)
	met := metrics.New()
	act := activity.New()

	dbPath := cfg.Database
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(filepath.Dir(configPath), dbPath)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	work, err := os.MkdirTemp("", "x-tester-*")
	if err != nil {
		return err
	}
	a := &App{
		configPath: configPath,
		log:        logger,
		logs:       logs,
		met:        met,
		act:        act,
		db:         db,
		cfg:        cfg,
		bal:        balancer.New(cfg.Balancer),
		work:       work,
		mounted:    map[string]*xray.NodeLocal{},
		nodes:      map[string]sub.Node{},
		standby:    map[string]probe.Result{},
	}
	defer os.RemoveAll(work)

	logger.Printf("database %s (ignored=%d active=%d failed=%d)",
		db.Path(), db.CountIgnored(), db.CountByStatus(store.StatusActive), db.CountByStatus(store.StatusFailed))

	// If filter now allows a previously ignored country, drop those rows
	if n, err := db.UnignoreIfAllowed(cfg.CountryAllowed); err != nil {
		logger.Printf("db unignore: %v", err)
	} else if n > 0 {
		logger.Printf("unignored %d nodes now allowed by filter_country", n)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := a.startXray(cfg); err != nil {
		return err
	}
	defer a.stopXray()

	a.stats = stats.New(cfg.Stats, a.bal)
	a.stats.SetDashboard(a.dashboard)
	if err := a.stats.Start(); err != nil {
		return err
	}
	defer a.stats.Close()

	a.listen = listen.New(cfg.Listen, a.bal, logger, met)
	if err := a.listen.Start(ctx); err != nil {
		return err
	}
	defer a.listen.Close()

	go a.watchConfig(ctx)
	go a.loopSub(ctx)
	go a.loopProbe(ctx)

	a.log.Printf("listening mode=%s accept_proxy_protocol=%v balancer=%s stats=%s:%d",
		cfg.Listen.Mode, cfg.Listen.AcceptProxyProtocol, cfg.Balancer, cfg.Stats.Host, cfg.Stats.Port)

	if headless {
		a.log.Printf("headless mode (no tty) — waiting for signal")
		<-ctx.Done()
		a.log.Printf("shutting down")
		return nil
	}

	prog := tui.New(a.tuiStatus, logs)
	// bubbletea handles ctrl+c; also stop app when TUI quits
	done := make(chan error, 1)
	go func() {
		_, err := prog.Run()
		done <- err
		cancel()
	}()

	select {
	case <-ctx.Done():
		prog.Quit()
		<-done
	case err := <-done:
		if err != nil {
			return err
		}
	}
	a.log.Printf("shutting down")
	return nil
}

func (a *App) tuiStatus() tui.Status {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	counts := a.bal.Counts()
	nodes := a.bal.Snapshot()
	sorted := make([]balancer.NodeState, 0, len(nodes))
	var rest []balancer.NodeState
	for _, n := range nodes {
		if n.Ignored {
			continue // hide ignored from main list (shown in counts)
		}
		if n.Active && !n.Failed {
			sorted = append(sorted, n)
		} else {
			rest = append(rest, n)
		}
	}
	sorted = append(sorted, rest...)

	listenAddr := cfg.Listen.Unix
	if listenAddr == "" {
		if cfg.Listen.Port.IsRange() {
			listenAddr = net.JoinHostPort(cfg.Listen.Host, fmt.Sprintf("%d-%d", cfg.Listen.Port.Start, cfg.Listen.Port.End))
		} else {
			listenAddr = net.JoinHostPort(cfg.Listen.Host, strconv.Itoa(cfg.Listen.Port.Start))
		}
	}
	return tui.Status{
		Mode:       cfg.Listen.Mode,
		Balancer:   cfg.Balancer,
		Listen:     listenAddr,
		StatsAddr:  net.JoinHostPort(cfg.Stats.Host, strconv.Itoa(cfg.Stats.Port)),
		TotalNodes: counts.Total,
		Active:     counts.Active,
		Standby:    counts.Standby,
		Failed:     counts.Failed,
		Ignored:    counts.Ignored,
		Pending:    counts.Pending,
		Countries:  a.bal.CountryGroups(),
		Filter:     append([]string{}, cfg.FilterCountry...),
		Traffic:    a.met.Snapshot(),
		Nodes:      sorted,
		Logs:       a.logs.Lines(16),
		Activity:   a.act.Snapshot(),
	}
}

func (a *App) dashboard() stats.Dashboard {
	st := a.tuiStatus()
	return stats.Dashboard{
		Mode:      st.Mode,
		Balancer:  st.Balancer,
		Listen:    st.Listen,
		StatsAddr: st.StatsAddr,
		Count:     st.TotalNodes,
		Active:    st.Active,
		Standby:   st.Standby,
		Failed:    st.Failed,
		Ignored:   st.Ignored,
		Pending:   st.Pending,
		Countries: st.Countries,
		Filter:    st.Filter,
		Traffic:   stats.TrafficFromMetrics(st.Traffic),
		Activity:  stats.ActivityFromTracker(st.Activity),
		Logs:      st.Logs,
		Nodes:     stats.NodesFromBalancer(st.Nodes),
	}
}

func (a *App) startXray(cfg *config.Config) error {
	bin := cfg.XrayBin
	mainInst, err := xray.StartMain(bin, filepath.Join(a.work, "main"))
	if err != nil {
		return err
	}
	probeInst, err := xray.StartProbe(bin, filepath.Join(a.work, "probe"))
	if err != nil {
		_ = mainInst.Stop()
		return err
	}
	a.main = mainInst
	a.probe = probeInst
	a.log.Printf("xray main api=%s probe api=%s", mainInst.APIAddr, probeInst.APIAddr)
	return nil
}

func (a *App) stopXray() {
	if a.probe != nil {
		_ = a.probe.Stop()
	}
	if a.main != nil {
		_ = a.main.Stop()
	}
}

func (a *App) watchConfig(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		a.log.Printf("config watch: %v", err)
		return
	}
	defer w.Close()
	dir := filepath.Dir(a.configPath)
	_ = w.Add(dir)
	var debounce *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events:
			if filepath.Clean(ev.Name) != filepath.Clean(a.configPath) {
				continue
			}
			if debounce != nil {
				debounce.Stop()
			}
			debounce = time.AfterFunc(400*time.Millisecond, func() {
				a.reloadConfig()
			})
		case err := <-w.Errors:
			a.log.Printf("watch error: %v", err)
		}
	}
}

func (a *App) reloadConfig() {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		a.log.Printf("reload config failed: %v", err)
		return
	}
	a.mu.Lock()
	old := a.cfg
	a.cfg = cfg
	a.bal.SetMode(cfg.Balancer)
	a.mu.Unlock()

	if a.db != nil {
		if n, err := a.db.UnignoreIfAllowed(cfg.CountryAllowed); err != nil {
			a.log.Printf("db unignore on reload: %v", err)
		} else if n > 0 {
			a.log.Printf("reload: unignored %d nodes now in filter_country", n)
		}
	}

	needListenRestart := old.Listen != cfg.Listen
	if needListenRestart {
		a.log.Printf("listen config changed; restart listener")
		_ = a.listen.Close()
		a.listen = listen.New(cfg.Listen, a.bal, a.log, a.met)
		if err := a.listen.Start(context.Background()); err != nil {
			a.log.Printf("restart listen: %v", err)
		}
	}
	a.log.Printf("config reloaded")
}

func (a *App) loopSub(ctx context.Context) {
	for {
		a.refreshSubs(ctx)
		a.mu.Lock()
		d := a.cfg.SubRefresh.Std()
		a.mu.Unlock()
		if d <= 0 {
			d = 5 * time.Minute
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *App) refreshSubs(ctx context.Context) {
	a.mu.Lock()
	urls := append([]string{}, a.cfg.SubURLs...)
	a.mu.Unlock()
	a.act.Set("fetching", fmt.Sprintf("%d subscription url(s)", len(urls)))
	a.log.Printf("fetching %d subscription url(s)", len(urls))
	nodes, errs := sub.FetchAll(urls, 30*time.Second)
	for _, e := range errs {
		a.log.Printf("sub error: %v", e)
	}

	type remount struct {
		node sub.Node
		st   store.NodeState
	}
	var toRemount []remount
	ignoredN, skippedN, resumeN := 0, 0, 0

	a.mu.Lock()
	a.nodes = map[string]sub.Node{}
	for _, n := range nodes {
		a.nodes[n.ID] = n
		dbSt, hasDB := a.db.Get(n.ID)

		switch {
		case hasDB && dbSt.Status == store.StatusIgnored:
			ignoredN++
			a.bal.Upsert(balancer.NodeState{
				ID: n.ID, Name: n.Name, SubURL: n.SubURL, Address: n.AddressPort(),
				Ignored: true, Country: dbSt.Country, LastCheck: dbSt.LastCheck,
			})
		case hasDB && !dbSt.LastCheck.IsZero():
			skippedN++
			active := dbSt.Status == store.StatusActive
			failed := dbSt.Status == store.StatusFailed
			a.bal.Upsert(balancer.NodeState{
				ID: n.ID, Name: n.Name, SubURL: n.SubURL, Address: n.AddressPort(),
				Active: false, // not mounted yet; remount below if was active
				Failed: failed, Country: dbSt.Country, ExitIP: dbSt.ExitIP,
				Latency: dbSt.Latency, LastCheck: dbSt.LastCheck, FailCount: dbSt.FailCount,
			})
			if active {
				resumeN++
				toRemount = append(toRemount, remount{node: n, st: dbSt})
			}
		default:
			a.bal.UpsertPending(balancer.NodeState{
				ID: n.ID, Name: n.Name, SubURL: n.SubURL, Address: n.AddressPort(),
			})
		}
	}
	a.mu.Unlock()

	a.log.Printf("subscription ready: %d nodes (db: ignored=%d known=%d resume_active=%d)",
		len(nodes), ignoredN, skippedN, resumeN)
	a.act.Set("fetched", fmt.Sprintf("%d nodes · resume %d", len(nodes), resumeN))

	// Remount previously-active nodes without retesting (continue after restart)
	if len(toRemount) > 0 {
		maxActive := a.cfg.Probe.MaxActive
		if maxActive > 0 && len(toRemount) > maxActive {
			a.log.Printf("max_active=%d: remounting %d of %d previously active", maxActive, maxActive, len(toRemount))
			toRemount = toRemount[:maxActive]
		}
		a.act.Set("resuming", fmt.Sprintf("remount %d active from db", len(toRemount)))
		a.log.Printf("resuming %d previously active nodes from database", len(toRemount))
		for _, item := range toRemount {
			r := probe.Result{
				Node:    item.node,
				OK:      true,
				Country: item.st.Country,
				ExitIP:  item.st.ExitIP,
				Latency: item.st.Latency,
			}
			a.ensureMounted(ctx, r, true) // keep LastCheck from db
		}
		a.log.Printf("resume remount done")
	}

	if len(nodes) == 0 {
		a.act.Idle("no nodes from subscriptions")
	} else {
		a.act.Idle(fmt.Sprintf("ready · %d known from db skipped until interval", skippedN))
	}
}

func (a *App) loopProbe(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.promoteStandbys(ctx)
			due := a.nodesDue()
			if len(due) == 0 {
				continue
			}
			a.runProbeRound(ctx, due)
		}
	}
}

func (a *App) nodesDue() []sub.Node {
	a.mu.Lock()
	cfg := a.cfg
	nodes := a.nodes
	standbyIDs := make(map[string]struct{}, len(a.standby))
	for id := range a.standby {
		standbyIDs[id] = struct{}{}
	}
	standbyHave := len(a.standby)
	a.mu.Unlock()

	activeIV := cfg.Probe.IntervalActive.Std()
	failedIV := cfg.Probe.IntervalFailed.Std()
	maxActive := cfg.Probe.MaxActive
	standbyWant := cfg.Probe.Standby
	activeCount := a.bal.Counts().Active
	atActiveCap := maxActive > 0 && activeCount >= maxActive
	standbyFull := standbyWant <= 0 || standbyHave >= standbyWant
	needFill := !atActiveCap || !standbyFull
	now := time.Now()

	var recheck, fill []sub.Node
	for id, n := range nodes {
		if a.db != nil && a.db.IsIgnored(id) {
			continue
		}
		st, ok := a.bal.Get(id)
		if ok && st.Ignored {
			continue
		}
		isActive := ok && st.Active && !st.Failed
		_, inStandby := standbyIDs[id]
		if isActive || inStandby {
			if !ok || st.LastCheck.IsZero() || now.Sub(st.LastCheck) >= activeIV {
				recheck = append(recheck, n)
			}
			continue
		}
		if !needFill {
			continue
		}
		if !ok || st.LastCheck.IsZero() {
			fill = append(fill, n)
			continue
		}
		if now.Sub(st.LastCheck) >= failedIV {
			fill = append(fill, n)
		}
	}
	if len(recheck) == 0 && len(fill) == 0 {
		return nil
	}
	sub.Shuffle(recheck)
	sub.Shuffle(fill)
	return append(recheck, fill...)
}

func (a *App) runProbeRound(ctx context.Context, nodes []sub.Node) {
	a.mu.Lock()
	if a.probing {
		a.mu.Unlock()
		return
	}
	a.probing = true
	cfg := a.cfg
	probeInst := a.probe
	mainInst := a.main
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.probing = false
		a.mu.Unlock()
	}()

	if len(nodes) == 0 || probeInst == nil || mainInst == nil {
		return
	}

	batch := cfg.Probe.MountBatch
	maxActive := cfg.Probe.MaxActive
	standbyWant := cfg.Probe.Standby
	a.act.BeginProbe(len(nodes))
	a.log.Printf("probe start: %d due concurrency=%d mount_batch=%d max_active=%d standby=%d geo=%v filter=%v",
		len(nodes), cfg.Probe.Concurrency, batch, maxActive, standbyWant, cfg.NeedGeo(), cfg.FilterCountry)

	var (
		resMu   sync.Mutex
		pending []probe.Result
		alive   = map[string]probe.Result{}
		okN     int
		failN   int
		ignN    int
		skipN   int
		standN  int
	)

	flushMount := func(reason string) {
		resMu.Lock()
		toMount := pending
		pending = nil
		n := len(toMount)
		totalOK := okN
		resMu.Unlock()
		if n == 0 {
			return
		}
		a.act.Set("mounting", fmt.Sprintf("+%d → main (ok=%d)", n, totalOK))
		a.log.Printf("mount batch %s: +%d nodes (healthy so far %d)", reason, n, totalOK)
		for _, r := range toMount {
			a.ensureMounted(ctx, r, false)
		}
		a.evictSlowActives(ctx)
		a.promoteStandbys(ctx)
	}

	atActiveCap := func() bool {
		return maxActive > 0 && a.bal.Counts().Active >= maxActive
	}
	standbyFull := func() bool {
		if standbyWant <= 0 {
			return true
		}
		a.mu.Lock()
		n := len(a.standby)
		a.mu.Unlock()
		return n >= standbyWant
	}
	poolFull := func() bool {
		return atActiveCap() && standbyFull()
	}

	pool := &probe.Pool{
		Concurrency: cfg.Probe.Concurrency,
		Delay:       cfg.Probe.Delay.Std(),
		Checker: &probe.Checker{
			Probe:   probeInst,
			Check:   cfg.HTTPCheck,
			Timeout: cfg.HTTPCheck.Timeout.Std(),
			NeedGeo: cfg.NeedGeo(),
			GeoURL:  cfg.Grouping.URL,
			AllowCountry: func(code string) bool {
				return cfg.CountryAllowed(code)
			},
		},
		ShouldSkip: func(n sub.Node) bool {
			st, ok := a.bal.Get(n.ID)
			if ok && st.Active && !st.Failed {
				return false
			}
			a.mu.Lock()
			_, inStandby := a.standby[n.ID]
			a.mu.Unlock()
			if inStandby {
				return false // refresh warm pool
			}
			if !poolFull() {
				return false
			}
			resMu.Lock()
			skipN++
			resMu.Unlock()
			return true
		},
		OnStart: func(n sub.Node) {
			name := n.Name
			if name == "" {
				name = n.AddressPort()
			}
			a.act.StartNode(n.ID, name)
			a.log.Printf("testing %s (%s %s)", name, n.Protocol, n.AddressPort())
		},
		OnResult: func(r probe.Result) {
			name := r.Node.Name
			if name == "" {
				name = r.Node.AddressPort()
			}
			lat := r.Latency.Round(time.Millisecond).String()

			if r.Ignored {
				a.act.FinishNode(r.Node.ID, name, false, lat, "ignore "+r.Country)
				a.log.Printf("IGNORE %s country=%s — will not retest", name, r.Country)
				resMu.Lock()
				ignN++
				resMu.Unlock()
				a.markIgnored(r)
				return
			}

			if !r.OK {
				errMsg := r.Error
				if len(errMsg) > 160 {
					errMsg = errMsg[:160] + "…"
				}
				a.act.FinishNode(r.Node.ID, name, false, lat, errMsg)
				a.log.Printf("FAIL %s [%s %s] %s", name, r.Node.Protocol, r.Node.AddressPort(), errMsg)
				resMu.Lock()
				failN++
				resMu.Unlock()
				a.markFailed(r)
				return
			}

			extra := ""
			if r.Country != "" {
				extra = " country=" + r.Country
			}
			a.act.FinishNode(r.Node.ID, name, true, lat, "")
			a.log.Printf("OK %s latency=%s%s", name, lat, extra)

			st, _ := a.bal.Get(r.Node.ID)
			alreadyActive := st.Active && !st.Failed
			if alreadyActive || !atActiveCap() {
				doFlush := false
				resMu.Lock()
				okN++
				alive[r.Node.ID] = r
				pending = append(pending, r)
				if batch > 0 && okN%batch == 0 {
					doFlush = true
				}
				resMu.Unlock()
				if doFlush {
					flushMount(fmt.Sprintf("every %d", batch))
				}
				return
			}

			// At active cap: replace a slow active if this node is meaningfully faster.
			if a.tryReplaceSlowActive(ctx, r) {
				resMu.Lock()
				okN++
				alive[r.Node.ID] = r
				resMu.Unlock()
				return
			}

			// Too slow vs best active → out of cycle (not standby).
			if a.tooSlowVsBestActive(r.Latency) {
				a.log.Printf("reject slow %s latency=%s (over best+tolerance) — out of cycle",
					name, r.Latency.Round(time.Millisecond))
				resMu.Lock()
				okN++
				resMu.Unlock()
				a.markFailed(r)
				return
			}

			// Otherwise warm standby.
			if a.offerStandby(r) {
				resMu.Lock()
				okN++
				standN++
				alive[r.Node.ID] = r
				resMu.Unlock()
				return
			}
			a.log.Printf("pools full (active=%d/%s standby full) — skip %s",
				a.bal.Counts().Active, maxActiveLabel(maxActive), name)
		},
	}
	_ = pool.Run(ctx, nodes)
	flushMount("remainder")
	a.promoteStandbys(ctx)

	resMu.Lock()
	aliveCopy := make(map[string]probe.Result, len(alive))
	for k, v := range alive {
		aliveCopy[k] = v
	}
	finalOK, finalFail, finalIgn, finalSkip, finalStand := okN, failN, ignN, skipN, standN
	resMu.Unlock()

	dueIDs := map[string]struct{}{}
	for _, n := range nodes {
		dueIDs[n.ID] = struct{}{}
	}
	a.mu.Lock()
	mounted := make([]string, 0, len(a.mounted))
	for id := range a.mounted {
		mounted = append(mounted, id)
	}
	a.mu.Unlock()
	for _, id := range mounted {
		if _, wasDue := dueIDs[id]; !wasDue {
			continue
		}
		if _, ok := aliveCopy[id]; ok {
			continue
		}
		a.unmount(ctx, id, "unmounted after failed recheck")
	}
	a.evictSlowActives(ctx)
	a.promoteStandbys(ctx)

	a.mu.Lock()
	sb := len(a.standby)
	a.mu.Unlock()
	a.log.Printf("probe done: ok=%d fail=%d ignored=%d skipped=%d →standby=%d active=%d standby=%d/%d",
		finalOK, finalFail, finalIgn, finalSkip, finalStand, a.bal.Counts().Active, sb, standbyWant)
	a.act.Idle(fmt.Sprintf("ready · active=%d/%s standby=%d/%d ok=%d fail=%d",
		a.bal.Counts().Active, maxActiveLabel(maxActive), sb, standbyWant, finalOK, finalFail))
}

func (a *App) saveState(st balancer.NodeState, status, reason string) {
	if a.db == nil {
		return
	}
	if status == "" {
		switch {
		case st.Ignored:
			status = store.StatusIgnored
		case st.Active && !st.Failed:
			status = store.StatusActive
		case st.Failed:
			status = store.StatusFailed
		default:
			status = store.StatusPending
		}
	}
	err := a.db.Save(store.NodeState{
		ID:        st.ID,
		Address:   st.Address,
		Name:      st.Name,
		SubURL:    st.SubURL,
		Country:   st.Country,
		ExitIP:    st.ExitIP,
		Status:    status,
		Latency:   st.Latency,
		FailCount: st.FailCount,
		Reason:    reason,
		LastCheck: st.LastCheck,
	})
	if err != nil {
		a.log.Printf("db save %s: %v", st.ID, err)
	}
}

func (a *App) markIgnored(r probe.Result) {
	a.dropStandby(r.Node.ID)
	a.mu.Lock()
	if local, ok := a.mounted[r.Node.ID]; ok {
		delete(a.mounted, r.Node.ID)
		mainInst := a.main
		a.mu.Unlock()
		if mainInst != nil {
			_ = mainInst.RemoveNode(context.Background(), local.OutboundTag, local.InboundTag)
		}
	} else {
		a.mu.Unlock()
	}
	reason := r.Error
	if reason == "" {
		reason = "country filtered: " + r.Country
	}
	now := time.Now()
	st := balancer.NodeState{
		ID: r.Node.ID, Name: r.Node.Name, SubURL: r.Node.SubURL, Address: r.Node.AddressPort(),
		Ignored: true, Country: r.Country, ExitIP: r.ExitIP, Latency: r.Latency, LastCheck: now,
	}
	a.bal.Upsert(st)
	a.saveState(st, store.StatusIgnored, reason)
}

func (a *App) markFailed(r probe.Result) {
	a.dropStandby(r.Node.ID)
	a.mu.Lock()
	localPort := 0
	if m, ok := a.mounted[r.Node.ID]; ok {
		localPort = m.LocalPort
	}
	prev, _ := a.bal.Get(r.Node.ID)
	a.mu.Unlock()
	now := time.Now()
	st := balancer.NodeState{
		ID: r.Node.ID, Name: r.Node.Name, SubURL: r.Node.SubURL, Address: r.Node.AddressPort(),
		LocalPort: localPort, Active: false, Failed: true, Standby: false,
		Country: firstNonEmpty(r.Country, prev.Country), ExitIP: firstNonEmpty(r.ExitIP, prev.ExitIP),
		Latency: r.Latency, LastCheck: now, FailCount: prev.FailCount + 1,
	}
	a.bal.Upsert(st)
	a.saveState(st, store.StatusFailed, r.Error)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (a *App) unmount(ctx context.Context, id string, reason string) {
	a.removeActive(ctx, id, reason)
	a.promoteStandbys(ctx)
}

// removeActive unmounts without promoting standbys (used when a faster node will take the slot).
func (a *App) removeActive(ctx context.Context, id string, reason string) {
	a.dropStandby(id)
	a.mu.Lock()
	local, ok := a.mounted[id]
	if ok {
		delete(a.mounted, id)
	}
	node := a.nodes[id]
	prev, _ := a.bal.Get(id)
	mainInst := a.main
	a.mu.Unlock()
	if !ok || mainInst == nil {
		return
	}
	if err := mainInst.RemoveNode(ctx, local.OutboundTag, local.InboundTag); err != nil {
		a.log.Printf("remove node %s: %v", id, err)
	}
	if reason == "" {
		reason = "unmounted after failed recheck"
	}
	now := time.Now()
	st := balancer.NodeState{
		ID: id, Name: node.Name, SubURL: node.SubURL, Address: node.AddressPort(),
		Active: false, Failed: true, Standby: false, Country: prev.Country, ExitIP: prev.ExitIP,
		Latency: prev.Latency, LastCheck: now, FailCount: prev.FailCount + 1,
	}
	a.bal.Upsert(st)
	a.saveState(st, store.StatusFailed, reason)
	a.log.Printf("unmounted %s (%s) latency=%s", id, reason, prev.Latency.Round(time.Millisecond))
}

func (a *App) ensureMounted(ctx context.Context, r probe.Result, resume bool) {
	a.dropStandby(r.Node.ID)
	a.mu.Lock()
	local, exists := a.mounted[r.Node.ID]
	mainInst := a.main
	prev, _ := a.bal.Get(r.Node.ID)
	maxActive := a.cfg.Probe.MaxActive
	a.mu.Unlock()
	if mainInst == nil {
		return
	}
	if !exists {
		if maxActive > 0 && a.bal.Counts().Active >= maxActive {
			if a.tryReplaceSlowActive(ctx, r) {
				return
			}
			if a.offerStandby(r) {
				return
			}
			a.log.Printf("max_active=%d — skip mount %s", maxActive, r.Node.Name)
			return
		}
		tag := "m-" + r.Node.ID
		nlocal, err := mainInst.AddNode(ctx, tag, r.Node.Outbound)
		if err != nil {
			a.log.Printf("mount FAIL %s [%s %s]: %v", r.Node.Name, r.Node.Protocol, r.Node.AddressPort(), err)
			a.markFailed(r)
			return
		}
		a.mu.Lock()
		a.mounted[r.Node.ID] = nlocal
		a.mu.Unlock()
		local = nlocal
		extra := ""
		if r.Country != "" {
			extra = " country=" + r.Country
		}
		prefix := "mounted OK"
		if resume {
			prefix = "resumed OK"
		}
		a.log.Printf("%s %s (%s) latency=%s%s → :%d", prefix, r.Node.Name, r.Node.AddressPort(), r.Latency.Round(time.Millisecond), extra, local.LocalPort)
	}
	lastCheck := time.Now()
	if resume && !prev.LastCheck.IsZero() {
		lastCheck = prev.LastCheck
	}
	st := balancer.NodeState{
		ID: r.Node.ID, Name: r.Node.Name, SubURL: r.Node.SubURL, Address: r.Node.AddressPort(),
		LocalPort: local.LocalPort, Active: true, Failed: false, Standby: false,
		Country: r.Country, ExitIP: r.ExitIP, Latency: r.Latency, LastCheck: lastCheck,
	}
	a.bal.Upsert(st)
	a.saveState(st, store.StatusActive, "")
}

func (a *App) offerStandby(r probe.Result) bool {
	if a.tooSlowVsBestActive(r.Latency) {
		return false
	}
	a.mu.Lock()
	want := a.cfg.Probe.Standby
	if want <= 0 {
		a.mu.Unlock()
		return false
	}
	if _, exists := a.mounted[r.Node.ID]; exists {
		a.mu.Unlock()
		return false
	}
	if _, exists := a.standby[r.Node.ID]; exists {
		a.standby[r.Node.ID] = r
		a.mu.Unlock()
		now := time.Now()
		a.bal.Upsert(balancer.NodeState{
			ID: r.Node.ID, Name: r.Node.Name, SubURL: r.Node.SubURL, Address: r.Node.AddressPort(),
			Active: false, Failed: false, Standby: true,
			Country: r.Country, ExitIP: r.ExitIP, Latency: r.Latency, LastCheck: now,
		})
		return true
	}
	if len(a.standby) >= want {
		a.mu.Unlock()
		return false
	}
	a.standby[r.Node.ID] = r
	n := len(a.standby)
	a.mu.Unlock()
	now := time.Now()
	a.bal.Upsert(balancer.NodeState{
		ID: r.Node.ID, Name: r.Node.Name, SubURL: r.Node.SubURL, Address: r.Node.AddressPort(),
		Active: false, Failed: false, Standby: true,
		Country: r.Country, ExitIP: r.ExitIP, Latency: r.Latency, LastCheck: now,
	})
	a.log.Printf("standby +%s (%d/%d) latency=%s", r.Node.Name, n, want, r.Latency.Round(time.Millisecond))
	return true
}

func (a *App) dropStandby(id string) {
	a.mu.Lock()
	delete(a.standby, id)
	a.mu.Unlock()
}

func (a *App) promoteStandbys(ctx context.Context) {
	for {
		a.mu.Lock()
		maxActive := a.cfg.Probe.MaxActive
		a.mu.Unlock()
		if maxActive <= 0 {
			return
		}
		if a.bal.Counts().Active >= maxActive {
			return
		}
		r, ok := a.popStandby()
		if !ok {
			return
		}
		if a.tooSlowVsBestActive(r.Latency) {
			a.log.Printf("standby drop slow %s latency=%s", r.Node.Name, r.Latency.Round(time.Millisecond))
			a.markFailed(r)
			continue
		}
		name := r.Node.Name
		if name == "" {
			name = r.Node.AddressPort()
		}
		a.log.Printf("promote standby → active %s", name)
		a.ensureMounted(ctx, r, false)
	}
}

func (a *App) popStandby() (probe.Result, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.standby) == 0 {
		return probe.Result{}, false
	}
	ids := make([]string, 0, len(a.standby))
	for id := range a.standby {
		ids = append(ids, id)
	}
	id := ids[time.Now().UnixNano()%int64(len(ids))]
	r := a.standby[id]
	delete(a.standby, id)
	return r, true
}

func (a *App) bestActiveLatency() (time.Duration, bool) {
	actives := a.bal.Active()
	if len(actives) == 0 {
		return 0, false
	}
	best := actives[0].Latency
	for _, n := range actives[1:] {
		if n.Latency > 0 && (best <= 0 || n.Latency < best) {
			best = n.Latency
		}
	}
	if best <= 0 {
		return 0, false
	}
	return best, true
}

func (a *App) worstActive() (balancer.NodeState, bool) {
	actives := a.bal.Active()
	if len(actives) == 0 {
		return balancer.NodeState{}, false
	}
	worst := actives[0]
	for _, n := range actives[1:] {
		if n.Latency > worst.Latency {
			worst = n
		}
	}
	return worst, true
}

func (a *App) tooSlowVsBestActive(lat time.Duration) bool {
	a.mu.Lock()
	tol := a.cfg.Probe.LatencyTolerance.Std()
	a.mu.Unlock()
	if tol <= 0 || lat <= 0 {
		return false
	}
	best, ok := a.bestActiveLatency()
	if !ok {
		return false
	}
	return lat > best+tol
}

// evictSlowActives removes actives whose latency exceeds best+tolerance.
// Evicted nodes are failed (out of cycle), never moved to standby.
func (a *App) evictSlowActives(ctx context.Context) {
	a.mu.Lock()
	tol := a.cfg.Probe.LatencyTolerance.Std()
	a.mu.Unlock()
	if tol <= 0 {
		return
	}
	best, ok := a.bestActiveLatency()
	if !ok {
		return
	}
	for _, n := range a.bal.Active() {
		if n.Latency <= 0 {
			continue
		}
		if n.Latency > best+tol {
			reason := fmt.Sprintf("latency tolerance: %s > best %s + %s",
				n.Latency.Round(time.Millisecond), best.Round(time.Millisecond), tol.Round(time.Millisecond))
			a.log.Printf("evict slow active %s (%s)", n.Name, reason)
			a.unmount(ctx, n.ID, reason)
		}
	}
}

// tryReplaceSlowActive swaps the worst active for r when r is faster by more than tolerance.
func (a *App) tryReplaceSlowActive(ctx context.Context, r probe.Result) bool {
	a.mu.Lock()
	tol := a.cfg.Probe.LatencyTolerance.Std()
	a.mu.Unlock()
	if tol <= 0 {
		return false
	}
	worst, ok := a.worstActive()
	if !ok || r.Latency <= 0 {
		return false
	}
	if worst.Latency-r.Latency <= tol {
		return false
	}
	reason := fmt.Sprintf("replaced by faster node (was %s, new %s, tol %s)",
		worst.Latency.Round(time.Millisecond), r.Latency.Round(time.Millisecond), tol.Round(time.Millisecond))
	a.log.Printf("replace active %s → %s (%s)", worst.Name, r.Node.Name, reason)
	a.removeActive(ctx, worst.ID, reason)
	a.ensureMounted(ctx, r, false)
	a.promoteStandbys(ctx)
	return true
}

func maxActiveLabel(n int) string {
	if n <= 0 {
		return "∞"
	}
	return strconv.Itoa(n)
}
