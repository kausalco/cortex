package ruler

import (
	native_ctx "context"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	gklog "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	config_util "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	sd_config "github.com/prometheus/prometheus/discovery/config"
	"github.com/prometheus/prometheus/discovery/dns"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/util/strutil"
	"golang.org/x/net/context"
	"golang.org/x/net/context/ctxhttp"

	"github.com/weaveworks/common/user"
	"github.com/weaveworks/cortex/pkg/chunk"
	"github.com/weaveworks/cortex/pkg/distributor"
	"github.com/weaveworks/cortex/pkg/querier"
	"github.com/weaveworks/cortex/pkg/util"
)

var (
	evalDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "group_evaluation_duration_seconds",
		Help:      "The duration for a rule group to execute.",
	})
	rulesProcessed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "cortex",
		Name:      "rules_processed_total",
		Help:      "How many rules have been processed.",
	})
	blockedWorkers = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "cortex",
		Name:      "blocked_workers",
		Help:      "How many workers are waiting on an item to be ready.",
	})
	evalLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "group_evaluation_latency_seconds",
		Help:      "How far behind the target time each rule group executed.",
		Buckets:   []float64{.1, .25, .5, 1, 2.5, 5, 10, 25},
	})
)

func init() {
	prometheus.MustRegister(evalDuration)
	prometheus.MustRegister(evalLatency)
	prometheus.MustRegister(rulesProcessed)
	prometheus.MustRegister(blockedWorkers)
}

// Config is the configuration for the recording rules server.
type Config struct {
	// This is used for template expansion in alerts; must be a valid URL
	ExternalURL util.URLValue

	// How frequently to evaluate rules by default.
	EvaluationInterval time.Duration
	NumWorkers         int

	// URL of the Alertmanager to send notifications to.
	AlertmanagerURL util.URLValue
	// Whether to use DNS SRV records to discover alertmanagers.
	AlertmanagerDiscovery bool
	// How long to wait between refreshing the list of alertmanagers based on
	// DNS service discovery.
	AlertmanagerRefreshInterval time.Duration

	// Capacity of the queue for notifications to be sent to the Alertmanager.
	NotificationQueueCapacity int
	// HTTP timeout duration when sending notifications to the Alertmanager.
	NotificationTimeout time.Duration
	// Timeout for rule group evaluation, including sending result to ingester
	GroupTimeout time.Duration
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	cfg.ExternalURL.URL, _ = url.Parse("") // Must be non-nil
	f.Var(&cfg.ExternalURL, "ruler.external.url", "URL of alerts return path.")
	f.DurationVar(&cfg.EvaluationInterval, "ruler.evaluation-interval", 15*time.Second, "How frequently to evaluate rules")
	f.IntVar(&cfg.NumWorkers, "ruler.num-workers", 1, "Number of rule evaluator worker routines in this process")
	f.Var(&cfg.AlertmanagerURL, "ruler.alertmanager-url", "URL of the Alertmanager to send notifications to.")
	f.BoolVar(&cfg.AlertmanagerDiscovery, "ruler.alertmanager-discovery", false, "Use DNS SRV records to discover alertmanager hosts.")
	f.DurationVar(&cfg.AlertmanagerRefreshInterval, "ruler.alertmanager-refresh-interval", 1*time.Minute, "How long to wait between refreshing alertmanager hosts.")
	f.IntVar(&cfg.NotificationQueueCapacity, "ruler.notification-queue-capacity", 10000, "Capacity of the queue for notifications to be sent to the Alertmanager.")
	f.DurationVar(&cfg.NotificationTimeout, "ruler.notification-timeout", 10*time.Second, "HTTP timeout duration when sending notifications to the Alertmanager.")
	f.DurationVar(&cfg.GroupTimeout, "ruler.group-timeout", 10*time.Second, "Timeout for rule group evaluation, including sending result to ingester")
}

// Ruler evaluates rules.
type Ruler struct {
	engine        *promql.Engine
	pusher        Pusher
	alertURL      *url.URL
	notifierCfg   *config.Config
	queueCapacity int
	groupTimeout  time.Duration

	// Per-user notifiers with separate queues.
	notifiersMtx sync.Mutex
	notifiers    map[string]*rulerNotifier
}

