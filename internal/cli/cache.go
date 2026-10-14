package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zzstar101/mytoken/internal/app"
	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/model"
	"github.com/zzstar101/mytoken/internal/query"
)

// cacheService is the slice of query.Service the cache command needs. Keeping it
// narrow (like relayService) lets the command run against a fake in tests: the
// cache ledger is the only part of the query surface it touches.
type cacheService interface {
	CacheTotals(ctx context.Context, f query.Filter) (query.CacheTotals, error)
	CacheDaily(ctx context.Context, f query.Filter) ([]query.CachePoint, error)
	CacheBy(ctx context.Context, f query.Filter, dim query.CacheDim) ([]query.CacheBucket, error)
	SessionCache(ctx context.Context, h model.Harness, session string) ([]query.CacheStep, error)
}

// instanceCacheService is the instance-aware extension of cacheService:
// *query.service implements it, so a session timeline scoped to exactly one
// --instance-id reads that instance's steps instead of the whole index (a
// session id can repeat across instances).
type instanceCacheService interface {
	SessionCacheIn(ctx context.Context, h model.Harness, instance, session string) ([]query.CacheStep, error)
}

// cacheEnv is everything a cache run needs once the flags are parsed.
type cacheEnv struct {
	rng         query.Range
	format      outputFormat
	noCost      bool
	harness     string
	providers   []string // --provider: original pricing provider names
	providerIDs []string // --provider-id: stable endpoint identity keys
	instanceIDs []string // --instance-id: instance ids from sources.json
	service     cacheService
}

// cacheDims maps the --by values that select a CacheBy dimension. "cause" and
// "day" are not dimensions: cause is the CacheTotals breakdown and day comes
// from CacheDaily.
var cacheDims = map[string]query.CacheDim{
	"provider": query.CacheByProvider,
	"model":    query.CacheByModel,
	"harness":  query.CacheByHarness,
	"project":  query.CacheByProject,
	"session":  query.CacheBySession,
}

// cacheArgs is the parsed cache command line.
type cacheArgs struct {
	by      string
	session string
	harness string
	opts    options
}

// parseCache parses and validates the cache command line. A positional session
// id switches to timeline mode, which has no --by and ignores the range flags
// (a timeline always covers the whole session).
func parseCache(args []string, errout io.Writer) (cacheArgs, int) {
	var req cacheArgs
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	fs.SetOutput(errout)
	var cf commonFlags
	addCommonFlags(fs, &cf, "7d")
	by := fs.String("by", "cause", "grouping: cause, provider, model, harness, project, session or day")
	harnessFlag := fs.String("harness", "", "harness owning the session timeline (default: resolve it from the ledger)")
	flags, positional, e := splitArgs(args, map[string]bool{"json": true, "offline": true, "no-cost": true})
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	if e := cf.parseFlags(fs, flags); e != nil {
		return req, 2
	}
	if len(positional) > 1 {
		fmt.Fprint(errout, cacheUsage)
		return req, 2
	}
	bySet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "by" {
			bySet = true
		}
	})
	harnessName := strings.TrimSpace(*harnessFlag)
	if harnessName != "" && !knownHarness(harnessName) {
		fmt.Fprintf(errout, "invalid --harness %q (want a harness id such as claude-code, codex or dsh)\n", harnessName)
		return req, 2
	}
	if len(positional) == 1 {
		if bySet {
			fmt.Fprintln(errout, "cache: --by cannot be combined with a session timeline")
			return req, 2
		}
		// A session timeline reads one session's steps through SessionCache,
		// which takes no Filter: there is nothing for an identity filter to
		// narrow, so accepting one silently would falsely imply it applied.
		if flags := providerFilterFlags(&cf); flags != "" {
			fmt.Fprintf(errout, "cache: %s cannot be combined with a session timeline\n", flags)
			return req, 2
		}
		// --instance-id is different: a timeline can read one instance's copy of
		// the session (SessionCacheIn), but it cannot intersect several copies,
		// so more than one id is a usage error rather than a silent union.
		if len(cf.instanceIDs) > 1 {
			fmt.Fprintln(errout, "cache: a session timeline reads one instance; pass at most one --instance-id")
			return req, 2
		}
		req.session = positional[0]
		req.harness = harnessName
	} else {
		if harnessName != "" {
			fmt.Fprintln(errout, "cache: --harness is only valid with a session id")
			return req, 2
		}
		if _, ok := cacheDims[*by]; !ok && *by != "cause" && *by != "day" {
			fmt.Fprintln(errout, "invalid --by value:", *by, "(want cause, provider, model, harness, project, session or day)")
			return req, 2
		}
		req.by = *by
	}
	opts, e := cf.resolve(time.Now())
	if e != nil {
		fmt.Fprintln(errout, e)
		return req, 2
	}
	req.opts = opts
	return req, 0
}

// knownHarness reports whether name is a harness id the index can hold.
func knownHarness(name string) bool {
	if model.Harness(name).Known() {
		return true
	}
	for _, p := range harness.All() {
		if string(p.Harness()) == name {
			return true
		}
	}
	return false
}

// runCache is `mytoken cache`: the cache ledger over a filter, or one session's
// cache timeline when a session id is given.
func runCache(ctx context.Context, args []string, out, errout io.Writer) int {
	req, code := parseCache(args, errout)
	if code != 0 {
		return code
	}
	return runInLocation(req.opts.loc, func() int {
		// The ledger is read from stored cache_expected/cache_miss/miss_cost
		// columns, so no price refresh is needed: cache is always local, and
		// --offline is accepted but redundant (like relay list).
		a, e := app.OpenLocal()
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		defer a.Close()
		env := cacheEnv{
			rng:         req.opts.rng,
			format:      req.opts.format,
			noCost:      req.opts.noCost,
			harness:     req.harness,
			providers:   req.opts.providers,
			providerIDs: req.opts.providerIDs,
			instanceIDs: req.opts.instanceIDs,
			service:     a.Query,
		}
		return cacheRun(ctx, env, req.by, req.session, out, errout)
	})
}

// cacheRun dispatches one cache view. It is separate from runCache so tests can
// drive it with a fake cacheService.
func cacheRun(ctx context.Context, env cacheEnv, by, session string, out, errout io.Writer) int {
	if env.service == nil {
		fmt.Fprintln(errout, "cache: no query service")
		return 1
	}
	if session != "" {
		return cacheTimeline(ctx, env, session, out, errout)
	}
	f := query.Filter{Range: env.rng, Providers: env.providers, ProviderIDs: env.providerIDs, InstanceIDs: env.instanceIDs}
	var payload any
	var err error
	switch by {
	case "cause":
		var totals query.CacheTotals
		if totals, err = env.service.CacheTotals(ctx, f); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if totals.ByCause == nil {
			totals.ByCause = []query.CacheCause{}
		}
		payload = cacheTotalsReport{By: by, Totals: totals}
	case "day":
		var points []query.CachePoint
		if points, err = env.service.CacheDaily(ctx, f); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if points == nil {
			points = []query.CachePoint{}
		}
		payload = cacheRowsReport{By: by, Rows: points}
	default:
		var buckets []query.CacheBucket
		if buckets, err = env.service.CacheBy(ctx, f, cacheDims[by]); err != nil {
			fmt.Fprintln(errout, err)
			return 1
		}
		if buckets == nil {
			buckets = []query.CacheBucket{}
		}
		payload = cacheRowsReport{By: by, Rows: buckets}
	}
	if err = writeCacheView(out, env, by, payload); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	return 0
}

// cacheTimeline prints one session's per-request cache verdicts. With no
// --harness the owning harness is resolved from the ledger's session buckets:
// session ids are only unique inside a harness.
func cacheTimeline(ctx context.Context, env cacheEnv, session string, out, errout io.Writer) int {
	h := model.Harness(env.harness)
	if h == "" {
		buckets, e := env.service.CacheBy(ctx, query.Filter{InstanceIDs: env.instanceIDs}, query.CacheBySession)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		suffix := "/" + session
		var found []string
		for _, b := range buckets {
			if strings.HasSuffix(b.Key, suffix) {
				found = append(found, strings.TrimSuffix(b.Key, suffix))
			}
		}
		switch len(found) {
		case 0:
			fmt.Fprintf(errout, "cache: no session %q in the cache ledger; pass --harness <name> to read it by harness\n", session)
			return 1
		case 1:
			h = model.Harness(found[0])
		default:
			fmt.Fprintf(errout, "cache: session %q appears under %d harnesses; pass --harness to pick one:\n", session, len(found))
			for _, name := range found {
				fmt.Fprintf(errout, "  %s\n", name)
			}
			return 1
		}
	}
	var steps []query.CacheStep
	var e error
	if len(env.instanceIDs) == 1 {
		scoped, ok := env.service.(instanceCacheService)
		if !ok {
			fmt.Fprintf(errout, "cache: %T does not support instance-aware cache lookups\n", env.service)
			return 1
		}
		steps, e = scoped.SessionCacheIn(ctx, h, env.instanceIDs[0], session)
	} else {
		steps, e = env.service.SessionCache(ctx, h, session)
	}
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	if steps == nil {
		steps = []query.CacheStep{}
	}
	payload := cacheTimelineReport{Harness: h, SessionID: session, Steps: steps}
	if e := writeCacheView(out, env, "timeline", payload); e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	return 0
}