// rulerNotifier bundles a notifer.Notifier together with an associated
// Alertmanager service discovery manager and handles the lifecycle
// of both actors.
type rulerNotifier struct {
	notifier  *notifier.Notifier
	sdCtx     context.Context
	sdCancel  context.CancelFunc
	sdManager *discovery.Manager
	wg        sync.WaitGroup
	logger    gklog.Logger
}

func newRulerNotifier(o *notifier.Options, l gklog.Logger) *rulerNotifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &rulerNotifier{
		notifier:  notifier.New(o, l),
		sdCtx:     ctx,
		sdCancel:  cancel,
		sdManager: discovery.NewManager(l),
		logger:    l,
	}
}

func (rn *rulerNotifier) run() {
	rn.wg.Add(2)
	go func() {
		if err := rn.sdManager.Run(rn.sdCtx); err != nil {
			level.Error(rn.logger).Log("msg", "error starting notifier discovery manager", "err", err)
		}
		rn.wg.Done()
	}()
	go func() {
		rn.notifier.Run(rn.sdManager.SyncCh())
		rn.wg.Done()
	}()
}

func (rn *rulerNotifier) applyConfig(cfg *config.Config) error {
	if err := rn.notifier.ApplyConfig(cfg); err != nil {
		return err
	}

	sdCfgs := make(map[string]sd_config.ServiceDiscoveryConfig)
	for _, v := range cfg.AlertingConfig.AlertmanagerConfigs {
		// AlertmanagerConfigs doesn't hold an unique identifier so we use the config hash as the identifier.
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		// This hash needs to be identical to the one computed in the notifier in
		// https://github.com/prometheus/prometheus/blob/719c579f7b917b384c3d629752dea026513317dc/notifier/notifier.go#L265
		// This kind of sucks, but it's done in Prometheus in main.go in the same way.
		sdCfgs[fmt.Sprintf("%x", md5.Sum(b))] = v.ServiceDiscoveryConfig
	}
	return rn.sdManager.ApplyConfig(sdCfgs)
}

func (rn *rulerNotifier) stop() {
	rn.sdCancel()
	rn.notifier.Stop()
	rn.wg.Wait()
}

// NewRuler creates a new ruler from a distributor and chunk store.
func NewRuler(cfg Config, d *distributor.Distributor, c *chunk.Store) (*Ruler, error) {
	ncfg, err := buildNotifierConfig(&cfg)
	if err != nil {
		return nil, err
	}
	return &Ruler{
		engine:        querier.NewEngine(d, c),
		pusher:        d,
		alertURL:      cfg.ExternalURL.URL,
		notifierCfg:   ncfg,
		queueCapacity: cfg.NotificationQueueCapacity,
		notifiers:     map[string]*rulerNotifier{},
		groupTimeout:  cfg.GroupTimeout,
	}, nil
}

// Builds a Prometheus config.Config from a ruler.Config with just the required
// options to configure notifications to Alertmanager.
func buildNotifierConfig(rulerConfig *Config) (*config.Config, error) {
	if rulerConfig.AlertmanagerURL.URL == nil {
		return &config.Config{}, nil
	}

	u := rulerConfig.AlertmanagerURL
	var sdConfig sd_config.ServiceDiscoveryConfig
	if rulerConfig.AlertmanagerDiscovery {
		if !strings.Contains(u.Host, "_tcp.") {
			return nil, fmt.Errorf("When alertmanager-discovery is on, host name must be of the form _portname._tcp.service.fqdn (is %q)", u.Host)
		}
		dnsSDConfig := dns.SDConfig{
			Names:           []string{u.Host},
			RefreshInterval: model.Duration(rulerConfig.AlertmanagerRefreshInterval),
			Type:            "SRV",
			Port:            0, // Ignored, because of SRV.
		}
		sdConfig = sd_config.ServiceDiscoveryConfig{
			DNSSDConfigs: []*dns.SDConfig{&dnsSDConfig},
		}
	} else {
		sdConfig = sd_config.ServiceDiscoveryConfig{
			StaticConfigs: []*targetgroup.Group{
				{
					Targets: []model.LabelSet{
						{
							model.AddressLabel: model.LabelValue(u.Host),
						},
					},
				},
			},
		}
	}
	amConfig := &config.AlertmanagerConfig{
		Scheme:                 u.Scheme,
		PathPrefix:             u.Path,
		Timeout:                rulerConfig.NotificationTimeout,
		ServiceDiscoveryConfig: sdConfig,
	}

	promConfig := &config.Config{
		AlertingConfig: config.AlertingConfig{
			AlertmanagerConfigs: []*config.AlertmanagerConfig{amConfig},
		},
	}

	if u.User != nil {
		amConfig.HTTPClientConfig = config_util.HTTPClientConfig{
			BasicAuth: &config_util.BasicAuth{
				Username: u.User.Username(),
			},
		}

		if password, isSet := u.User.Password(); isSet {
			amConfig.HTTPClientConfig.BasicAuth.Password = config_util.Secret(password)
		}
	}

	return promConfig, nil
}

func (r *Ruler) newGroup(ctx context.Context, rs []rules.Rule) (*rules.Group, error) {
	appendable := &appendableAppender{pusher: r.pusher, ctx: ctx}
	userID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}
	notifier, err := r.getOrCreateNotifier(userID)
	if err != nil {
		return nil, err
	}
	opts := &rules.ManagerOptions{
		Appendable:  appendable,
		QueryFunc:   rules.EngineQueryFunc(r.engine),
		Context:     ctx,
		ExternalURL: r.alertURL,
		NotifyFunc:  sendAlerts(notifier, r.alertURL.String()),
		Logger:      gklog.NewNopLogger(),
		Registerer:  prometheus.DefaultRegisterer,
	}
	delay := 0 * time.Second // Unused, so 0 value is fine.
	return rules.NewGroup("default", "none", delay, rs, opts), nil
}

// sendAlerts implements a rules.NotifyFunc for a Notifier.
// It filters any non-firing alerts from the input.
//
// Copied from Prometheus's main.go.
func sendAlerts(n *notifier.Notifier, externalURL string) rules.NotifyFunc {
	return func(ctx native_ctx.Context, expr string, alerts ...*rules.Alert) error {
		var res []*notifier.Alert

		for _, alert := range alerts {
			// Only send actually firing alerts.
			if alert.State == rules.StatePending {
				continue
			}
			a := &notifier.Alert{
				StartsAt:     alert.FiredAt,
				Labels:       alert.Labels,
				Annotations:  alert.Annotations,
				GeneratorURL: externalURL + strutil.TableLinkForExpression(expr),
			}
			if !alert.ResolvedAt.IsZero() {
				a.EndsAt = alert.ResolvedAt
			}
			res = append(res, a)
		}

		if len(alerts) > 0 {
			n.Send(res...)
		}
		return nil
	}
}

func (r *Ruler) getOrCreateNotifier(userID string) (*notifier.Notifier, error) {
	r.notifiersMtx.Lock()
	defer r.notifiersMtx.Unlock()

	n, ok := r.notifiers[userID]
	if ok {
		return n.notifier, nil
	}

	n = newRulerNotifier(&notifier.Options{
		QueueCapacity: r.queueCapacity,
		Do: func(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
			// Note: The passed-in context comes from the Prometheus rule group code
			// and does *not* contain the userID. So it needs to be added to the context
			// here before using the context to inject the userID into the HTTP request.
			ctx = user.InjectOrgID(ctx, userID)
			if err := user.InjectOrgIDIntoHTTPRequest(ctx, req); err != nil {
				return nil, err
			}
			return ctxhttp.Do(ctx, client, req)
		},
	}, util.Logger)

	go n.run()

	// This should never fail, unless there's a programming mistake.
	if err := n.applyConfig(r.notifierCfg); err != nil {
		return nil, err
	}

	// TODO: Remove notifiers for stale users. Right now this is a slow leak.
	r.notifiers[userID] = n
	return n.notifier, nil
}

// Evaluate a list of rules in the given context.
func (r *Ruler) Evaluate(ctx context.Context, rs []rules.Rule) {
	logger := util.WithContext(ctx, util.Logger)
	level.Debug(logger).Log("msg", "evaluating rules...", "num_rules", len(rs))
	start := time.Now()
	ctx, cancelTimeout := context.WithTimeout(ctx, r.groupTimeout)
	g, err := r.newGroup(ctx, rs)
	if err != nil {
		level.Error(logger).Log("msg", "failed to create rule group", "err", err)
		return
	}
	g.Eval(ctx, start)
	if err := ctx.Err(); err == nil {
		cancelTimeout() // release resources
	} else {
		level.Warn(util.Logger).Log("msg", "context error", "error", err)
	}

	// The prometheus routines we're calling have their own instrumentation
	// but, a) it's rule-based, not group-based, b) it's a summary, not a
	// histogram, so we can't reliably aggregate.
	evalDuration.Observe(time.Since(start).Seconds())
	rulesProcessed.Add(float64(len(rs)))
}

// Stop stops the Ruler.
func (r *Ruler) Stop() {
	r.notifiersMtx.Lock()
	defer r.notifiersMtx.Unlock()

	for _, n := range r.notifiers {
		n.stop()
	}
}

// Server is a rules server.
type Server struct {
	scheduler *scheduler
	workers   []worker
}

// NewServer makes a new rule processing server.
func NewServer(cfg Config, ruler *Ruler, rulesAPI RulesAPI) (*Server, error) {
	// TODO: Separate configuration for polling interval.
	s := newScheduler(rulesAPI, cfg.EvaluationInterval, cfg.EvaluationInterval)
	if cfg.NumWorkers <= 0 {
		return nil, fmt.Errorf("must have at least 1 worker, got %d", cfg.NumWorkers)
	}
	workers := make([]worker, cfg.NumWorkers)
	for i := 0; i < cfg.NumWorkers; i++ {
		workers[i] = newWorker(&s, ruler)
	}
	srv := Server{
		scheduler: &s,
		workers:   workers,
	}
	go srv.run()
	return &srv, nil
}

// Run the server.
func (s *Server) run() {
	go s.scheduler.Run()
	for _, w := range s.workers {
		go w.Run()
	}
	level.Info(util.Logger).Log("msg", "ruler up and running")
}

// Stop the server.
func (s *Server) Stop() {
	for _, w := range s.workers {
		w.Stop()
	}
	s.scheduler.Stop()
}

// Worker does a thing until it's told to stop.
type Worker interface {
	Run()
	Stop()
}

type worker struct {
	scheduler *scheduler
	ruler     *Ruler

	done       chan struct{}
	terminated chan struct{}
}

func newWorker(scheduler *scheduler, ruler *Ruler) worker {
	return worker{
		scheduler:  scheduler,
		ruler:      ruler,
		done:       make(chan struct{}),
		terminated: make(chan struct{}),
	}
}

func (w *worker) Run() {
	defer close(w.terminated)
	for {
		select {
		case <-w.done:
			return
		default:
		}
		blockedWorkers.Inc()
		level.Debug(util.Logger).Log("msg", "waiting for next work item")
		item := w.scheduler.nextWorkItem()
		blockedWorkers.Dec()
		if item == nil {
			level.Debug(util.Logger).Log("msg", "queue closed and empty; terminating worker")
			return
		}
		evalLatency.Observe(time.Since(item.scheduled).Seconds())
		level.Debug(util.Logger).Log("msg", "processing item", "item", item)
		ctx := user.InjectOrgID(context.Background(), item.userID)
		w.ruler.Evaluate(ctx, item.rules)
		w.scheduler.workItemDone(*item)
		level.Debug(util.Logger).Log("msg", "item handed back to queue", "item", item)
	}
}

func (w *worker) Stop() {
	close(w.done)
	<-w.terminated
}
